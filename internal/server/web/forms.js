// Form tabs: 厂商 / 模型 / 工具. They edit S.doc (the decoded config) in place.
import { h, toast, busy, modal, confirmBox, promptBox, debounce } from './dom.js';
import {
  S, hooks, rpc, TOOLS, EFFORTS, markDirty, docJSON, toYAML, getDraft, setDraft, clearDraft, dropDrafts,
  httpError, probeHeaders, uniqueId, slug, clone,
} from './state.js';

// ---------------------------------------------------------------------------
// shared bits

let compat = null; // last compat() result for S.doc

export async function refreshCompat() {
  const r = await rpc('compat', docJSON());
  compat = r.ok ? r : null;
  return compat;
}

function changed() {
  markDirty('form');
}

function setOrDelete(obj, key, v) {
  if (v === '' || v == null || (Array.isArray(v) && !v.length)) delete obj[key];
  else obj[key] = v;
}

// rebuildable wraps a card builder so the card can redraw itself while
// keeping keyboard focus on the same logical field (data-f).
function rebuildable(build) {
  let el = build();
  const redraw = async ({ compat: c = false } = {}) => {
    if (c) await refreshCompat();
    const f = document.activeElement?.closest?.('[data-f]')?.dataset.f;
    const next = build();
    el.replaceWith(next);
    el = next;
    if (f) next.querySelector(`[data-f="${CSS.escape(f)}"]`)?.focus();
  };
  return { el: () => el, redraw };
}

function field(label, control, { hint, wide, error, cls } = {}) {
  // <label> only around a single form control: a label wrapping chips or
  // several buttons would forward clicks on its text to the first button.
  const single = /^(INPUT|SELECT|TEXTAREA)$/.test(control.tagName);
  return h(single ? 'label' : 'div', { class: 'field' + (wide ? ' wide' : '') + (cls ? ' ' + cls : '') },
    h('span', { class: 'label' }, label), control,
    hint && h('span', { class: 'hint' }, hint),
    error !== undefined && error);
}

function textInput(obj, key, { placeholder, f, mono, onCommit, type = 'text', autocomplete = 'off' } = {}) {
  const el = h('input', {
    type, value: obj[key] ?? '', placeholder, autocomplete, spellcheck: 'false', dataset: { f: f || key },
    class: mono ? 'code' : '',
  });
  el.addEventListener('input', () => {
    setOrDelete(obj, key, el.value);
    changed();
  });
  if (onCommit) el.addEventListener('change', onCommit);
  return el;
}

function numberInput(obj, key, { placeholder, f } = {}) {
  const d = getDraft(obj, key);
  const el = h('input', { type: 'text', inputmode: 'numeric', value: d ? d.text : obj[key] ?? '', placeholder, dataset: { f: f || key } });
  if (d?.error) el.classList.add('invalid');
  el.addEventListener('input', () => {
    const t = el.value.replace(/[_,\s]/g, '');
    if (t === '') {
      delete obj[key];
    } else if (/^\d+$/.test(t) && Number(t) <= Number.MAX_SAFE_INTEGER) {
      obj[key] = Number(t);
    } else {
      el.classList.add('invalid');
      setDraft(obj, key, { text: el.value, error: `「${el.value}」不是正整数` });
      changed();
      return;
    }
    el.classList.remove('invalid');
    clearDraft(obj, key);
    changed();
  });
  return el;
}

function selectInput(options, value, onChange, { f } = {}) {
  const el = h('select', { dataset: { f: f || '' } },
    options.map(([v, label]) => h('option', { value: v, selected: v === (value ?? '') }, label)));
  el.value = value ?? '';
  el.addEventListener('change', () => onChange(el.value));
  return el;
}

function chips(all, selected, onToggle, { f, warnUnknown = true } = {}) {
  const sel = new Set(selected || []);
  const list = [...all];
  if (warnUnknown) for (const s of sel) if (!list.includes(s)) list.push(s);
  return h('div', { class: 'chips', dataset: { f } },
    list.map((v) => h('button', {
      type: 'button',
      class: 'chip' + (sel.has(v) ? ' on' : '') + (all.includes(v) ? '' : ' warn'),
      'aria-pressed': sel.has(v) ? 'true' : 'false',
      title: all.includes(v) ? null : '不在可选范围内',
      dataset: { f: `${f}:${v}` },
      onclick: () => onToggle(v, !sel.has(v)),
    }, v)));
}

function toggleInList(obj, key, v, on, order) {
  let cur = Array.isArray(obj[key]) ? [...obj[key]] : [];
  if (on && !cur.includes(v)) cur.push(v);
  if (!on) cur = cur.filter((x) => x !== v);
  if (order) cur.sort((a, b) => (order.indexOf(a) + 1 || 99) - (order.indexOf(b) + 1 || 99));
  setOrDelete(obj, key, cur);
  changed();
}

function switchInput(checked, onChange, label, f) {
  const input = h('input', { type: 'checkbox', checked, dataset: { f } });
  input.addEventListener('change', () => onChange(input.checked));
  return h('label', { class: 'switch' }, input, label && h('span', null, label));
}

