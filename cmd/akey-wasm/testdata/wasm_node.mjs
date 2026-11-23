// Drives akey.wasm under Node (used by wasm_node_test.go, which builds the
// wasm and checks interop with the Go vault package). Manual run:
//
//   GOOS=js GOARCH=wasm go build -o /tmp/akey.wasm ./cmd/akey-wasm
//   node cmd/akey-wasm/testdata/wasm_node.mjs /tmp/akey.wasm "$(go env GOROOT)/misc/wasm/wasm_exec.js" job.json
//
// job.json: {"params":{...}, "passphrase":"...", "envelope":"<sealed by Go>", "yaml":"<to seal>"}
// Prints one JSON object with the results of each call.
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';

const [wasmPath, wasmExec, jobPath] = process.argv.slice(2);
const require = createRequire(import.meta.url);
globalThis.fs ??= require('fs');
require(wasmExec);

const job = JSON.parse(readFileSync(jobPath, 'utf8'));
const go = new Go();
const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject);
go.run(instance); // returns when main blocks in select{}
const akey = globalThis.akey;
if (!akey) throw new Error('akey global not registered');

const out = {};
out.version = akey.version();
out.badArg = akey.derive(42);
out.lockedOpen = akey.open(job.envelope);
let t = Date.now();
out.derive = akey.derive(job.passphrase, JSON.stringify(job.params));
out.deriveMs = Date.now() - t;
out.unlocked = akey.unlocked();
out.open = akey.open(job.envelope);
out.badEnvelope = akey.open('{"v":1,"alg":"xchacha20poly1305","nonce":"AAAA","ct":"AAAA"}');
out.seal = akey.seal(job.yaml);
out.sealInvalid = akey.seal('tools: {codex: {}}\n');
out.validate = akey.validate('models:\n  - id: a\n    provider: nope\n');
out.decode = akey.decode(job.yaml);
out.encode = out.decode.ok ? akey.encode(JSON.stringify(out.decode.json)) : null;
out.template = akey.template();
out.compat = akey.compat(job.yaml);
out.render = akey.render(job.yaml, 'codex');
out.renderMissing = akey.render(job.yaml, 'nosuchtool');
out.lock = akey.lock();
out.afterLock = akey.unlocked();
process.stdout.write(JSON.stringify(out));
process.exit(0);
