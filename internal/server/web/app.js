// akey panel entry point: unlock / create flow, editor shell, save, lock.
import { h, $, toast, busy, modal, confirmBox, promptBox, errorList, fmtVersion } from './dom.js';
import {
  S, hooks, rpc, resetState, workerReady, errorsOf, syncToYaml, syncToDoc,
  hasComments, draftErrors, flushPending, httpError, writeHeaders,
} from './state.js';
import { renderProviders, renderModels, renderTools } from './forms.js';
import { renderYaml, renderPreview, renderHistory, renderInstall } from './views.js';

const IDLE_MS = 15 * 60 * 1000;
const FORM_TABS = ['providers', 'models', 'tools'];
const YAML_TABS = ['yaml', 'preview'];
const RENDER = {
  providers: renderProviders,
  models: renderModels,
  tools: renderTools,
  yaml: renderYaml,
  preview: renderPreview,
  history: renderHistory,
  install: renderInstall,
};

// UI preference only (never secrets): last tab.
const pref = {
  get(k) { try { return localStorage.getItem('akey.ui.' + k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem('akey.ui.' + k, v); } catch { /* ignore */ } },
};

// ---------------------------------------------------------------------------
// Unlock / create

let pendingPass = null; // passphrase kept only until "create" is confirmed or cancelled

function unlockMsg(text, kind = 'err') {
  const el = $('#unlock-msg');
  el.className = 'msg ' + kind;
  el.textContent = text || '';
}

async function onUnlock(e) {
  e.preventDefault();
  const passEl = $('#pass');
  const pass = passEl.value;
  if (!pass) return;
  unlockMsg('');
  $('#create-box').hidden = true;
  $('#create-form').hidden = true;
  $('#unlock-btn').disabled = true;
  let done = busy('正在加载…');
  try {
    const ready = await workerReady;
    if (!ready.ready) throw new Error(ready.error || '面板组件加载失败');
    const pr = await fetch('/v1/params', { cache: 'no-store' });
    if (!pr.ok) throw new Error('获取 KDF 参数失败：' + (await httpError(pr)));
    const params = await pr.text();
    done();
    done = busy('正在派生密钥…（Argon2id，可能需要几秒）');
    const d = await rpc('derive', pass, params);
    if (!d.ok) throw new Error(d.error);
    S.id = d.id;
    S.wt = d.writeToken;
    done();
    done = busy('正在下载配置…');
    const r = await fetch('/v1/blob/' + S.id, { cache: 'no-store' });
    if (r.status === 404) {
      await r.text().catch(() => {}); // finish the request
      done();
      pendingPass = pass;
      passEl.value = '';
      $('#create-box').hidden = false;
      $('#create-start').focus();
      return;
    }
    if (!r.ok) throw new Error(await httpError(r));
    const env = await r.text();
    const ver = r.headers.get('X-Akey-Version');
    const o = await rpc('open', env);
    if (!o.ok) throw new Error(o.error);
    passEl.value = '';
    S.exists = true;
    S.version = ver;
    await enterEditor(o.yaml, false);
  } catch (err) {
    await rpc('lock');
    S.id = S.wt = null;
    unlockMsg(String(err.message || err));
  } finally {
    done();
    $('#unlock-btn').disabled = false;
  }
}

function onCreateStart() {
  $('#create-form').hidden = false;
  $('#create-start').parentElement.hidden = true;
  $('#pass-weak').hidden = !(pendingPass && [...pendingPass].length < 12);
  $('#pass2').focus();
}

async function onCreateCancel() {
  pendingPass = null;
  $('#pass2').value = '';
  $('#admin').value = '';
  $('#create-form').hidden = true;
  $('#create-box').hidden = true;
  $('#create-start').parentElement.hidden = false;
  await rpc('lock');
  S.id = S.wt = null;
  $('#pass').focus();
}

async function onCreate(e) {
  e.preventDefault();
  const msg = $('#create-msg');
  msg.className = 'msg err';
  msg.textContent = '';
  if (!pendingPass || $('#pass2').value !== pendingPass) {
    msg.textContent = '两次输入的口令不一致。';
    $('#pass2').focus();
    return;
  }
  const admin = $('#admin').value.trim();
  if (!admin) {
    msg.textContent = '请填写 admin token（第一次创建配置时需要）。';
    $('#admin').focus();
    return;
  }
  const t = await rpc('template');
  if (!t.ok) {
    msg.textContent = t.error;
    return;
  }
  S.admin = admin;
  S.exists = false;
  pendingPass = null;
  $('#pass2').value = '';
  $('#admin').value = '';
  $('#create-form').hidden = true;
  $('#create-box').hidden = true;
  $('#create-start').parentElement.hidden = false;
  await enterEditor(t.yaml, true);
  toast('已用模板创建新配置。修改厂商、Key 和模型后点「保存」才会写入服务器。', 'info', 7000);
}

// ---------------------------------------------------------------------------
// Editor shell

async function enterEditor(yaml, isNew) {
  S.yaml = yaml;
  S.side = 'yaml';
  S.doc = null;
  S.dirty = isNew;
  unlockMsg('');
  $('#unlock').hidden = true;
  $('#editor').hidden = false;
  touch();
  let tab = pref.get('tab');
  if (!RENDER[tab]) tab = 'providers';
  if (isNew && !FORM_TABS.includes(tab)) tab = 'providers';
  await showTab(tab, { initial: true });
  hooks.status();
}

function setStatus() {
  const el = $('#status');
  if (!el) return;
  el.replaceChildren();
  el.className = 'status';
  let text;
  if (!S.exists) {
    el.classList.add('new');
    text = '新配置 · 尚未保存到服务器';
  } else if (S.dirty) {
    el.classList.add('dirty');
    text = S.loadedFrom ? `已载入历史版本 ${fmtVersion(S.loadedFrom)} · 未保存` : '有未保存的修改';
  } else {
    text = S.version ? `已保存 · 版本 ${fmtVersion(S.version)}` : '已保存';
  }
  el.append(h('span', { class: 'dot' }), h('span', null, text));
  el.title = 'ID ' + (S.id || '').slice(0, 12) + '…';
  document.title = (S.dirty ? '● ' : '') + 'akey 配置面板';
}

function setBanner() {
  const b = $('#banner');
  b.replaceChildren();
  if (FORM_TABS.includes(S.tab) && hasComments(S.yaml) && S.side !== 'form') {
    b.append(h('div', { class: 'msg info' },
      '提示：你的 YAML 里有注释。在表单里做的修改会重新生成整份 YAML，注释和自定义格式会丢失；只想保留注释，请在「YAML」页修改。'));
  }
}

let renderSeq = 0;
async function renderMain({ keepScroll = false } = {}) {
  const my = ++renderSeq;
  const y = window.scrollY;
  let node;
  try {
    node = await RENDER[S.tab]();
  } catch (err) {
    console.error(err);
    node = h('div', { class: 'msg err' }, '渲染出错：' + (err.message || err));
  }
  if (my !== renderSeq) return; // a newer render won
  $('#main').replaceChildren(node);
  setBanner();
  if (keepScroll) window.scrollTo(0, y);
  else window.scrollTo(0, 0);
}

async function showTab(tab, { initial = false } = {}) {
  await flushPending();
  if (FORM_TABS.includes(tab)) {
    const errs = await syncToDoc();
    if (errs) {
      if (!initial && S.tab === 'yaml') {
        toast('YAML 有错误，修好后才能切换到表单：\n' + errs.slice(0, 3).join('\n'), 'err');
        return;
      }
      if (initial) toast('这份 YAML 无法用表单编辑，已打开 YAML 页：\n' + errs[0], 'warn');
      tab = 'yaml';
    }
  }
  if (YAML_TABS.includes(tab)) {
    const de = draftErrors();
    if (de.length) {
      toast('有字段的 YAML 格式错误，先修正：\n' + de.slice(0, 3).join('\n'), 'err');
      return;
    }
    const errs = await syncToYaml();
    if (errs) {
      toast('表单内容无法转换为 YAML：\n' + errs.slice(0, 3).join('\n'), 'err');
      return;
    }
  }
  S.tab = tab;
  pref.set('tab', tab);
  for (const b of document.querySelectorAll('#tabs button')) {
    b.classList.toggle('active', b.dataset.tab === tab);
    b.setAttribute('aria-selected', String(b.dataset.tab === tab));
  }
  await renderMain();
}

hooks.status = setStatus;
hooks.showTab = showTab;
let rerenderP = null;
hooks.rerender = () => {
  // coalesce several calls in the same tick into one render
  if (!rerenderP) {
    rerenderP = Promise.resolve().then(() => {
      rerenderP = null;
      return renderMain({ keepScroll: true });
    });
  }
  return rerenderP;
};

// prepare returns the YAML to validate/save, or null after reporting errors.
async function prepare() {
  await flushPending();
  const de = draftErrors();
  if (de.length) {
    await showErrors('有字段格式错误', de);
    return null;
  }
  const errs = await syncToYaml();
  if (errs) {
    await showErrors('表单内容无法转换为 YAML', errs);
    return null;
  }
  return S.yaml;
}

function showErrors(title, errs, extra) {
  return modal({
    title,
    wide: true,
    body: h('div', null, extra && h('p', null, extra), errorList(errs)),
    actions: [
      ...(S.tab !== 'yaml' ? [{ label: '打开 YAML 页', value: 'yaml' }] : []),
      { label: '知道了', value: true, primary: true },
    ],
  }).then((v) => { if (v === 'yaml') showTab('yaml'); });
}

async function onValidate() {
  const y = await prepare();
  if (y == null) return;
  const r = await rpc('validate', y);
  if (!r.ok) return showErrors('校验失败', errorsOf(r));
  if (r.valid) toast('校验通过 ✓', 'ok');
  else showErrors(`发现 ${r.errors.length} 个问题`, r.errors);
}

let saving = false;
async function onSave() {
  if (saving) return;
  saving = true;
  const btn = $('#btn-save');
  btn.disabled = true;
  try {
    const y = await prepare();
    if (y == null) return;
    const edits = S.edits;
    const v = await rpc('validate', y);
    if (!v.ok || !v.valid) {
      await showErrors('配置有错误，未保存', errorsOf(v.ok ? { errors: v.errors } : v), '修正下面的问题后再保存：');
      return;
    }
    for (;;) {
      const s = await rpc('seal', y);
      if (!s.ok) {
        await showErrors('加密失败', errorsOf(s));
        return;
      }
      const headers = { 'Content-Type': 'application/json', ...writeHeaders() };
      if (!S.exists) headers.Authorization = 'Bearer ' + S.admin;
      const done = busy('正在保存…');
      let r;
      try {
        r = await fetch('/v1/blob/' + S.id, { method: 'PUT', headers, body: s.envelope });
      } finally {
        done();
      }
      if (r.status === 204 || r.ok) {
        S.exists = true;
        S.admin = '';
        S.version = r.headers.get('X-Akey-Version') || S.version;
        S.loadedFrom = null;
        // Edits made while the request was in flight keep the page dirty.
        if (S.edits === edits) S.dirty = false;
        hooks.status();
        toast(`已保存 · 版本 ${fmtVersion(S.version)}\n各台机器运行 akey pull 即可拿到新配置。`, 'ok', 6000);
        if (S.tab === 'history') hooks.rerender();
        return;
      }
      if (!S.exists && (r.status === 401 || r.status === 403)) {
        const tok = await promptBox(r.status === 401 ? '需要 admin token' : 'admin token 不正确', {
          label: 'Admin token',
          type: 'password',
          hint: '服务端横幅里打印的 admin token，或在 VPS 上运行 sudo akey info --data /var/lib/akey 查看。',
          ok: '重试保存',
        });
        if (!tok) return;
        S.admin = tok.trim();
        continue;
      }
      const msg = await httpError(r);
      if (r.status === 403) toast('保存被拒绝：写入令牌不匹配。\n' + msg, 'err');
      else toast('保存失败：' + msg, 'err');
      return;
    }
  } finally {
    saving = false;
    btn.disabled = false;
  }
}

async function lock(reason) {
  await rpc('lock');
  resetState();
  pendingPass = null;
  $('#main').replaceChildren();
  $('#banner').replaceChildren();
  $('#editor').hidden = true;
  $('#unlock').hidden = false;
  for (const d of document.querySelectorAll('dialog')) d.close();
  document.title = 'akey 配置面板';
  unlockMsg(reason || '', reason ? 'info' : 'err');
  $('#pass').focus();
}

async function onLock() {
  if (S.dirty && !(await confirmBox('锁定面板', '有未保存的修改，锁定后会丢失。确定锁定吗？', { ok: '锁定', danger: true }))) return;
  lock('已锁定。');
}

// ---------- idle auto-lock ----------
let lastActive = Date.now();
function touch() { lastActive = Date.now(); }
for (const ev of ['pointerdown', 'keydown', 'input', 'wheel', 'touchstart']) {
  window.addEventListener(ev, touch, { passive: true, capture: true });
}
setInterval(() => {
  if (S.id && Date.now() - lastActive > IDLE_MS) {
    lock('空闲超过 15 分钟，已自动锁定' + (S.dirty ? '（未保存的修改已丢弃）' : '') + '。');
  }
}, 15 * 1000);

window.addEventListener('beforeunload', (e) => {
  if (S.id && S.dirty) {
    e.preventDefault();
    e.returnValue = '';
  }
});

// ---------------------------------------------------------------------------
// Boot

$('#unlock-form').addEventListener('submit', onUnlock);
$('#create-start').addEventListener('click', onCreateStart);
$('#create-cancel').addEventListener('click', onCreateCancel);
$('#create-form').addEventListener('submit', onCreate);
$('#btn-validate').addEventListener('click', onValidate);
$('#btn-save').addEventListener('click', onSave);
$('#btn-lock').addEventListener('click', onLock);
$('#tabs').addEventListener('click', (e) => {
  const b = e.target.closest('button[data-tab]');
  if (b && b.dataset.tab !== S.tab) showTab(b.dataset.tab);
});
window.addEventListener('keydown', (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's' && S.id) {
    e.preventDefault();
    onSave();
  }
});

workerReady.then(async (r) => {
  if (!r.ready) {
    unlockMsg(r.error || '面板组件加载失败');
    return;
  }
  const v = await rpc('version');
  if (v.ok) $('#build-info').textContent = 'akey ' + v.version;
});