// yamlField edits an arbitrary object (extra / overrides) as YAML text.
// The text the user typed is kept as a draft so errors never lose work.
function yamlField(obj, key, { placeholder, rows = 4, f, validate } = {}) {
  const ta = h('textarea', { class: 'code', rows, placeholder, spellcheck: 'false', dataset: { f: f || key } });
  const err = h('div', { class: 'field-error' });
  const d = getDraft(obj, key);
  if (d) {
    ta.value = d.text;
    err.textContent = d.error || '';
    ta.classList.toggle('invalid', !!d.error);
  } else {
    toYAML(obj[key]).then((t) => { if (!getDraft(obj, key)) ta.value = t; });
  }
  const flush = async () => {
    S.pending.delete(ta);
    const text = ta.value;
    const r = await rpc('fromYAML', text);
    let error = r.ok ? '' : r.error;
    let v = r.ok ? r.json : undefined;
    if (!error && v != null && (typeof v !== 'object' || Array.isArray(v))) error = '这里需要一个映射（key: value）';
    if (!error && validate) error = validate(v) || '';
    if (error) {
      setDraft(obj, key, { text, error });
    } else {
      setOrDelete(obj, key, v && Object.keys(v).length ? v : undefined);
      setDraft(obj, key, { text, error: '' });
    }
    err.textContent = error;
    ta.classList.toggle('invalid', !!error);
  };
  const later = debounce(flush, 450);
  ta.addEventListener('input', () => {
    setDraft(obj, key, { text: ta.value, error: getDraft(obj, key)?.error || '' });
    S.pending.set(ta, flush);
    changed();
    later();
  });
  return { ta, err };
}

const KEY_HINT = '在浏览器里加密后才上传；pull 时写进各工具的配置文件（权限 0600）';

// ---------------------------------------------------------------------------
// 厂商

const endpointStash = new WeakMap(); // provider -> {openai, anthropic} when toggled off

function modelsOf(pid) {
  return S.doc.models.filter((m) => m && m.provider === pid);
}

function renameProvider(oldId, newId) {
  const out = {};
  for (const [k, v] of Object.entries(S.doc.providers)) out[k === oldId ? newId : k] = v;
  S.doc.providers = out;
  for (const m of S.doc.models) if (m && m.provider === oldId) m.provider = newId;
}

const ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

function providerCard(pid) {
  const p = S.doc.providers[pid] || (S.doc.providers[pid] = {});
  let curId = pid; // updated on rename
  const self = rebuildable(() => {
    const nModels = modelsOf(curId).length;
    const idEl = h('input', { class: 'id-input', value: curId, spellcheck: 'false', 'aria-label': '厂商 ID', dataset: { f: 'id' } });
    idEl.addEventListener('change', () => {
      const v = idEl.value.trim();
      if (v === curId) return;
      if (!v || S.doc.providers[v]) {
        toast(!v ? '厂商 ID 不能为空' : `厂商 ID「${v}」已存在`, 'err');
        idEl.value = curId;
        return;
      }
      if (!ID_RE.test(v)) toast('建议 ID 只用字母、数字、点、下划线和连字符：它会原样出现在各工具的配置里。', 'warn');
      renameProvider(curId, v);
      curId = v;
      changed();
      self.redraw();
    });

    const keyEl = h('input', { type: 'password', value: p.key ?? '', autocomplete: 'off', spellcheck: 'false', placeholder: 'sk-…', dataset: { f: 'key' } });
    keyEl.addEventListener('input', () => { setOrDelete(p, 'key', keyEl.value); changed(); });
    const eye = h('button', { type: 'button', class: 'sm', 'aria-label': '显示或隐藏 Key', onclick: () => {
      keyEl.type = keyEl.type === 'password' ? 'text' : 'password';
      eye.textContent = keyEl.type === 'password' ? '显示' : '隐藏';
    } }, '显示');

    const stash = endpointStash.get(p) || {};
    const toggleEndpoint = (kind, on, dflt) => {
      if (on) p[kind] = stash[kind] || p[kind] || dflt;
      else {
        stash[kind] = p[kind];
        endpointStash.set(p, stash);
        delete p[kind];
      }
      // keep key order: name, key, openai, anthropic, overrides
      const order = ['name', 'key', 'openai', 'anthropic', 'overrides'];
      const copy = { ...p };
      for (const k of Object.keys(p)) delete p[k];
      for (const k of order) if (copy[k] !== undefined) p[k] = copy[k];
      for (const k of Object.keys(copy)) if (!order.includes(k)) p[k] = copy[k];
      changed();
      self.redraw({ compat: true });
    };

    const oa = p.openai;
    const an = p.anthropic;
    const oaBox = h('div', { class: 'endpoint wide' + (oa ? '' : ' off') },
      h('div', { class: 'row' },
        switchInput(!!oa, (on) => toggleEndpoint('openai', on, { base_url: '', wire: 'responses' }), 'OpenAI 协议', 'oa'),
        h('span', { class: 'hint' }, 'codex / xdcode 只能用 responses；opencode / pi 两种都行')),
      oa && h('div', { class: 'grid' },
        field('base_url', textInput(oa, 'base_url', { placeholder: 'https://api.example.com/v1', f: 'oa.base_url' }), { hint: '通常以 /v1 结尾' }),
        field('wire', selectInput([['responses', 'responses（默认）'], ['chat', 'chat']], oa.wire || 'responses', (v) => {
          oa.wire = v;
          changed();
          self.redraw({ compat: true });
        }, { f: 'oa.wire' }))));
    const anBox = h('div', { class: 'endpoint wide' + (an ? '' : ' off') },
      h('div', { class: 'row' },
        switchInput(!!an, (on) => toggleEndpoint('anthropic', on, { base_url: '' }), 'Anthropic 协议', 'an'),
        h('span', { class: 'hint' }, 'claude 必须用这个；opencode / pi 也可以用')),
      an && h('div', { class: 'grid' },
        field('base_url', textInput(an, 'base_url', { placeholder: 'https://api.example.com', f: 'an.base_url' }), { hint: '与 ANTHROPIC_BASE_URL 相同，不带 /v1' }),
        field('auth', selectInput([['api_key', 'api_key（x-api-key，默认）'], ['bearer', 'bearer']], an.auth || 'api_key', (v) => {
          an.auth = v;
          changed();
        }, { f: 'an.auth' }))));

    const ov = yamlField(p, 'overrides', {
      placeholder: 'xdcode:\n  supports_standalone_web_search: false\ncodex:\n  stream_idle_timeout_ms: 600000',
      f: 'overrides',
      validate: validateOverrides,
    });

    const badges = [];
    if (oa) badges.push(h('span', { class: 'badge accent' }, 'OpenAI · ' + (oa.wire || 'responses')));
    if (an) badges.push(h('span', { class: 'badge accent' }, 'Anthropic'));
    if (!oa && !an) badges.push(h('span', { class: 'badge danger' }, '缺少接入点'));
    badges.push(h('span', { class: 'badge' }, nModels + ' 个模型'));

    return h('div', { class: 'card', dataset: { pid: curId } },
      h('div', { class: 'card-head' }, idEl, badges, h('span', { class: 'spacer' }),
        h('button', { type: 'button', class: 'sm', onclick: () => probe(curId), dataset: { f: 'probe' } }, '拉取模型'),
        h('button', { type: 'button', class: 'sm danger', onclick: () => deleteProvider(curId) }, '删除')),
      h('div', { class: 'card-body' },
        h('div', { class: 'grid' },
          field('显示名', textInput(p, 'name', { placeholder: curId, f: 'name' })),
          field('API Key', h('div', { class: 'inline-input' }, keyEl, eye), { hint: KEY_HINT, cls: 'span2' }),
          oaBox, anBox,
          h('details', { class: 'subtle-details wide', open: !!(p.overrides || getDraft(p, 'overrides')) },
            h('summary', null, '高级：overrides（按工具名写原生键，深度合并进该厂商生成的配置块）'),
            field('', ov.ta, { error: ov.err })))));
  });
  return self.el();
}

