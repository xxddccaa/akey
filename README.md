# akey

[![ci](https://github.com/xxddccaa/akey/actions/workflows/ci.yml/badge.svg)](https://github.com/xxddccaa/akey/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/xxddccaa/akey)](https://github.com/xxddccaa/akey/releases)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

一份主配置，一条命令配好 **codex / xdcode / claude / opencode / pi** 五个编程 Agent。

主配置端到端加密后存放在你自己的 VPS 上（只要 `IP:端口`，不需要域名），服务器上只有密文。在任意一台新机器上执行一行命令并输入口令，就会装好 `akey`，并把各工具的原生配置文件全部写好。

[English](#english)

## 特性

- **一份 `master.yaml`**：统一描述厂商（provider）、模型和工具，渲染成每个工具自己的配置格式，包括 Responses / Chat / Anthropic 协议差异、思考强度映射和上下文窗口。
- **端到端加密**：口令在本地经 Argon2id 派生密钥，用 XChaCha20-Poly1305 加密。服务器和偷走磁盘的人都看不到 Key。
- **自托管，不需要域名**：使用自签证书加公钥 PIN，安装脚本、二进制下载和所有 API 请求都会校验 PIN。
- **VPS 一键安装**：一条命令完成下载校验、systemd 服务和防火墙配置。
- **Web 面板**：在浏览器里管理厂商、模型和工具，可预览渲染结果，也能回滚历史版本。加解密在浏览器内完成（Go 编译成 WASM），服务器仍然是零知识的。
- **新机器一行命令**：自动下载并校验 `akey`，输入口令后写入全部配置。旧文件会先备份。
- 单个静态二进制，支持 Linux / macOS / Windows 的 amd64 和 arm64。

```
编辑：浏览器面板 / akey edit ── 口令 → 本地加密 ──► 密文 ──► 你的 VPS（akey serve，只存密文）
使用：新机器 curl …/i | sh ── 校验 PIN、SHA256 ──► 安装 akey ── 口令 → 本地解密 ──► 写入 ~/.codex ~/.claude …
```

## 快速开始

1. 在 VPS 上[一键安装服务端](#vps-一键安装)，记下输出里的 **admin token**、面板地址和客户端一键命令。
2. 用浏览器打开[面板](#web-面板)，设置口令并输入 admin token，创建配置，填好厂商、模型和工具，然后保存。
3. 在每台机器上执行[客户端一键命令](#客户端一键安装)，输入口令。

## VPS 一键安装

适用于使用 systemd 的 Linux（amd64/arm64），需要 root 权限：

```sh
curl -fsSL https://raw.githubusercontent.com/xxddccaa/akey/main/scripts/install-server.sh | sudo bash
```

带参数运行：

```sh
curl -fsSL https://raw.githubusercontent.com/xxddccaa/akey/main/scripts/install-server.sh | sudo bash -s -- --host 1.2.3.4 --port 8787
```

| 参数 | 说明 |
|---|---|
| `--host IP[,IP]` | 公网 IP 或主机名，用于证书和打印出的命令。默认沿用已安装服务的设置，没有则自动探测 |
| `--port N` | 监听端口。默认沿用已安装服务的设置，没有则用 `8787` |
| `--version vX.Y.Z` | 安装指定版本，默认安装最新 release |
| `--mirror URL` | GitHub 下载镜像前缀，也可以用环境变量 `AKEY_GITHUB_MIRROR` |
| `--no-start` | 只安装文件，不配置 systemd，并打印 `akey serve` 启动命令 |
| `--uninstall [--purge]` | 卸载服务和二进制，默认保留 `/var/lib/akey`。加 `--purge` 会同时删除所有密文配置、TLS 密钥和 admin token，删除前要求确认（`--yes` 跳过确认） |

脚本会依次完成以下工作：

1. 从 GitHub Releases 下载 6 个平台的二进制，用 `SHA256SUMS` 校验。
2. 把服务端安装到 `/usr/local/bin/akey`，客户端二进制放到 `/usr/local/share/akey/dist`，供 `/dl/` 下载。
3. 写入加固过的 systemd 服务 `akey.service`（DynamicUser，数据目录 `/var/lib/akey`）。
4. 如果启用了 ufw 或 firewalld，自动放行端口。如果 iptables 里有拦截规则，会提示你手动放行。
5. 打印面板地址、TLS PIN、admin token 和客户端一键命令。

- **云厂商安全组**：需要在控制台手动放行 TCP `8787`（或你指定的端口），脚本改不了安全组。
- **升级**：重新运行同一条命令即可。`/var/lib/akey` 不受影响，证书和 PIN 也保持不变，已经发出去的一键命令继续有效。
- **再次查看 admin token 和一键命令**：`sudo akey info --data /var/lib/akey --host 1.2.3.4 --listen :8787`。安装脚本最后也会打印这条命令。运行日志用 `journalctl -u akey -f` 查看。
- **国内 VPS**：如果访问 GitHub 很慢，可以给脚本 URL 和 `--mirror` 加上 GitHub 加速镜像前缀（下例中的 `https://gh-mirror.example.com` 换成你信任的镜像）：

  ```sh
  curl -fsSL https://gh-mirror.example.com/https://raw.githubusercontent.com/xxddccaa/akey/main/scripts/install-server.sh \
    | sudo bash -s -- --mirror https://gh-mirror.example.com/
  ```

  只要能连上 github.com，脚本就会把镜像下载的 `SHA256SUMS` 和 GitHub 上的原件比对，不一致时拒绝安装；连不上时只能信任镜像。另外，脚本本身也经过镜像，所以只用你信任的镜像。

不想用脚本时，也可以手动部署：`make dist`，然后参考 [`deploy/akey.service`](deploy/akey.service) 运行 `akey serve --listen :8787 --data /var/lib/akey --dist DIR --host 1.2.3.4`。

## Web 面板

1. 打开 `https://1.2.3.4:8787/`。服务器用的是自签证书，浏览器会提示"不安全"。在证书详情里核对 **SHA-256 指纹**，确认它和安装输出（或 `akey info`）里的 `cert sha256` 一致后，再选择继续访问。
   如果想完全排除中间人风险，可以走 SSH 隧道，这样流量只经过 SSH：
   ```sh
   ssh -L 8787:127.0.0.1:8787 root@1.2.3.4
   # 然后在本机浏览器打开 https://127.0.0.1:8787/
   ```
   注意：通过隧道访问时，面板「安装命令」页生成的命令里地址是 `127.0.0.1`。给其他机器用的命令请以 `akey info` 的输出为准。
2. **创建配置**：口令输入两遍，并填写 admin token。以后打开面板只需要口令。口令就是这份配置的唯一凭据。
3. 编辑器有以下标签页：
   - **厂商**：base_url、Key、协议。点击「拉取模型」可以从厂商的 `/models` 接口拉取模型列表，勾选后加入模型目录。
   - **模型**：上下文窗口、思考强度、适用工具。
   - **工具**：启用开关、默认模型、默认思考强度、extra 和附加文件。
   - **原始 YAML**：和表单双向切换。注意，用表单保存会丢掉 YAML 注释。
   - **预览**：各工具渲染出来的文件，Key 已打码。
   - **历史版本**：保留最近 10 个版本。载入旧版本后保存，就完成了回滚。
   - **安装命令**：客户端一键命令，可以直接复制。
4. 页面空闲 15 分钟后自动锁定。口令和密钥不会写入浏览器存储。

> 「拉取模型」是唯一会让 Key 离开浏览器的操作：Key 会经 VPS 转发给厂商，用于请求模型列表，服务器不保存、不记录。如果不希望这样，可以手动填写模型。

## 客户端一键安装

把安装输出、`akey info` 或面板「安装命令」页里的命令原样复制到新机器上执行，然后输入口令：

```sh
# Linux / macOS
curl -fsSk --pinnedpubkey 'sha256//<PIN>' https://1.2.3.4:8787/i | sh

# 只写部分工具（参数会传给 akey pull）
curl -fsSk --pinnedpubkey 'sha256//<PIN>' https://1.2.3.4:8787/i | sh -s -- --only codex,claude
```

`-k` 只是跳过 CA 校验（因为证书是自签的）。`--pinnedpubkey` 仍然会校验服务器公钥，公钥不匹配就会拒绝连接。

```powershell
# Windows PowerShell 5.1+：完整命令以安装输出为准，其中带证书 SHA-256 校验
[Net.ServicePointManager]::SecurityProtocol=…;[Net.ServicePointManager]::ServerCertificateValidationCallback={…};iex (New-Object Net.WebClient).DownloadString('https://1.2.3.4:8787/i.ps1')
```

`akey` 会装到 `~/.local/bin`（Windows 是 `%LOCALAPPDATA%\akey`）。服务器地址和 PIN 保存在 `~/.config/akey/client.json`（macOS 是 `~/Library/Application Support/akey/`，Windows 是 `%AppData%\akey\`），所以以后直接运行 `akey pull` 即可。在非交互环境中，可以用 `AKEY_PASSPHRASE` 环境变量提供口令。

## CLI

| 命令 | 作用 |
|---|---|
| `akey pull [--only codex,claude] [--dry-run] [--install-missing]` | 输入口令，拉取并解密配置，写入各工具。`--dry-run` 只显示 diff（Key 已打码）；`--install-missing` 会按 `tools.<name>.install` 安装缺失的工具 |
| `akey edit [--admin-token T]` | 拉取配置，用 `$EDITOR` 编辑，校验后加密上传。新建配置时需要 admin token |
| `akey push FILE [--admin-token T]` | 加密并上传本地的 `master.yaml` |
| `akey import [-o FILE] [-f]` | 从本机现有的工具配置生成 `master.yaml` 草稿（权限 0600，包含 Key） |
| `akey render FILE --out DIR [--only …]` | 不经过服务器，在本地渲染到 `DIR/<tool>/`，用于检查 |
| `akey serve --data DIR --dist DIR --host IP [--listen :8787]` | 运行服务端 |
| `akey info --data DIR [--host IP] [--listen :PORT]` | 不启动服务，重新打印 PIN、admin token、面板地址和一键命令 |
| `akey version` | 显示版本 |

客户端命令都接受 `--server URL --pin PIN`，默认读取 `client.json`。环境变量：`AKEY_PASSPHRASE`、`AKEY_ADMIN_TOKEN`。

## 主配置格式

完整的带注释示例见 [`testdata/master.example.yaml`](testdata/master.example.yaml)，其中包含 overrides、extra、files 和各工具的映射规则。下面是一个最小示例：

```yaml
version: 1

providers:
  relay:                                   # provider id，会原样出现在各工具的配置里
    key: sk-test-relay
    openai:
      base_url: https://relay.example.com/v1
      wire: responses                      # responses（默认）| chat
  claude-relay:
    key: sk-test-claude
    anthropic:
      base_url: https://claude.example.com # 与 ANTHROPIC_BASE_URL 相同
      auth: bearer                         # api_key（默认）| bearer

models:
  - id: sol
    provider: relay
    model: gpt-6.1-sol
    context_window: 1000000
    reasoning_efforts: [low, medium, high, xhigh]
    default_effort: high
  - id: opus
    provider: claude-relay
    model: claude-opus-5.5
    max_output: 64000

tools:
  codex:    { default_model: sol }
  xdcode:   { default_model: sol }
  claude:   { default_model: opus }
  opencode: { default_model: opus }
  pi:       { default_model: opus, extra: { quietStartup: true } }
```

各工具支持的协议不同：codex 和 xdcode 只能用 OpenAI Responses 协议；claude 只能用 Anthropic 协议；opencode 和 pi 两种都支持。akey 只会把兼容的模型写进每个工具。

## 支持的工具

| 工具 | 写入的文件 | 目录覆盖变量 |
|---|---|---|
| codex | `~/.codex/config.toml` | `CODEX_HOME` |
| xdcode（codex 分支） | `~/.xdcode/config.toml` | `XDCODE_HOME` |
| claude（Claude Code） | `~/.claude/settings.json` | `CLAUDE_CONFIG_DIR` |
| opencode | `~/.config/opencode/opencode.jsonc` | `XDG_CONFIG_HOME` |
| pi | `~/.pi/agent/models.json` + `settings.json` | `PI_CODING_AGENT_DIR` |

- 写入方式是整个文件覆盖：先把旧文件备份为 `<文件>.akey-bak-YYYYMMDD-HHMMSS`（每个文件保留最近 5 份），再原子写入。工具自己的个性化设置请写进主配置的 `tools.<tool>.extra`。
- `tools.<tool>.files` 可以往该工具的配置目录里额外写文件，例如 `AGENTS.md`、`CLAUDE.md`。

## 多套配置

一个口令对应一份配置，没有账号和 profile 的概念。想给另一组机器（比如内网机器）用另一套配置，换一个口令新建即可（需要 admin token）。同一台服务器上可以存放任意多份配置，彼此独立。

## 安全模型

- **密钥派生**：`Argon2id(口令, "akey-v1|" + 服务器随机盐)`，参数为 64 MiB、t=3、p=4。派生结果经 HKDF-SHA256 分成三个值：存储 id、加密密钥、写入凭据。加密密钥和明文都不会离开客户端（CLI 进程或浏览器）。
- **加密**：XChaCha20-Poly1305，随机 nonce，AAD 绑定存储 id。
- **服务端只见密文**：服务器只保存密文和写入凭据的 sha256。新建配置需要 admin token，更新配置需要由口令派生的写入凭据。每份配置保留最近 10 个版本。
- **不需要域名也能防中间人**：服务器使用自签证书和公钥 PIN。`curl --pinnedpubkey` 校验公钥，Windows 校验证书 SHA-256；下载的二进制还会用 `SHA256SUMS` 校验。
- **限速**：错误口令（404）、错误凭据和错误 admin token 都按 IP 计数，10 分钟内失败 10 次，该 IP 会被封禁 30 分钟。
- **口令要足够强**：口令是唯一的凭据。服务器一旦被攻破，密文可以被离线暴力破解，Argon2 只能拖慢这个过程。建议使用 4 个以上随机单词或 16 位以上随机字符。
- **信任边界**：面板页面、安装脚本和客户端二进制都由你的 VPS 下发。如果 VPS 被攻破，已有的密文仍然安全，但攻击者可以篡改下发的代码，在你下次输入口令时截获。对此敏感的话，可以从 GitHub Releases 下载 `akey` 并自行校验，然后只用 CLI。

协议和加密细节见 [`docs/protocol.md`](docs/protocol.md)。

## 从源码构建

需要 Go（版本见 `go.mod`）和 make：

```sh
git clone https://github.com/xxddccaa/akey && cd akey
make            # bin/akey（先编译 WASM 面板，再嵌入服务端）
make test       # go test ./...
make check      # gofmt + go vet + test
make dist       # dist/akey-{linux,darwin,windows}-{amd64,arm64}[.exe] + SHA256SUMS
```

Makefile 默认设置 `GOTOOLCHAIN=local`。如果本机的 Go 版本比 `go.mod` 要求的旧，可以用 `make GOTOOLCHAIN=auto`。推送 `v*` tag 后，GitHub Actions 会构建并发布 release。

## License

[MIT](LICENSE)

---

## English

**akey** keeps one master config for five coding agents (**codex, xdcode** (a codex fork)**, claude, opencode, pi**), encrypts it end-to-end with a passphrase and stores it on your own VPS. No domain is needed: the server uses `IP:port` with a self-signed certificate and a pinned public key. On any new machine, one command plus your passphrase installs `akey` and writes every tool's native config. Existing files are backed up first.

**Server (Linux + systemd, as root):**

```sh
curl -fsSL https://raw.githubusercontent.com/xxddccaa/akey/main/scripts/install-server.sh | sudo bash
# options: --host IP --port 8787 --version vX.Y.Z --mirror URL --no-start --uninstall [--purge]
```

Running the installer again upgrades akey and keeps `/var/lib/akey`. `sudo akey info --data /var/lib/akey --host IP --listen :8787` reprints the admin token, pin and install commands. Remember to open the port in your cloud provider's security group.

**Web panel:** open `https://VPS_IP:8787/` and accept the self-signed certificate after checking that its SHA-256 fingerprint matches `cert sha256` from the installer output. To rule out a MITM entirely, tunnel with `ssh -L 8787:127.0.0.1:8787 root@VPS` and open `https://127.0.0.1:8787/`. To create a config, enter a passphrase and the admin token. All encryption runs in the browser (Go compiled to WASM), so the server only ever sees ciphertext. The one exception is "fetch models", which relays a provider key through the server for a single `/models` request; the server does not store or log it.

**Clients:** paste the one-liner printed by the installer or the panel, e.g. `curl -fsSk --pinnedpubkey 'sha256//<PIN>' https://1.2.3.4:8787/i | sh` (append `-s -- --only codex,claude` to limit tools). Windows gets a PowerShell one-liner that pins the certificate hash. After that, run `akey pull` to update and `akey edit` to change the config from the CLI.

**Security:** Argon2id (64 MiB, t=3, p=4) derives the passphrase into a storage id, an XChaCha20-Poly1305 key and a write token via HKDF. The server stores only ciphertext and sha256(write token), keeps 10 versions per config and rate-limits failed attempts. Choose a strong passphrase: it is the only credential, and the panel, installer and binaries are served by your VPS, so that machine still has to be trusted. Each passphrase is a separate config. See [`docs/protocol.md`](docs/protocol.md).

**Build:** `make` (bin/akey with embedded panel), `make test`, `make dist`. MIT licensed.
