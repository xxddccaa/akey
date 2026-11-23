// Browser smoke test for the akey web panel (headless Chrome via playwright-core).
//
// Run from anywhere (runs `make wasm` and builds the server unless AKEY_BIN is set):
//
//   npm i --prefix /tmp/pw playwright-core          # once
//   node internal/server/webtest/smoke.mjs
//
// Env: AKEY_BIN=/path/to/akey (prebuilt, must embed the panel), CHROME=/usr/bin/google-chrome,
//      PLAYWRIGHT_CORE=/tmp/pw/node_modules/playwright-core, SHOTS=/tmp (screenshot dir, akey-panel-*.png),
//      KEEP=1 keeps the temp dir (server data, CLI HOME).
//
// Flow: start `akey serve` on a temp data dir + a fake OpenAI-compatible /v1/models
// upstream -> unlock with a new passphrase -> create (admin token from the data dir)
// -> add a provider, "拉取模型" from the fake upstream, edit the model, set it as
// codex's default -> save -> CLI `akey pull --dry-run` sees the model -> reload,
// unlock again, model is there -> second save, history shows 2 versions, load the
// old one -> lock -> a config pushed with the CLI opens in the panel. Fails on any console error, page error or CSP violation, or if
// the fake key appears in plaintext in the data dir. Uses fake keys only and a
// temporary HOME for the CLI.
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, readdirSync, statSync, mkdirSync, rmSync } from 'node:fs';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, '../../..');
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_CORE || '/tmp/pw/node_modules/playwright-core');
const SHOTS = process.env.SHOTS || '/tmp';
const PASS = 'smoke test passphrase 2026';
const FAKE_KEY = 'sk-test-smoke-0123456789abcdef';
const work = mkdtempSync(join(tmpdir(), 'akey-smoke-'));
const cleanup = [];
let failed = false;

function log(...a) { console.log('[smoke]', ...a); }
function assert(cond, msg) {
  if (!cond) throw new Error('assertion failed: ' + msg);
}

// ---------- build ----------
function goEnv(extra) {
  return { ...process.env, GOTOOLCHAIN: 'local', ...extra };
}
let bin = process.env.AKEY_BIN;
if (!bin) {
  log('building akey.wasm (make wasm) + akey');
  execFileSync('make', ['wasm'], { cwd: repo, env: goEnv(), stdio: 'inherit' });
  bin = join(work, 'akey');
  execFileSync('go', ['build', '-o', bin, './cmd/akey'], { cwd: repo, env: goEnv({ CGO_ENABLED: '0' }), stdio: 'inherit' });
}

// ---------- fake upstream ----------
const upstream = createServer((req, res) => {
  if (req.url !== '/v1/models') { res.writeHead(404); res.end('nope'); return; }
  if (req.headers.authorization !== 'Bearer ' + FAKE_KEY) { res.writeHead(401); res.end('bad key'); return; }
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify({
    object: 'list',
    data: [
      { id: 'acme-coder-1', object: 'model', name: 'Acme Coder 1', context_length: 200000 },
      { id: 'acme-chat-2', object: 'model' },
      { id: 'acme-embed-3', object: 'model' },
    ],
  }));
});
await new Promise((r) => upstream.listen(0, '127.0.0.1', r));
cleanup.push(() => upstream.close());
const upstreamURL = `http://127.0.0.1:${upstream.address().port}/v1`;

// ---------- akey serve ----------
const data = join(work, 'data');
const port = 18790 + Math.floor(Math.random() * 200);
const base = `https://127.0.0.1:${port}`;
const noProxy = { ...process.env };
for (const k of ['http_proxy', 'https_proxy', 'HTTP_PROXY', 'HTTPS_PROXY', 'all_proxy', 'ALL_PROXY']) delete noProxy[k];
const srv = spawn(bin, ['serve', '--listen', `127.0.0.1:${port}`, '--data', data, '--host', '127.0.0.1'], { env: noProxy, stdio: ['ignore', 'pipe', 'pipe'] });
let srvOut = '';
srv.stdout.on('data', (d) => { srvOut += d; });
srv.stderr.on('data', (d) => { srvOut += d; });
cleanup.push(() => srv.kill());
for (let i = 0; i < 100 && !srvOut.includes('listening'); i++) await new Promise((r) => setTimeout(r, 100));
assert(srvOut.includes('listening'), 'server did not start:\n' + srvOut);
const admin = readFileSync(join(data, 'admin_token'), 'utf8').trim();
const pin = /tls pin:\s+(\S+)/.exec(srvOut)?.[1];
log('server', base, 'upstream', upstreamURL);