function validateOverrides(v) {
  if (v == null) return '';
  for (const [t, val] of Object.entries(v)) {
    if (!TOOLS.includes(t)) return `未知工具「${t}」（可用：${TOOLS.join(', ')}）`;
    if (val == null || typeof val !== 'object' || Array.isArray(val)) return `${t} 下面需要是映射（key: value）`;
  }
  return '';
}

async function deleteProvider(pid) {
  const refs = modelsOf(pid);
  const msg = refs.length
    ? h('div', null, h('p', null, `删除厂商「${pid}」会同时删除引用它的 ${refs.length} 个模型：`),
      h('p', null, h('code', null, refs.map((m) => m.id).join(', '))))
    : `删除厂商「${pid}」？`;
  if (!(await confirmBox('删除厂商', msg, { ok: '删除', danger: true }))) return;
  dropDrafts(S.doc.providers[pid]);
  delete S.doc.providers[pid];
  for (const m of refs) removeModel(m);
  changed();
  hooks.rerender();
}

export async function renderProviders() {
  const ids = Object.keys(S.doc.providers);
  const add = async () => {
    const id = await promptBox('添加厂商', {
      label: '厂商 ID',
      hint: '例如 openai、relay、deepseek。只用字母、数字、点、下划线和连字符；它会出现在各工具的配置里。',
      value: uniqueId('provider', new Set(Object.keys(S.doc.providers))),
      ok: '添加',
    });
    if (id == null) return;
    const v = id.trim();
    if (!v) return toast('厂商 ID 不能为空', 'err');
    if (S.doc.providers[v]) return toast(`厂商 ID「${v}」已存在`, 'err');
    S.doc.providers[v] = { name: '', key: '', openai: { base_url: '', wire: 'responses' } };
    delete S.doc.providers[v].name;
    changed();
    await hooks.rerender();
    const cards = document.querySelectorAll('#main .card');
    const last = cards[cards.length - 1];
    last?.scrollIntoView({ block: 'center' });
    last?.querySelector('[data-f="oa.base_url"]')?.focus();
  };
  return h('div', null,
    h('div', { class: 'toolbar' }, h('h2', null, '厂商'), h('span', { class: 'badge' }, ids.length),
      h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'primary', onclick: add }, '＋ 添加厂商')),
    h('p', { class: 'section-intro' }, '每个厂商是一个 API 接入点，可以同时提供 OpenAI 和 Anthropic 两种协议。点「拉取模型」可以从厂商的 /models 接口获取模型列表并加入模型目录。'),
    ids.length ? ids.map(providerCard) : h('div', { class: 'empty' }, '还没有厂商。点右上角「添加厂商」。'));
}

