# akey protocol

This document describes akey's wire protocol, key derivation, envelope format and server storage. It is intended for auditors and for anyone writing a compatible client. The reference implementation lives in [`internal/vault`](../internal/vault/vault.go) (crypto), [`internal/server`](../internal/server) (HTTP server) and [`internal/client`](../internal/client) (CLI client). The browser panel runs the same `vault`/`spec`/`render` Go code compiled to WebAssembly ([`cmd/akey-wasm`](../cmd/akey-wasm)).

## 1. Overview

```
passphrase ──Argon2id(server salt)──► master ──HKDF──┬─► id           (public; blob address)
                                                     ├─► enc key      (never leaves the client)
                                                     └─► write token  (sent on writes; server stores sha256)

master.yaml ──XChaCha20-Poly1305(enc key, AAD=id)──► envelope ──PUT /v1/blob/{id}──► server
```

- Each passphrase maps to exactly one config, so several configs on one server are simply several passphrases. There are no accounts.
- The server sees only the `id`, the envelope and `sha256(write token)`. It cannot decrypt anything. The one exception is that `POST /v1/probe` receives a provider API key in transit (see §4.7).
- Creating a new `id` requires the server's **admin token**. Updating an existing one requires that id's **write token**.

## 2. Key derivation

Public parameters come from `GET /v1/params`:

```json
{"version":1,"salt":"<base64, 32 bytes>","time":3,"memory_kib":65536,"threads":4}
```

The server generates `salt` randomly once, on first start. Clients **must validate** the parameters before using them, so that a malicious server cannot exhaust client memory or weaken the KDF. The checks are:

- `version` == 1
- `salt` is exactly 32 bytes
- `memory_kib` is in [8192, 1048576]
- `time` is in [1, 10]
- `threads` ≥ 1

Derivation:

```
master      = Argon2id(password = passphrase (UTF-8),
                       salt     = "akey-v1|" || salt,
                       t = time, m = memory_kib, p = threads, keyLen = 32)
id          = hex( HKDF-SHA256(ikm = master, salt = none, info = INFO_ID,    32 bytes) )   // 64 lowercase hex
enc         =      HKDF-SHA256(ikm = master, salt = none, info = INFO_ENC,   32 bytes)     // raw 32-byte key
write_token = hex( HKDF-SHA256(ikm = master, salt = none, info = INFO_WRITE, 32 bytes) )   // 64 lowercase hex
```

The HKDF info strings, as ASCII, are:

| constant | value |
|---|---|
| `INFO_ID` | `akey/id` |
| `INFO_ENC` | `akey/enc` |
| `INFO_WRITE` | `akey/write` |

`TestDeriveKnownAnswer` in `internal/vault/vault_test.go` freezes this derivation. Changing the salt, the parameters or any info string changes every derived value, so all existing configs become unreachable. The server therefore never regenerates `params.json`.

## 3. Envelope

The blob stored on the server is a JSON envelope:

```json
{"v":1,"alg":"xchacha20poly1305","nonce":"<base64 24 bytes>","ct":"<base64 ciphertext||tag>"}
```

- Cipher: XChaCha20-Poly1305 with key `enc` and a fresh random 24-byte nonce for every seal.
- AAD: `"akey-v1|" || id` (the hex id as ASCII). Binding the id means an envelope copied to another id fails to open.
- Plaintext: the master config YAML (UTF-8). Clients validate it with `spec.Parse` before sealing.
- Base64 uses the standard alphabet with padding.
- A failed authentication (wrong passphrase or tampered data) is reported as a generic "decryption failed" error.

## 4. HTTP API

All endpoints are served over HTTPS with the server's self-signed certificate (see §6). Plain HTTP is used only when the server runs with `--insecure-http`, which is meant for trusted LANs only.

**Response conventions**

- Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`.
- Every response except panel assets also carries `Cache-Control: no-store` (see §4.8).
- Errors are `text/plain` messages.
- A rate-limited client gets `429` with a `Retry-After` header (in seconds).

**Formats**

- `id` and write tokens are exactly 64 lowercase hex characters. Anything else returns `400`.
- Versions are decimal `unixnano` timestamps that strictly increase within an id.

| Method & path | Auth | Purpose |
|---|---|---|
| `GET /v1/params` | none | KDF parameters (§2) |
| `GET /v1/info` | none | server version, TLS pin, one-liners |
| `GET /v1/blob/{id}` | none (rate limited) | current envelope |
| `PUT /v1/blob/{id}` | write token (+ admin token on create) | store a new version |
| `GET /v1/blob/{id}/versions` | write token | list stored versions |
| `GET /v1/blob/{id}/versions/{v}` | write token | fetch one version's envelope |
| `POST /v1/probe` | id + write token, or admin token | list a provider's models (panel helper) |
| `GET /i` | none | POSIX sh installer |
| `GET /i.ps1` | none | PowerShell installer |
| `GET /dl/{file}` | none | client binaries and `SHA256SUMS` |
| `GET /` | none | web panel `index.html` |
| `GET /assets/{file}` | none | panel static files |

### 4.1 `GET /v1/info`

```json
{
  "version": "v0.1.0",
  "tls": true,
  "pin": "sha256//<base64>",
  "cert_sha256": "<UPPERCASE HEX>",
  "install": {"sh": "curl -fsSk --pinnedpubkey '…' https://1.2.3.4:8787/i | sh", "ps1": "…"}
}
```

The one-liners are built from the `Host` header the request arrived with. If the header does not match `^[A-Za-z0-9.\-:\[\]]{1,255}$`, the server uses its first configured `--host` instead.

### 4.2 `GET /v1/blob/{id}`

- `200`: the envelope (`application/json`) with `X-Akey-Version: <version>`.
- `404`: no config for this passphrase. Each 404 counts as a failed attempt for rate limiting, which throttles online passphrase guessing.

### 4.3 `PUT /v1/blob/{id}`

Headers:

- `X-Akey-Write-Token: <write token>` is always required.
- `Authorization: Bearer <admin token>` is also required when the id does not exist yet.

The body is the envelope and must be valid JSON of at most 1 MiB.

| status | meaning |
|---|---|
| `204` | stored; `X-Akey-Version` holds the new version |
| `400` | malformed id or token, or the body is not JSON |
| `401` | missing write token, or creating an id without an admin token |
| `403` | wrong write token for an existing id, or wrong admin token |
| `413` | body too large |

`401` and `403` count as failed attempts. On create, the server stores `sha256(write token)`, and every later PUT must present a token with the same hash. The server keeps the newest 10 versions per id.

### 4.4 `GET /v1/blob/{id}/versions`

Requires the header `X-Akey-Write-Token`. Returns the versions newest first:

```json
{"versions":[{"version":1763900000000000000,"size":1234,"current":true},{"version":1763800000000000000,"size":1200,"current":false}]}
```

| status | meaning |
|---|---|
| `401` | missing token |
| `403` | wrong token |
| `404` | unknown id |

`401`, `403` and `404` all count as failed attempts.

### 4.5 `GET /v1/blob/{id}/versions/{v}`

Requires the header `X-Akey-Write-Token`. Returns that version's envelope with `X-Akey-Version: v`. If the id is known but the version is not, the response is `404 version not found`.

To roll back, a client fetches an old version, decrypts it, and PUTs it again. The result is stored as a new version.

### 4.6 `GET /i`, `GET /i.ps1`, `GET /dl/{file}`

The installers are templates rendered with the server URL (from the `Host` header, same check as §4.1), the pin and the certificate hash.

Each installer does the following:

1. Downloads `/dl/akey-<os>-<arch>[.exe]` and `/dl/SHA256SUMS` over the pinned connection.
2. Verifies the SHA-256 of the binary.
3. Installs the binary to `~/.local/bin/akey` (Windows: `%LOCALAPPDATA%\akey\akey.exe`).
4. Runs `akey pull --server URL --pin PIN [args…]`. Extra arguments come from `sh -s -- …`.

`/dl/` serves only these names from `--dist DIR`, and returns `404` for everything else:

```
akey-linux-amd64  akey-linux-arm64  akey-darwin-amd64  akey-darwin-arm64
akey-windows-amd64.exe  akey-windows-arm64.exe  SHA256SUMS
```

`SHA256SUMS` is in `sha256sum` format (`<hex>  <name>`) and lists exactly the six binaries. GitHub releases publish the same seven files plus `install-server.sh`.

### 4.7 `POST /v1/probe`

The browser panel uses this endpoint to list a provider's models, because browsers cannot call most provider APIs directly (CORS).

**Auth**, either of:

- `X-Akey-Id` + `X-Akey-Write-Token` of an existing config
- `Authorization: Bearer <admin token>`, which is needed before the first config exists

Missing credentials return `401`, and wrong ones `403`. Both count as failed attempts.

**Body** (≤ 64 KiB):

```json
{"base_url":"https://api.example.com/v1","api":"openai","key":"sk-…","auth":"api_key"}
```

**Upstream request**, depending on `api`:

- `openai`: `GET {base_url}/models` with `Authorization: Bearer key`.
- `anthropic`: `GET {base_url minus trailing /v1}/v1/models?limit=1000` with `anthropic-version: 2023-06-01`. The key goes in `x-api-key`, or in `Authorization: Bearer` when `auth` is `bearer`.

**Limits and safeguards**

- Only http and https URLs are accepted. Credentials, query strings and fragments in `base_url` are rejected.
- The request times out after 10 s, follows at most 3 redirects and accepts at most 2 MiB of response.
- Auth headers are dropped on cross-host redirects.
- Link-local, multicast and cloud-metadata addresses are refused.

**Response**

```json
{"models":[{"id":"…","name":"…","context_window":200000}]}
```

- Models are sorted by id. `name` and `context_window` appear only when the upstream provides them, such as Anthropic's `display_name` or OpenRouter's `name` and `context_length`.
- If the upstream fails, the response is `502` with the upstream status; the key is never echoed.

The key is used for this single request only. It is not logged and not stored.

### 4.8 Panel: `GET /`, `GET /assets/{file}`

The panel is a set of static files embedded in the binary from `internal/server/web/`: `index.html`, JS, CSS, `akey.wasm` and `wasm_exec.js`.

- `/assets/{file}` serves files from that flat directory only.
- `.wasm` is served as `application/wasm`. Responses are gzip-compressed when the client accepts it.
- `/assets/*` uses `Cache-Control: no-cache` with a content-hash `ETag`, so browsers revalidate on every load and never mix files from two releases.
- If the binary was built without `akey.wasm`, `/` serves a short "panel not built (make wasm)" page instead.

Panel responses also carry `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin` and this Content Security Policy:

```
default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self'; connect-src 'self';
style-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
```

Inside the browser:

- The WASM module runs in a Web Worker and keeps the `enc` key inside WASM memory. JavaScript only ever sees `id` and the write token, which it needs for HTTP requests.
- Nothing is written to `localStorage` or other browser storage.

## 5. Rate limiting

Failed attempts are counted per client IP. IPv6 addresses are grouped by `/64`.

A failed attempt is any of the following:

- `404` on `GET /v1/blob/{id}`
- `401` or `403` on any authenticated endpoint
- `404` for an unknown id on the version endpoints

After **10 failed attempts within 10 minutes**, the IP is blocked for **30 minutes**. While blocked, every rate-limited endpoint returns `429` with `Retry-After`.

Because of this, online guessing of passphrases is slow. Offline guessing against a stolen data dir is bounded only by Argon2id and the passphrase's entropy.

## 6. TLS and pins

On first start the server generates a self-signed ECDSA P-256 certificate, valid for 10 years. Its SANs are the `--host` values plus `127.0.0.1` and `localhost`. The certificate is **never regenerated** automatically, since that would break every pin. Deleting `tls/` forces a new key and therefore a new pin.

Two pin formats are printed:

| used by | format |
|---|---|
| curl, the `akey` client, `client.json` | `sha256//` + base64(SHA-256(SubjectPublicKeyInfo DER)), i.e. curl's `--pinnedpubkey` format |
| Windows PowerShell installer, browser fingerprint check | `cert_sha256` = uppercase hex SHA-256 of the certificate DER |

When a pin is configured, the `akey` client skips CA verification but fails closed unless the leaf public key matches the pin. With `https://` and no pin, it uses normal system CA verification.

The client stores `{"server": "...", "pin": "..."}` in `client.json` under the OS user config dir (`~/.config/akey/`, `~/Library/Application Support/akey/` or `%AppData%\akey\`).

## 7. Server data directory

The default is `/var/lib/akey`. Directories are created `0700` and files `0600`, and all writes are atomic (temp file, fsync, rename).

```
params.json                     public KDF params (§2), created once
admin_token                     hex admin token, created once (shown on first start and by `akey info`)
tls/cert.pem                    self-signed certificate (§6)
tls/key.pem                     its private key
blobs/<id>/current              latest envelope
blobs/<id>/v-<unixnano>         version history, newest 10 kept
blobs/<id>/write_token.sha256   hex(sha256(write token))
```

Backing up this directory backs up every config. It contains no plaintext, but `admin_token` and `tls/key.pem` are secrets of the server itself.

## 8. Threat model summary

| attacker | can | cannot |
|---|---|---|
| network MITM (pinned clients) | block traffic | read or alter anything; pin mismatch aborts |
| reads the server disk / backup | see ids, sizes, version times; brute-force passphrases offline | decrypt without guessing the passphrase |
| controls the VPS | serve a malicious panel, installer or binary to capture the passphrase next time it is typed; read keys passed through `/v1/probe` | decrypt existing envelopes without the passphrase |
| knows the passphrase + server address | read and modify that config | touch other configs or create new ones (needs admin token) |