// ---------- browser ----------
const browser = await chromium.launch({
  executablePath: process.env.CHROME || '/usr/bin/google-chrome',
  headless: true,
  args: ['--no-proxy-server', '--no-sandbox'],
});
cleanup.push(() => browser.close());

const problems = [];
function watch(page, label) {
  page.on('console', (m) => {
    // 404 on GET /v1/blob/{id} is the expected "no config for this passphrase" answer.
    if (/status of 404/.test(m.text()) && /\/v1\/blob\/[0-9a-f]{64}$/.test(m.location()?.url || '')) return;
    if (m.type() === 'error' || /Content Security Policy/i.test(m.text())) problems.push(`${label} console.${m.type()}: ${m.text()}`);
  });
  page.on('pageerror', (e) => problems.push(`${label} pageerror: ${e.message}`));
  page.on('requestfailed', (r) => problems.push(`${label} request failed: ${r.url()} ${r.failure()?.errorText}`));
}

async function newPage(opts = {}) {
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 900 }, locale: 'zh-CN', ...opts });
  const page = await ctx.newPage();
  watch(page, opts.colorScheme || 'light');
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => console.error('CSP violation: ' + e.violatedDirective + ' ' + e.blockedURI));
  });
  return page;
}

const shot = async (page, name) => {
  const p = join(SHOTS, `akey-panel-${name}.png`);
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: p, fullPage: true });
  log('screenshot', p);
};
const tab = (page, name) => page.click(`#tabs button[data-tab="${name}"]`);
const toastText = (page) => page.locator('#toasts .toast').last().innerText();

async function unlock(page) {
  await page.goto(base + '/');
  await page.fill('#pass', PASS);
  await page.click('#unlock-btn');
}