// ---------- 拉取模型 ----------

let probeAcked = false;

async function probe(pid) {
  const p = S.doc.providers[pid];
  if (!p) return;
  const eps = [];
  if (p.openai) eps.push('openai');
  if (p.anthropic) eps.push('anthropic');
  if (!eps.length) return toast('先给这个厂商配置 OpenAI 或 Anthropic 接入点', 'err');
  let api = eps[0];
  if (eps.length > 1) {
    api = await modal({
      title: '用哪个接入点拉取？',
      body: h('p', null, '这个厂商同时提供两种协议。选择要查询模型列表的接入点（添加的模型也会使用这个协议）。'),
      actions: [{ label: '取消', value: undefined }, { label: 'Anthropic', value: 'anthropic' }, { label: 'OpenAI', value: 'openai', primary: true }],
    });
    if (!api) return;
  }
  const ep = p[api];
  if (!ep.base_url) return toast(`先填写 ${api} 的 base_url`, 'err');
  if (!probeAcked) {
    const ok = await confirmBox('拉取模型列表',
      h('div', null,
        h('p', null, '浏览器不能直接访问大多数厂商的 API（跨域限制），所以这一步由 akey 服务器代为请求厂商的模型列表接口。'),
        h('p', null, h('strong', null, '这时 Key 会经过服务器转发：'), '只用于这一次请求，不保存、不写日志。除此之外，配置始终只以密文形式到达服务器。')),
      { ok: '继续' });
    if (!ok) return;
    probeAcked = true;
  }
  const headers = probeHeaders();
  if (!headers) return toast('缺少凭据：请先保存一次配置', 'err');
  const done = busy('正在从厂商拉取模型列表…');
  let r;
  try {
    r = await fetch('/v1/probe', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...headers },
      body: JSON.stringify({ base_url: ep.base_url, api, key: p.key || '', auth: api === 'anthropic' ? (ep.auth || 'api_key') : undefined }),
    });
  } catch (e) {
    done();
    return toast('请求失败：' + e.message, 'err');
  }
  done();
  if (!r.ok) {
    const msg = await httpError(r);
    if (r.status === 502) return toast('厂商接口返回错误（检查 base_url 和 Key）：\n' + msg, 'err');
    if (r.status === 401 || r.status === 403) return toast('服务器拒绝了请求（凭据无效）：' + msg, 'err');
    return toast('拉取失败：' + msg, 'err');
  }
  let data;
  try { data = await r.json(); } catch { return toast('服务器返回了无法解析的结果', 'err'); }
  const models = Array.isArray(data.models) ? data.models : [];
  if (!models.length) return toast('厂商没有返回任何模型', 'warn');
  pickModels(pid, api, models);
}

function pickModels(pid, api, models) {
  const have = new Set(S.doc.models.filter((m) => m && m.provider === pid).map((m) => m.model || m.id));
  const chosen = new Set();
  const filter = h('input', { type: 'search', placeholder: '筛选模型，例如 gpt、claude、coder', autocomplete: 'off', spellcheck: 'false' });
  const count = h('span', { class: 'muted small' });
  const list = h('div', { class: 'probe-list' });
  const items = models.map((m) => {
    const added = have.has(m.id);
    const cb = h('input', { type: 'checkbox', disabled: added, checked: added });
    cb.addEventListener('change', () => {
      if (cb.checked) chosen.add(m);
      else chosen.delete(m);
      upd();
    });
    const meta = [m.name && m.name !== m.id ? m.name : '', m.context_window ? `上下文 ${m.context_window.toLocaleString()}` : '', added ? '已添加' : '']
      .filter(Boolean).join(' · ');
    const row = h('label', { class: 'probe-item' + (added ? ' added' : '') }, cb,
      h('div', null, h('div', { class: 'pid' }, m.id), meta && h('div', { class: 'pmeta' }, meta)));
    return { m, row, cb, added, text: (m.id + ' ' + (m.name || '')).toLowerCase() };
  });
  list.append(...items.map((i) => i.row));
  const visible = () => items.filter((i) => !i.row.hidden && !i.added);
  const upd = () => { count.textContent = `共 ${models.length} 个，已选 ${chosen.size} 个`; };
  filter.addEventListener('input', () => {
    const q = filter.value.trim().toLowerCase();
    for (const i of items) i.row.hidden = q && !i.text.includes(q);
  });
  const all = h('button', { type: 'button', class: 'sm', onclick: () => {
    for (const i of visible()) { i.cb.checked = true; chosen.add(i.m); }
    upd();
  } }, '全选可见');
  const none = h('button', { type: 'button', class: 'sm', onclick: () => {
    for (const i of items) if (!i.added) { i.cb.checked = false; chosen.delete(i.m); }
    upd();
  } }, '全不选');
  upd();
  modal({
    title: `从「${pid}」添加模型`,
    wide: true,
    body: h('div', { class: 'stack' },
      filter, h('div', { class: 'row' }, all, none, h('span', { class: 'spacer' }), count), list,
      h('p', { class: 'hint' }, '添加后到「模型」页补充推理档位、上下文窗口等信息。')),
    actions: [
      { label: '取消', value: false },
      { label: '添加所选模型', value: true, primary: true, onClick: () => {
        if (!chosen.size) { toast('还没有勾选模型', 'warn'); return false; }
        return true;
      } },
    ],
    init: () => filter.focus(),
  }).then(async (ok) => {
    if (!ok) return;
    const taken = new Set(S.doc.models.map((m) => m && m.id));
    const p = S.doc.providers[pid];
    let n = 0;
    for (const m of models.filter((x) => chosen.has(x))) {
      // "acme" + "acme-coder-1" -> "acme-coder-1", not "acme-acme-coder-1".
      const base = slug(m.id);
      const id = uniqueId(base.startsWith(slug(pid) + '-') ? base : slug(pid + '-' + m.id), taken);
      taken.add(id);
      const md = { id, provider: pid, model: m.id };
      if (m.name && m.name !== m.id) md.name = m.name;
      if (p.openai && p.anthropic) md.api = api;
      if (m.context_window > 0) md.context_window = m.context_window;
      S.doc.models.push(md);
      S.open.add(md);
      n++;
    }
    changed();
    hooks.rerender();
    toast(`已添加 ${n} 个模型。到「模型」页补充推理档位等信息，再到「工具」页选默认模型。`, 'ok', 6000);
  });
}

