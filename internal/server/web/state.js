// Shared editor state, the wasm worker RPC and server helpers.
// Nothing here is ever written to localStorage/sessionStorage.

export const TOOLS = ['codex', 'xdcode', 'claude', 'opencode', 'pi'];
export const EFFORTS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'];

export const S = blankState();

function blankState() {
  return {
    id: null, // blob id (hex), derived from the passphrase
    wt: null, // write token (hex)
    admin: '', // admin token, only while creating the first config
    exists: false, // blob exists on the server
    version: null, // X-Akey-Version (unixnano string) of the loaded/saved blob
    loadedFrom: null, // history version loaded into the editor (unsaved)
    yaml: '', // YAML text
    doc: null, // decoded config object (form editing)
    side: null, // 'yaml' | 'form': side with edits not yet synced to the other
    docError: null, // [errors] when the YAML can't be decoded for the forms
    dirty: false,
    edits: 0, // bumped on every edit; save clears dirty only if unchanged meanwhile
    tab: 'providers',
    drafts: new Map(), // obj -> Map(field -> {text, error}) for YAML snippet fields
    pending: new Map(), // element -> async flush fn (snippet fields being edited)
    open: new WeakSet(), // expanded model cards
  };
}

export function resetState() {
  Object.assign(S, blankState());
}

// Hooks filled in by app.js (avoids import cycles).
export const hooks = {
  rerender: () => {},
  status: () => {},
  showTab: async () => {},
};

// ---------- worker RPC ----------
let worker;
let seq = 0;
const pending = new Map();
let readyResolve;
export const workerReady = new Promise((r) => { readyResolve = r; });

function startWorker() {
  worker = new Worker('/assets/worker.js');
  worker.onmessage = (e) => {
    const d = e.data || {};
    if ('ready' in d) {
      readyResolve(d);
      return;
    }
    const cb = pending.get(d.id);
    if (cb) {
      pending.delete(d.id);
      cb(d.result || { ok: false, error: '空结果' });
    }
  };
  worker.onerror = (e) => {
    const msg = '面板组件加载失败：' + (e.message || 'worker error');
    readyResolve({ ready: false, error: msg });
    for (const cb of pending.values()) cb({ ok: false, error: msg });
    pending.clear();
  };
}
startWorker();

// rpc never rejects: it resolves with {ok:true,...} or {ok:false,error}.
export function rpc(fn, ...args) {
  return new Promise((resolve) => {
    const id = ++seq;
    pending.set(id, resolve);
    worker.postMessage({ id, fn, args });
  });
}

export function errorsOf(r) {
  if (!r) return ['未知错误'];
  if (Array.isArray(r.errors) && r.errors.length) return r.errors;
  return [r.error || '未知错误'];
}

// ---------- edit tracking ----------
export function markDirty(side) {
  S.dirty = true;
  S.edits++;
  if (side) S.side = side;
  hooks.status();
}

// syncToYaml makes S.yaml reflect form edits. Returns null on success or a
// list of errors.
export async function syncToYaml() {
  if (S.side !== 'form') return null;
  const r = await rpc('encode', JSON.stringify(S.doc));
  if (!r.ok) return errorsOf(r);
  S.yaml = r.yaml;
  S.side = null;
  return null;
}

// syncToDoc makes S.doc reflect YAML edits. Returns null or errors.
export async function syncToDoc() {
  if (S.side === 'form' || (S.doc && S.side !== 'yaml')) return null;
  const r = await rpc('decode', S.yaml);
  if (!r.ok) {
    S.docError = errorsOf(r);
    return S.docError;
  }
  S.doc = normalizeDoc(r.json);
  S.docError = null;
  S.side = null;
  S.drafts.clear();
  return null;
}

export function normalizeDoc(d) {
  d = d && typeof d === 'object' ? d : {};
  if (!d.version) d.version = 1;
  if (!d.providers || typeof d.providers !== 'object') d.providers = {};
  if (!Array.isArray(d.models)) d.models = [];
  if (!d.tools || typeof d.tools !== 'object') d.tools = {};
  return d;
}

// The JSON the forms edit, for compat()/encode().
export function docJSON() {
  return JSON.stringify(S.doc);
}

export function hasComments(text) {
  return /(^|\s)#/m.test(text || '');
}

// ---------- YAML snippet fields (extra / overrides) ----------
const yamlCache = new Map();
export async function toYAML(value) {
  if (value == null) return '';
  const key = JSON.stringify(value);
  if (yamlCache.has(key)) return yamlCache.get(key);
  const r = await rpc('toYAML', key);
  const text = r.ok ? r.yaml : key;
  if (yamlCache.size > 200) yamlCache.clear();
  yamlCache.set(key, text);
  return text;
}

export function getDraft(obj, field) {
  return S.drafts.get(obj)?.get(field);
}
export function setDraft(obj, field, d) {
  let m = S.drafts.get(obj);
  if (!m) S.drafts.set(obj, (m = new Map()));
  m.set(field, d);
}
export function clearDraft(obj, field) {
  const m = S.drafts.get(obj);
  if (!m) return;
  m.delete(field);
  if (!m.size) S.drafts.delete(obj);
}
export function dropDrafts(obj) {
  S.drafts.delete(obj);
}
export function draftErrors() {
  const out = [];
  for (const [, m] of S.drafts) for (const [field, d] of m) if (d.error) out.push(field + ': ' + d.error);
  return out;
}

// flushPending applies snippet fields that are still being typed into.
export async function flushPending() {
  const fns = [...S.pending.values()];
  S.pending.clear();
  await Promise.all(fns.map((f) => f()));
}

// ---------- server ----------
export async function httpError(resp) {
  if (resp.status === 429) {
    const s = parseInt(resp.headers.get('Retry-After') || '0', 10);
    const mins = Math.floor(s / 60);
    const wait = s > 0 ? (mins ? `${mins} 分 ${s % 60} 秒` : `${s} 秒`) : '一段时间';
    return `请求过于频繁，已被服务器暂时限制，请 ${wait}后再试。`;
  }
  let text = '';
  try { text = (await resp.text()).trim(); } catch { /* ignore */ }
  return `HTTP ${resp.status}${text ? '：' + text.slice(0, 300) : ''}`;
}

export function writeHeaders() {
  return { 'X-Akey-Write-Token': S.wt };
}

// Credentials accepted by /v1/probe: the config's id + write token once it
// exists, otherwise the admin token.
export function probeHeaders() {
  if (S.exists) return { 'X-Akey-Id': S.id, 'X-Akey-Write-Token': S.wt };
  if (S.admin) return { Authorization: 'Bearer ' + S.admin };
  return null;
}

// Version numbers are unixnano (> 2^53): keep them as strings.
export function parseVersions(text) {
  return JSON.parse(text.replace(/"version"\s*:\s*(\d+)/g, '"version":"$1"'));
}

// ---------- misc ----------
export function uniqueId(base, taken) {
  base = base || 'item';
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) if (!taken.has(`${base}-${i}`)) return `${base}-${i}`;
}

export function slug(s) {
  return String(s).toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/-+/g, '-').replace(/^[-.]+|[-.]+$/g, '') || 'model';
}

export function clone(v) {
  return v === undefined ? undefined : JSON.parse(JSON.stringify(v));
}