try {
  const page = await newPage();

  // 1. new passphrase -> create
  await unlock(page);
  await page.locator('#create-box').waitFor({ state: 'visible', timeout: 60000 });
  await shot(page, 'unlock-new');
  await page.click('#create-start');
  await page.fill('#pass2', PASS);
  await page.fill('#admin', admin);
  await page.click('#create-form button[type=submit]');
  await page.locator('#editor').waitFor({ state: 'visible' });
  await page.locator('#main .card').first().waitFor();
  assert((await page.innerText('#status')).includes('新配置'), 'status shows new config');

  // 2. add provider "acme" pointing at the fake upstream
  await tab(page, 'providers');
  await page.click('text=＋ 添加厂商');
  await page.fill('dialog input', 'acme');
  await page.click('dialog button.primary');
  const card = page.locator('#main .card[data-pid="acme"]');
  await card.waitFor();
  await card.locator('[data-f="name"]').fill('Acme AI');
  await card.locator('[data-f="key"]').fill(FAKE_KEY);
  await card.locator('[data-f="oa.base_url"]').fill(upstreamURL);

  // 3. 拉取模型
  await card.locator('button', { hasText: '拉取模型' }).click();
  await page.click('dialog button:has-text("继续")');
  await page.locator('dialog .probe-item').first().waitFor();
  assert((await page.locator('dialog .probe-item').count()) === 3, 'probe lists 3 models');
  await page.fill('dialog input[type=search]', 'coder');
  await page.click('dialog button:has-text("全选可见")');
  await shot(page, 'probe');
  await page.click('dialog button:has-text("添加所选模型")');
  await page.locator('dialog').waitFor({ state: 'detached' });
  assert((await toastText(page)).includes('已添加 1 个模型'), 'probe added 1 model');
  await shot(page, 'providers');

  // 4. edit the model
  await tab(page, 'models');
  const mcard = page.locator('#main details.card[data-mid="acme-coder-1"]');
  await mcard.waitFor();
  assert(await mcard.evaluate((d) => d.open), 'new model card is open');
  assert((await mcard.locator('[data-f="context_window"]').inputValue()) === '200000', 'context_window from probe');
  for (const e of ['low', 'medium', 'high']) await mcard.locator(`[data-f="efforts:${e}"]`).click();
  await mcard.locator('[data-f="default_effort"]').selectOption('high');
  await mcard.locator('[data-f="auto_compact"]').fill('180000');
  await mcard.locator('[data-f="input:image"]').click();
  await mcard.locator('[data-f="input:text"]').click();
  await shot(page, 'models');

  // 5. make it codex's default
  await tab(page, 'tools');
  const codex = page.locator('#main .card[data-tool="codex"]');
  await codex.locator('[data-f="default_model"]').selectOption('acme-coder-1');
  await codex.locator('[data-f="extra"]').fill('approval_policy: never\nfeatures:\n  multi_agent: true\n');
  await codex.locator('[data-f="install"]').fill('npm i -g @openai/codex');
  await shot(page, 'tools');

  // 6. YAML + preview
  await tab(page, 'yaml');
  const y = await page.locator('textarea.yaml-editor').inputValue();
  assert(y.includes('acme-coder-1') && y.includes('approval_policy: never') && y.includes('context_window: 200000'), 'yaml reflects the forms');
  await page.locator('.validate-out .msg.ok').waitFor();
  await shot(page, 'yaml');
  await tab(page, 'preview');
  await page.locator('#main .file-card').first().waitFor();
  const preview = await page.innerText('#main');
  assert(preview.includes("model = 'acme-coder-1'") && !preview.includes(FAKE_KEY) && preview.includes('sk-t…(redacted)'), 'codex preview, key redacted');
  await shot(page, 'preview');

  // 7. save
  await page.click('#btn-save');
  await page.locator('#toasts .toast.ok').filter({ hasText: '已保存' }).waitFor();
  assert((await page.innerText('#status')).includes('已保存'), 'status saved');
  await tab(page, 'history');
  await page.locator('#main table.list').waitFor();
  assert((await page.locator('#main table.list tbody tr').count()) === 1, '1 version');
  await tab(page, 'install');
  await page.locator('#main .cmd pre').first().waitFor();
  assert((await page.innerText('#main')).includes('--pinnedpubkey'), 'install shows pinned one-liner');
  await shot(page, 'install');

  // 8. ciphertext only on the server
  const blobs = join(data, 'blobs');
  for (const id of readdirSync(blobs)) {
    for (const f of readdirSync(join(blobs, id))) {
      const p = join(blobs, id, f);
      if (statSync(p).isFile()) assert(!readFileSync(p, 'utf8').includes('sk-test-smoke'), 'plaintext key in ' + p);
    }
  }

  // 9. CLI interop: akey pull --dry-run sees what the panel saved
  const home = join(work, 'home');
  mkdirSync(home);
  const cli = execFileSync(bin, ['pull', '--server', base, '--pin', pin, '--dry-run', '--yes', '--only', 'codex'], {
    env: { ...noProxy, HOME: home, AKEY_PASSPHRASE: PASS, AKEY_CLIENT_CONFIG: join(home, 'client.json') },
  }).toString();
  assert(cli.includes('acme-coder-1'), 'CLI pull sees the panel-saved model:\n' + cli);
  log('CLI pull --dry-run OK');

  // 10. reload, unlock, model still there; edit + save again -> 2 versions
  await page.reload();
  await page.fill('#pass', PASS);
  await page.click('#unlock-btn');
  await page.locator('#editor').waitFor({ state: 'visible', timeout: 60000 });

  // YAML parse errors never lose work: the form tabs refuse to open
  await tab(page, 'yaml');
  const ta = page.locator('textarea.yaml-editor');
  const good = await ta.inputValue();
  await ta.fill(good + '\nbogus: [\n');
  await page.locator('.validate-out .msg.err').waitFor();
  await tab(page, 'models');
  assert((await ta.count()) === 1 && (await ta.inputValue()).includes('bogus'), 'stays on YAML tab with the broken text');
  await ta.fill(good);
  await page.locator('.validate-out .msg.ok').waitFor();

  // a broken YAML snippet field blocks saving
  await tab(page, 'tools');
  const ex = page.locator('#main .card[data-tool="codex"] [data-f="extra"]');
  await page.waitForFunction(() => document.querySelector('#main .card[data-tool="codex"] [data-f="extra"]')?.value.includes('approval_policy'));
  const exGood = await ex.inputValue();
  await ex.fill('a: [1');
  await page.locator('#main .card[data-tool="codex"] .field-error').filter({ hasText: /\S/ }).waitFor();
  await page.click('#btn-save');
  await page.locator('dialog').filter({ hasText: '有字段格式错误' }).waitFor();
  await page.click('dialog button.primary');
  await ex.fill(exGood);

  await tab(page, 'models');
  await page.locator('#main details.card[data-mid="acme-coder-1"]').waitFor();
  const m2 = page.locator('#main details.card[data-mid="acme-coder-1"]');
  if (!(await m2.evaluate((d) => d.open))) await m2.locator('summary.card-head').click();
  await m2.locator('[data-f="name"]').fill('Acme Coder One');
  assert((await page.innerText('#status')).includes('未保存'), 'dirty after edit');
  await page.keyboard.press('Control+s');
  await page.locator('#toasts .toast.ok').filter({ hasText: '已保存' }).waitFor();
  await tab(page, 'history');
  await page.locator('#main table.list').waitFor();
  assert((await page.locator('#main table.list tbody tr').count()) === 2, '2 versions');
  await shot(page, 'history');
  await page.locator('#main table.list tbody tr').nth(1).locator('button').click();
  await page.locator('textarea.yaml-editor').waitFor();
  const old = await page.locator('textarea.yaml-editor').inputValue();
  assert(old.includes('name: Acme Coder 1') && !old.includes('Acme Coder One'), 'old version loaded');
  assert((await page.innerText('#status')).includes('历史版本'), 'status shows loaded version');

  // 11. lock
  page.once('dialog', (d) => d.accept());
  await page.click('#btn-lock');
  await page.click('dialog button.danger');
  await page.locator('#unlock').waitFor({ state: 'visible' });

  // 12. dark mode + phone width screenshots (existing config)
  const dark = await newPage({ colorScheme: 'dark' });
  await unlock(dark);
  await dark.locator('#editor').waitFor({ state: 'visible', timeout: 60000 });
  await tab(dark, 'providers');
  await dark.locator('#main .card').first().waitFor();
  await shot(dark, 'dark-providers');
  const phone = await newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  await unlock(phone);
  await phone.locator('#editor').waitFor({ state: 'visible', timeout: 60000 });
  await tab(phone, 'models');
  await phone.locator('#main details.card').first().waitFor();
  await shot(phone, 'phone-models');
  const overflow = await phone.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  assert(overflow <= 1, `no horizontal scroll on phone width (overflow ${overflow}px)`);

  // 13. wrong admin token is reported (new passphrase)
  const p3 = await newPage();
  await p3.goto(base + '/');
  await p3.fill('#pass', 'another passphrase 2026');
  await p3.click('#unlock-btn');
  await p3.locator('#create-box').waitFor({ state: 'visible', timeout: 60000 });
  await p3.click('#create-start');
  await p3.fill('#pass2', 'mismatch');
  await p3.fill('#admin', 'x');
  await p3.click('#create-form button[type=submit]');
  assert((await p3.innerText('#create-msg')).includes('不一致'), 'passphrase mismatch detected');

  // 14. reverse interop: a config pushed by the CLI opens in the panel
  const home2 = join(work, 'home2');
  mkdirSync(home2);
  execFileSync(bin, ['push', join(repo, 'testdata/master.example.yaml'), '--server', base, '--pin', pin], {
    env: { ...noProxy, HOME: home2, AKEY_PASSPHRASE: 'cli pushed passphrase 2026', AKEY_ADMIN_TOKEN: admin, AKEY_CLIENT_CONFIG: join(home2, 'client.json') },
  });
  const p4 = await newPage();
  await p4.goto(base + '/');
  await p4.fill('#pass', 'cli pushed passphrase 2026');
  await p4.click('#unlock-btn');
  await p4.locator('#editor').waitFor({ state: 'visible', timeout: 60000 });
  await tab(p4, 'providers');
  await p4.locator('#main .card[data-pid="hybrid"]').waitFor();
  await tab(p4, 'yaml');
  assert((await p4.locator('textarea.yaml-editor').inputValue()) === readFileSync(join(repo, 'testdata/master.example.yaml'), 'utf8'),
    'panel shows the CLI-pushed YAML byte for byte (comments kept)');
  log('CLI push -> panel open OK');

  const real = problems.filter((p) => !/favicon/.test(p));
  assert(!real.length, 'browser problems:\n' + real.join('\n'));
  log('PASS');
} catch (e) {
  failed = true;
  console.error('[smoke] FAIL:', e.message);
  if (problems.length) console.error('[smoke] browser problems:\n' + problems.join('\n'));
} finally {
  for (const f of cleanup.reverse()) {
    try { await f(); } catch { /* ignore */ }
  }
  if (!process.env.KEEP) rmSync(work, { recursive: true, force: true });
}
process.exit(failed ? 1 : 0);