// ---------------------------------------------------------------------------
// 模型

function removeModel(md) {
  S.doc.models = S.doc.models.filter((m) => m !== md);
  dropDrafts(md);
  for (const t of Object.values(S.doc.tools)) {
    if (!t) continue;
    if (Array.isArray(t.models)) setOrDelete(t, 'models', t.models.filter((x) => x !== md.id));
    if (t.default_model === md.id) delete t.default_model;
  }
}

function renameModel(oldId, newId) {
  for (const t of Object.values(S.doc.tools)) {
    if (!t) continue;
    if (t.default_model === oldId) t.default_model = newId;
    if (Array.isArray(t.models)) t.models = t.models.map((x) => (x === oldId ? newId : x));
  }
}

function modelRefs(id) {
  const out = [];
  for (const [name, t] of Object.entries(S.doc.tools)) {
    if (!t) continue;
    if (t.default_model === id) out.push(`${name} 的默认模型`);
    else if (Array.isArray(t.models) && t.models.includes(id)) out.push(`${name} 的模型列表`);
  }
  return out;
}

function providerApis(pid) {
  const p = S.doc.providers[pid];
  const out = [];
  if (p?.openai) out.push('openai');
  if (p?.anthropic) out.push('anthropic');
  return out;
}

function modelCard(md) {
  const self = rebuildable(() => {
    const apis = providerApis(md.provider);
    const usable = compat ? TOOLS.filter((t) => compat.compatible?.[t]?.includes(md.id)) : [];
    const defaults = Object.entries(S.doc.tools).filter(([, t]) => t && t.default_model === md.id).map(([n]) => n);

    const summaryT = h('span', { class: 't' }, md.id || '（未命名）');
    const summaryS = h('span', { class: 's' });
    const refreshSummary = () => {
      summaryT.textContent = md.id || '（未命名）';
      summaryS.textContent = [md.provider || '?', md.model && md.model !== md.id ? md.model : '', md.name || ''].filter(Boolean).join(' · ');
    };
    refreshSummary();

    const idEl = h('input', { value: md.id ?? '', spellcheck: 'false', class: 'code', dataset: { f: 'id' } });
    idEl.addEventListener('change', () => {
      const v = idEl.value.trim();
      const old = md.id;
      if (v === old) return;
      if (!v) { toast('模型 ID 不能为空', 'err'); idEl.value = old ?? ''; return; }
      if (S.doc.models.some((m) => m !== md && m && m.id === v)) { toast(`模型 ID「${v}」已存在`, 'err'); idEl.value = old ?? ''; return; }
      md.id = v;
      if (old) renameModel(old, v);
      changed();
      self.redraw({ compat: true });
    });

    const provOpts = Object.keys(S.doc.providers).map((id) => [id, id]);
    if (!S.doc.providers[md.provider]) provOpts.unshift([md.provider || '', md.provider ? `${md.provider}（不存在）` : '— 选择厂商 —']);
    const provEl = selectInput(provOpts, md.provider, (v) => {
      md.provider = v;
      if (md.api && !providerApis(v).includes(md.api)) delete md.api;
      changed();
      self.redraw({ compat: true });
    }, { f: 'provider' });

    const apiOpts = [['', apis.length > 1 ? '自动（openai）' : apis.length ? `自动（${apis[0]}）` : '自动']];
    for (const a of apis) apiOpts.push([a, a]);
    if (md.api && !apis.includes(md.api)) apiOpts.push([md.api, `${md.api}（厂商没有这个接入点）`]);
    const apiEl = selectInput(apiOpts, md.api, (v) => {
      setOrDelete(md, 'api', v);
      changed();
      self.redraw({ compat: true });
    }, { f: 'api' });

    const effortChips = chips(EFFORTS, md.reasoning_efforts, (v, on) => {
      toggleInList(md, 'reasoning_efforts', v, on, EFFORTS);
      if (!on && md.default_effort === v) delete md.default_effort;
      self.redraw();
    }, { f: 'efforts' });
    const effOpts = [['', '不设置']];
    for (const e of md.reasoning_efforts || []) effOpts.push([e, e]);
    if (md.default_effort && !(md.reasoning_efforts || []).includes(md.default_effort)) effOpts.push([md.default_effort, md.default_effort + '（不在档位列表中）']);
    const effEl = selectInput(effOpts, md.default_effort, (v) => { setOrDelete(md, 'default_effort', v); changed(); }, { f: 'default_effort' });

    const inputChips = chips(['text', 'image'], md.input, (v, on) => { toggleInList(md, 'input', v, on, ['text', 'image']); self.redraw(); }, { f: 'input' });
    const toolChips = chips(TOOLS, md.tools, (v, on) => { toggleInList(md, 'tools', v, on, TOOLS); self.redraw({ compat: true }); }, { f: 'tools' });

    const compatEl = h('input', { value: md.compatibility ?? '', list: 'dl-compat', placeholder: 'responses', spellcheck: 'false', dataset: { f: 'compatibility' } });
    compatEl.addEventListener('input', () => { setOrDelete(md, 'compatibility', compatEl.value.trim()); changed(); });

    const ov = yamlField(md, 'overrides', {
      placeholder: 'xdcode:\n  auto_compact_token_limit: 800000\npi:\n  compat:\n    supportsDeveloperRole: false',
      f: 'overrides',
      validate: validateOverrides,
    });

    const dup = () => {
      const c = clone(md);
      c.id = uniqueId((md.id || 'model') + '-copy', new Set(S.doc.models.map((m) => m && m.id)));
      const i = S.doc.models.indexOf(md);
      S.doc.models.splice(i + 1, 0, c);
      S.open.add(c);
      changed();
      hooks.rerender();
    };
    const del = async () => {
      const refs = modelRefs(md.id);
      const msg = refs.length
        ? h('div', null, h('p', null, `模型「${md.id}」正在被使用：`), h('ul', null, refs.map((r) => h('li', null, r))),
          h('p', null, '删除后这些引用会被清除，相应工具需要重新选择默认模型。'))
        : `删除模型「${md.id}」？`;
      if (!(await confirmBox('删除模型', msg, { ok: '删除', danger: true }))) return;
      removeModel(md);
      changed();
      hooks.rerender();
    };

    const badges = [];
    if (!S.doc.providers[md.provider]) badges.push(h('span', { class: 'badge danger' }, '厂商无效'));
    for (const t of defaults) badges.push(h('span', { class: 'badge ok', title: `${t} 的默认模型` }, '★ ' + t));
    if (compat && !usable.length) badges.push(h('span', { class: 'badge warn' }, '没有工具能用'));

    const det = h('details', { class: 'card', open: S.open.has(md), dataset: { mid: md.id || '' } },
      h('summary', { class: 'card-head' }, h('span', { class: 'chev' }, '▸'), h('span', { class: 'summary-main' }, summaryT, summaryS), badges),
      h('div', { class: 'card-body' },
        h('div', { class: 'grid' },
          field('ID', idEl, { hint: '全文件唯一，工具通过它引用模型' }),
          field('厂商', provEl),
          field('上游模型名 model', textInput(md, 'model', { placeholder: md.id, f: 'model', onCommit: refreshSummary }), { hint: '发给厂商的模型名，默认等于 ID' }),
          field('显示名 name', textInput(md, 'name', { placeholder: md.model || md.id, f: 'name', onCommit: refreshSummary })),
          field('协议 api', apiEl, { hint: apis.length > 1 ? '厂商两种协议都有时默认 openai' : null }),
          field('上下文窗口 context_window', numberInput(md, 'context_window', { placeholder: '例如 272000' })),
          field('自动压缩阈值 auto_compact', numberInput(md, 'auto_compact', { placeholder: '例如 244800' }), { hint: 'codex / xdcode 使用' }),
          field('最大输出 max_output', numberInput(md, 'max_output', { placeholder: '例如 64000' }), { hint: 'opencode / pi 使用' }),
          field('推理档位 reasoning_efforts', effortChips, { wide: true, hint: '模型支持的思考强度，点击切换' }),
          field('默认档位 default_effort', effEl),
          field('输入模态 input', inputChips, { hint: 'opencode / pi 使用' }),
          field('compatibility', compatEl, { hint: '仅 xdcode 使用：responses（默认）或 unverified 等' }),
          field('只给这些工具用 tools', toolChips, { wide: true, hint: '都不选 = 所有协议兼容的工具' }),
          h('details', { class: 'subtle-details wide', open: !!(md.overrides || getDraft(md, 'overrides')) },
            h('summary', null, '高级：overrides（按工具名深度合并进该模型生成的条目）'),
            field('', ov.ta, { error: ov.err }))),
        h('div', { class: 'row card-foot' },
          h('span', { class: 'hint' }, compat ? (usable.length ? '可用于：' + usable.join('、') : '当前没有工具能用这个模型（检查厂商协议和 tools 限制）') : ''),
          h('span', { class: 'spacer' }),
          h('button', { type: 'button', class: 'sm', onclick: dup }, '复制'),
          h('button', { type: 'button', class: 'sm danger', onclick: del }, '删除'))));
    det.addEventListener('toggle', () => { if (det.open) S.open.add(md); else S.open.delete(md); });
    return det;
  });
  return self.el();
}

