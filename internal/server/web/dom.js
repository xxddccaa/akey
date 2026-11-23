// Small DOM helpers. Everything is built with createElement/textContent:
// no innerHTML with data, no inline handlers (strict CSP).

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  let value;
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'value') value = v;
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'checked' || k === 'disabled' || k === 'selected' || k === 'open' || k === 'hidden' || k === 'readOnly' || k === 'multiple') el[k] = !!v;
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  append(el, children);
  if (value !== undefined) el.value = value;
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c == null || c === false || c === true) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : String(c));
  }
}

export const $ = (sel, root = document) => root.querySelector(sel);

export function debounce(fn, ms) {
  let t;
  return (...a) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...a), ms);
  };
}

// ---------- toast ----------
export function toast(msg, kind = 'info', ms) {
  const box = $('#toasts');
  const el = h('div', { class: 'toast ' + kind, role: kind === 'err' ? 'alert' : 'status' }, msg);
  box.append(el);
  while (box.children.length > 3) box.firstChild.remove();
  const life = ms ?? (kind === 'err' ? 8000 : 4500);
  setTimeout(() => el.remove(), life);
  el.addEventListener('click', () => el.remove());
  return el;
}

// ---------- busy overlay ----------
let busyDepth = 0;
export function busy(text) {
  busyDepth++;
  $('#busy-text').textContent = text || '请稍候…';
  $('#busy').hidden = false;
  let done = false;
  return () => {
    if (done) return;
    done = true;
    if (--busyDepth <= 0) {
      busyDepth = 0;
      $('#busy').hidden = true;
    }
  };
}

// ---------- dialogs ----------
// modal({title, body, actions:[{label, value, primary, danger}], wide})
// resolves with the clicked action's value (or undefined on Esc/close).
// An action's onClick may return false to keep the dialog open.
export function modal({ title, body, actions = [{ label: '好', value: true, primary: true }], wide = false, init }) {
  return new Promise((resolve) => {
    const dlg = h('dialog', { class: wide ? 'wide' : '' });
    let result;
    const btns = actions.map((a) =>
      h('button', {
        type: 'button',
        class: (a.primary ? 'primary' : '') + (a.danger ? ' danger' : ''),
        onclick: async () => {
          if (a.onClick) {
            const r = await a.onClick();
            if (r === false) return;
          }
          result = a.value;
          dlg.close();
        },
      }, a.label));
    dlg.append(h('div', { class: 'dlg' },
      h('div', { class: 'dlg-head' }, h('h2', null, title)),
      h('div', { class: 'dlg-body' }, body),
      h('div', { class: 'dlg-actions' }, btns)));
    dlg.addEventListener('close', () => {
      dlg.remove();
      resolve(result);
    });
    document.body.append(dlg);
    dlg.showModal();
    if (init) init(dlg);
    else {
      const p = btns.find((b) => b.classList.contains('primary'));
      if (p) p.focus();
    }
  });
}

export function confirmBox(title, message, { ok = '确定', cancel = '取消', danger = false } = {}) {
  const body = typeof message === 'string' ? h('p', null, message) : message;
  return modal({
    title,
    body,
    actions: [
      { label: cancel, value: false },
      { label: ok, value: true, primary: !danger, danger },
    ],
  }).then((v) => v === true);
}

// promptBox resolves with the entered string or null.
export function promptBox(title, { label = '', hint = '', type = 'text', value = '', ok = '确定' } = {}) {
  const input = h('input', { type, value, autocomplete: 'off', spellcheck: 'false' });
  const body = h('div', null,
    h('label', { class: 'field' }, h('span', { class: 'label' }, label), input, hint && h('span', { class: 'hint' }, hint)));
  let out = null;
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      out = input.value;
      input.closest('dialog').close();
    }
  });
  return modal({
    title,
    body,
    actions: [
      { label: '取消', value: null },
      { label: ok, value: 'ok', primary: true, onClick: () => { out = input.value; } },
    ],
    init: () => input.focus(),
  }).then((v) => (v === 'ok' || out !== null ? out : null));
}

export function errorList(items) {
  return h('ul', { class: 'err-list' }, items.map((e) => h('li', null, e)));
}

// ---------- clipboard ----------
export async function copyText(text, el) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      toast('已复制', 'ok', 1800);
      return;
    }
  } catch { /* fall through */ }
  // Fallback: select the text so the user can press Ctrl/Cmd+C.
  if (el) {
    const r = document.createRange();
    r.selectNodeContents(el);
    const sel = getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
  }
  toast('浏览器不允许自动复制（需要 HTTPS 安全上下文），已选中文本，请按 Ctrl/⌘+C 复制', 'warn');
}

// ---------- formatting ----------
// v is a unixnano timestamp as a decimal string (too big for a JS number).
export function fmtVersion(v) {
  if (!v || v === '0') return '';
  try {
    const ms = Number(BigInt(v) / 1000000n);
    return new Date(ms).toLocaleString('zh-CN', { hour12: false });
  } catch {
    return String(v);
  }
}

export function fmtSize(n) {
  if (n < 1024) return n + ' B';
  return (n / 1024).toFixed(1) + ' KB';
}