export async function renderModels() {
  await refreshCompat();
  const q = { v: '' };
  const listEl = h('div');
  const draw = () => {
    const needle = q.v.trim().toLowerCase();
    const ms = S.doc.models.filter((m) => m && (!needle || [m.id, m.name, m.model, m.provider].some((x) => String(x || '').toLowerCase().includes(needle))));
    listEl.replaceChildren(...(ms.length ? ms.map(modelCard) : [h('div', { class: 'empty' }, S.doc.models.length ? '没有匹配的模型' : '还没有模型。到「厂商」页点「拉取模型」，或点「添加模型」手动填写。')]));
  };
  const search = h('input', { type: 'search', placeholder: '搜索模型', autocomplete: 'off', spellcheck: 'false' });
  search.addEventListener('input', () => { q.v = search.value; draw(); });
  const add = () => {
    const pids = Object.keys(S.doc.providers);
    const md = { id: uniqueId('new-model', new Set(S.doc.models.map((m) => m && m.id))), provider: pids[0] || '' };
    S.doc.models.push(md);
    S.open.add(md);
    changed();
    draw();
    setTimeout(() => {
      const cards = listEl.querySelectorAll('details.card');
      const last = cards[cards.length - 1];
      last?.scrollIntoView({ block: 'center' });
      last?.querySelector('[data-f="id"]')?.focus();
    }, 30);
  };
  const expandAll = (open) => {
    for (const m of S.doc.models) if (open) S.open.add(m); else S.open.delete(m);
    for (const d of listEl.querySelectorAll('details.card')) d.open = open;
  };
  draw();
  return h('div', null,
    h('datalist', { id: 'dl-compat' }, h('option', { value: 'responses' }), h('option', { value: 'unverified' })),
    h('div', { class: 'toolbar' }, h('h2', null, '模型'), h('span', { class: 'badge' }, S.doc.models.length), search,
      h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'sm ghost', onclick: () => expandAll(true) }, '全部展开'),
      h('button', { type: 'button', class: 'sm ghost', onclick: () => expandAll(false) }, '全部收起'),
      h('button', { type: 'button', class: 'primary', onclick: add }, '＋ 添加模型')),
    h('p', { class: 'section-intro' }, '模型目录：每个模型属于一个厂商。工具会自动列出所有协议兼容的模型；codex / xdcode 需要 OpenAI responses，claude 需要 Anthropic。'),
    listEl);
}

// ---------------------------------------------------------------------------
// 工具

const TOOL_INFO = {
  codex: { title: 'Codex CLI', dir: '~/.codex/config.toml', need: '需要 OpenAI 协议（responses）的模型', install: 'npm i -g @openai/codex' },
  xdcode: { title: 'xdcode', dir: '~/.xdcode/config.toml', need: '需要 OpenAI 协议（responses）的模型', install: '' },
  claude: { title: 'Claude Code', dir: '~/.claude/settings.json', need: '需要 Anthropic 协议的模型', install: 'npm i -g @anthropic-ai/claude-code' },
  opencode: { title: 'opencode', dir: '~/.config/opencode/opencode.jsonc', need: 'OpenAI（responses / chat）和 Anthropic 协议都可以', install: 'npm i -g opencode-ai' },
  pi: { title: 'pi', dir: '~/.pi/agent/models.json + settings.json', need: 'OpenAI 和 Anthropic 协议都可以', install: 'npm i -g @mariozechner/pi-coding-agent' },
};

const toolStash = new Map(); // tool name -> section removed by toggling off

function toolCard(name) {
  const info = TOOL_INFO[name];
  const self = rebuildable(() => {
    const t = S.doc.tools[name];
    const on = !!t && t.enabled !== false;
    const setEnabled = (v) => {
      if (v) {
        if (!S.doc.tools[name]) S.doc.tools[name] = toolStash.get(name) || {};
        delete S.doc.tools[name].enabled;
        const cat = compat?.[name] || [];
        if (!S.doc.tools[name].default_model && cat.length) S.doc.tools[name].default_model = cat[0];
      } else if (t) {
        const keys = Object.keys(t).filter((k) => k !== 'enabled');
        if (keys.length) t.enabled = false;
        else { toolStash.set(name, t); delete S.doc.tools[name]; }
      }
      changed();
      self.redraw({ compat: true });
    };
    const head = h('div', { class: 'card-head' },
      switchInput(on, setEnabled, null, 'enabled'),
      h('div', { class: 'summary-main' }, h('h3', null, info.title + ' ', h('span', { class: 'muted small' }, name)), h('span', { class: 's' }, info.dir)),
      t && !on && h('span', { class: 'badge warn' }, '已停用（设置保留）'),
      !t && h('span', { class: 'badge' }, '未启用'));
    if (!t) {
      return h('div', { class: 'card disabled', dataset: { tool: name } }, head,
        h('div', { class: 'card-body' }, h('p', { class: 'tool-desc' }, `未启用的工具不会被写入。${info.need}。`)));
    }
    const catalog = compat?.[name] || [];
    const compatible = compat?.compatible?.[name] || [];
    const dmOpts = [['', catalog.length ? '— 选择默认模型 —' : '— 没有可用的模型 —']];
    for (const id of catalog) {
      const md = S.doc.models.find((m) => m && m.id === id);
      dmOpts.push([id, md?.name && md.name !== id ? `${id} · ${md.name}` : id]);
    }
    if (t.default_model && !catalog.includes(t.default_model)) dmOpts.push([t.default_model, `${t.default_model}（不可用）`]);
    const dmEl = selectInput(dmOpts, t.default_model, (v) => { setOrDelete(t, 'default_model', v); changed(); self.redraw(); }, { f: 'default_model' });
    if (on && (!t.default_model || !catalog.includes(t.default_model))) dmEl.classList.add('invalid');

    const dm = S.doc.models.find((m) => m && m.id === t.default_model);
    const effs = dm?.reasoning_efforts?.length ? dm.reasoning_efforts : EFFORTS;
    const effOpts = [['', dm?.default_effort ? `使用模型默认（${dm.default_effort}）` : '使用模型默认']];
    for (const e of effs) effOpts.push([e, e]);
    if (t.default_effort && !effs.includes(t.default_effort)) effOpts.push([t.default_effort, t.default_effort + '（模型不支持）']);
    const effEl = selectInput(effOpts, t.default_effort, (v) => { setOrDelete(t, 'default_effort', v); changed(); }, { f: 'default_effort' });

    const subset = chips(compatible, t.models, (v, onv) => {
      toggleInList(t, 'models', v, onv, compatible);
      self.redraw({ compat: true });
    }, { f: 'models' });

    const ex = yamlField(t, 'extra', {
      rows: 5,
      placeholder: name === 'codex' || name === 'xdcode' ? 'approval_policy: never\nsandbox_mode: danger-full-access' : name === 'claude' ? 'permissions:\n  defaultMode: bypassPermissions' : name === 'opencode' ? 'autoupdate: false' : 'quietStartup: true',
      f: 'extra',
    });

    return h('div', { class: 'card' + (on ? '' : ' disabled'), dataset: { tool: name } }, head,
      h('div', { class: 'card-body' },
        h('p', { class: 'tool-desc' }, info.need + '。'),
        h('div', { class: 'grid cols-3' },
          field('默认模型 default_model', dmEl, { hint: catalog.length ? null : '先在「模型」页添加这个工具能用的模型' }),
          field('默认推理档位 default_effort', effEl),
          field('安装命令 install', textInput(t, 'install', { placeholder: info.install || '例如 npm i -g …', f: 'install' }), { hint: 'akey pull --install-missing 在找不到该工具时执行' }),
          field('模型目录 models', compatible.length ? subset : h('span', { class: 'hint' }, '没有兼容的模型'), { wide: true, hint: '都不选 = 所有兼容的模型（默认模型总会包含）' }),
          field('extra（YAML）', ex.ta, { wide: true, error: ex.err, hint: '用该工具的原生键名，最后深度合并到生成结果之上' }),
          h('div', { class: 'field wide' }, h('span', { class: 'label' }, '附加文件 files'), filesEditor(t, () => self.redraw())))));
  });
  return self.el();
}

function filesEditor(t, redraw) {
  const files = t.files || {};
  const names = Object.keys(files);
  const rebuildMap = (fn) => {
    const out = {};
    for (const [k, v] of Object.entries(t.files || {})) fn(out, k, v);
    setOrDelete(t, 'files', Object.keys(out).length ? out : undefined);
  };
  const rows = names.map((name, i) => {
    const nameEl = h('input', { value: name, class: 'code', placeholder: 'AGENTS.md', spellcheck: 'false', dataset: { f: `file.${i}.name` } });
    nameEl.addEventListener('change', () => {
      const v = nameEl.value.trim();
      if (v === name) return;
      if (!v || (t.files && v in t.files)) { toast(!v ? '文件名不能为空' : `文件「${v}」已存在`, 'err'); nameEl.value = name; return; }
      if (v.startsWith('/') || v.split('/').includes('..')) toast('文件名必须是相对路径，不能包含 ..', 'warn');
      rebuildMap((out, k, val) => { out[k === name ? v : k] = val; });
      changed();
      redraw();
    });
    const ta = h('textarea', { class: 'code', rows: 5, spellcheck: 'false', dataset: { f: `file.${i}.content` } });
    ta.value = files[name] ?? '';
    ta.addEventListener('input', () => { t.files[name] = ta.value; changed(); });
    return h('div', { class: 'file-row' },
      h('div', { class: 'row nowrap' }, nameEl,
        h('button', { type: 'button', class: 'sm danger', onclick: () => {
          rebuildMap((out, k, val) => { if (k !== name) out[k] = val; });
          changed();
          redraw();
        } }, '删除')), ta);
  });
  const add = h('button', { type: 'button', class: 'sm', onclick: () => {
    const n = uniqueId('NEW.md', new Set(Object.keys(t.files || {})));
    t.files = { ...(t.files || {}), [n]: '' };
    changed();
    redraw();
  } }, '＋ 添加文件');
  return h('div', null, rows, h('div', { class: 'row' }, add, h('span', { class: 'hint' }, '写进该工具配置目录的额外文件，例如 codex 的 AGENTS.md、claude 的 CLAUDE.md')));
}

export async function renderTools() {
  await refreshCompat();
  return h('div', null,
    h('div', { class: 'toolbar' }, h('h2', null, '工具')),
    h('p', { class: 'section-intro' }, '只有启用的工具会在 akey pull 时写入配置。默认模型只列出该工具能用的模型。'),
    TOOLS.map(toolCard));
}
