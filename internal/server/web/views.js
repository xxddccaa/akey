// Text tabs: YAML / 预览 / 历史 / 安装.
import { h, toast, busy, confirmBox, errorList, debounce, copyText, fmtVersion, fmtSize } from './dom.js';
import { S, hooks, rpc, TOOLS, markDirty, errorsOf, httpError, writeHeaders, parseVersions } from './state.js';

// ---------------------------------------------------------------------------
// YAML

export async function renderYaml() {
  const ta = h('textarea', { class: 'code yaml-editor', spellcheck: 'false', autocomplete: 'off', 'aria-label': 'master.yaml', dataset: { f: 'yaml' } });
  ta.value = S.yaml;
  const out = h('div', { class: 'validate-out' });
  const check = async () => {
    const text = ta.value;
    const r = await rpc('validate', text);
    if (text !== ta.value) return; // stale
    out.replaceChildren();
    if (!r.ok) out.append(h('div', { class: 'msg err' }, r.error));
    else if (r.valid) out.append(h('div', { class: 'msg ok' }, '✓ 校验通过'));
    else out.append(h('div', { class: 'msg err' }, `${r.errors.length} 个问题：`, errorList(r.errors)));
  };
  const later = debounce(check, 350);
  ta.addEventListener('input', () => {
    S.yaml = ta.value;
    markDirty('yaml');
    later();
  });
  // Tab inserts two spaces (press Esc first to move focus out with Tab).
  let escaped = false;
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { escaped = true; return; }
    if (e.key === 'Tab' && !escaped && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      const { selectionStart: a, selectionEnd: b } = ta;
      ta.setRangeText('  ', a, b, 'end');
      ta.dispatchEvent(new Event('input'));
    }
    escaped = false;
  });
  check();
  return h('div', null,
    h('div', { class: 'toolbar' }, h('h2', null, 'YAML'), h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'sm', onclick: () => copyText(ta.value, null) }, '复制全部')),
    h('p', { class: 'section-intro' }, '直接编辑主配置 master.yaml（保存时原样加密，保留注释）。Tab 键插入两个空格，先按 Esc 再按 Tab 可以离开编辑框。'),
    ta, out);
}

// ---------------------------------------------------------------------------
// 预览

let previewTool = null;

export async function renderPreview() {
  const wrap = h('div');
  const files = h('div');
  const v = await rpc('validate', S.yaml);
  const enabled = [];
  const c = await rpc('decode', S.yaml);
  if (c.ok && c.json && c.json.tools) {
    for (const t of TOOLS) {
      const sec = c.json.tools[t];
      if (sec !== undefined && !(sec && sec.enabled === false)) enabled.push(t);
    }
  }
  if (!previewTool || !enabled.includes(previewTool)) previewTool = enabled[0] || TOOLS[0];

  const seg = h('div', { class: 'seg', role: 'tablist' });
  const draw = async () => {
    seg.replaceChildren(...TOOLS.map((t) => h('button', {
      type: 'button',
      class: t === previewTool ? 'on' : '',
      title: enabled.includes(t) ? null : '这个工具没有启用',
      onclick: () => { previewTool = t; draw(); },
    }, t + (enabled.includes(t) ? '' : '（未启用）'))));
    files.replaceChildren(h('div', { class: 'row muted' }, h('div', { class: 'spinner sm' }), '正在渲染…'));
    if (!v.ok || !v.valid) {
      files.replaceChildren(h('div', { class: 'msg err' }, '配置有错误，无法预览：', errorList(errorsOf(v.ok ? { errors: v.errors } : v))));
      return;
    }
    if (!enabled.includes(previewTool)) {
      files.replaceChildren(h('div', { class: 'empty' }, `${previewTool} 没有启用，akey pull 不会写它的配置。到「工具」页启用。`));
      return;
    }
    const r = await rpc('render', S.yaml, previewTool);
    if (!r.ok) {
      files.replaceChildren(h('div', { class: 'msg err' }, r.error));
      return;
    }
    files.replaceChildren(...r.files.map((f) => {
      const pre = h('pre', { class: 'content' }, f.content);
      return h('div', { class: 'card file-card' },
        h('div', { class: 'card-head' }, h('span', { class: 'path' }, f.path), h('span', { class: 'badge' }, f.mode),
          h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'sm', onclick: () => copyText(f.content, pre) }, '复制')),
        pre);
    }));
  };
  wrap.append(
    h('div', { class: 'toolbar' }, h('h2', null, '预览'), seg),
    h('p', { class: 'section-intro' }, '这是 akey pull 会在每台机器上写出的文件（路径相对于该工具的配置目录）。Key 已打码，只显示前几位。'),
    files);
  await draw();
  return wrap;
}

// ---------------------------------------------------------------------------
// 历史

export async function renderHistory() {
  const head = h('div', null,
    h('div', { class: 'toolbar' }, h('h2', null, '历史版本')),
    h('p', { class: 'section-intro' }, '服务器为每个配置保留最近 10 个版本（都是密文）。载入旧版本后点「保存」，就是回滚到那个版本。'));
  if (!S.exists) return h('div', null, head, h('div', { class: 'empty' }, '这个配置还没有保存到服务器，暂无历史版本。'));
  let list;
  try {
    const r = await fetch(`/v1/blob/${S.id}/versions`, { headers: writeHeaders(), cache: 'no-store' });
    if (!r.ok) return h('div', null, head, h('div', { class: 'msg err' }, '获取历史版本失败：' + (await httpError(r))));
    list = parseVersions(await r.text()).versions || [];
  } catch (e) {
    return h('div', null, head, h('div', { class: 'msg err' }, '获取历史版本失败：' + e.message));
  }
  if (!list.length) return h('div', null, head, h('div', { class: 'empty' }, '没有历史版本。'));
  const rows = list.map((v) => h('tr', null,
    h('td', null, fmtVersion(v.version), ' ', v.current && h('span', { class: 'badge ok' }, '当前'),
      S.loadedFrom === v.version && h('span', { class: 'badge warn' }, '已载入')),
    h('td', { class: 'muted' }, fmtSize(v.size)),
    h('td', null, h('button', { type: 'button', class: 'sm', onclick: () => loadVersion(v) }, v.current ? '重新载入' : '载入'))));
  return h('div', null, head,
    h('table', { class: 'list' },
      h('thead', null, h('tr', null, h('th', null, '保存时间'), h('th', null, '大小'), h('th', null, ''))),
      h('tbody', null, rows)));
}

async function loadVersion(v) {
  if (S.dirty && !(await confirmBox('载入历史版本', '编辑器里有未保存的修改，载入后会被替换。继续吗？', { ok: '载入', danger: true }))) return;
  const done = busy('正在载入…');
  let yaml;
  try {
    const r = await fetch(`/v1/blob/${S.id}/versions/${v.version}`, { headers: writeHeaders(), cache: 'no-store' });
    if (!r.ok) throw new Error(await httpError(r));
    const o = await rpc('open', await r.text());
    if (!o.ok) throw new Error(o.error);
    yaml = o.yaml;
  } catch (e) {
    toast('载入失败：' + e.message, 'err');
    return;
  } finally {
    done();
  }
  S.yaml = yaml;
  S.doc = null;
  S.side = 'yaml';
  S.drafts.clear();
  S.loadedFrom = v.current ? null : v.version;
  S.dirty = !v.current;
  if (S.dirty) S.edits++;
  hooks.status();
  await hooks.showTab('yaml');
  toast(v.current ? '已重新载入当前版本' : `已载入 ${fmtVersion(v.version)} 的版本（未保存）。\n点「保存」即可回滚到这个版本。`, 'info', 6000);
}

// ---------------------------------------------------------------------------
// 安装

export async function renderInstall() {
  const head = h('div', null,
    h('div', { class: 'toolbar' }, h('h2', null, '在新机器上安装')),
    h('p', { class: 'section-intro' }, '在要使用 Agent 的电脑上运行下面的命令：下载 akey（校验 SHA-256），然后用口令拉取配置。命令里的地址就是你现在访问面板的地址。'));
  let info;
  try {
    const r = await fetch('/v1/info', { cache: 'no-store' });
    if (!r.ok) throw new Error(await httpError(r));
    info = await r.json();
  } catch (e) {
    return h('div', null, head, h('div', { class: 'msg err' }, '获取安装信息失败：' + e.message));
  }
  const cmd = (title, text, note) => {
    const pre = h('pre', null, text || '');
    return h('div', { class: 'card' },
      h('div', { class: 'card-head' }, h('h3', null, title)),
      h('div', { class: 'card-body stack' },
        h('div', { class: 'cmd' }, pre, h('button', { type: 'button', class: 'primary', onclick: () => copyText(text, pre) }, '复制')),
        note && h('p', { class: 'hint' }, note)));
  };
  const host = location.hostname;
  const local = host === 'localhost' || host === '127.0.0.1' || host === '::1' || host === '[::1]';
  return h('div', null, head,
    local && h('div', { class: 'msg warn' }, '你正通过本机地址访问面板，命令里的地址在其他机器上不可用。请用服务器的公网 IP 或域名打开面板后再复制。'),
    cmd('Linux / macOS', info.install?.sh,
      h('span', null, '只配置部分工具：把结尾的 ', h('code', null, '| sh'), ' 换成 ', h('code', null, '| sh -s -- --only codex,claude'), '。')),
    cmd('Windows PowerShell', info.install?.ps1,
      h('span', null, '只配置部分工具：安装后运行 ', h('code', null, 'akey pull --only codex,claude'), '。以后更新配置只需运行 ', h('code', null, 'akey pull'), '。')),
    h('div', { class: 'card' },
      h('div', { class: 'card-head' }, h('h3', null, '服务器信息')),
      h('div', { class: 'card-body' },
        h('dl', { class: 'kv' },
          h('dt', null, '版本'), h('dd', null, info.version || '—'),
          h('dt', null, '传输'), h('dd', null, info.tls ? 'HTTPS（自签名证书，命令里固定了公钥）' : 'HTTP（无 TLS，仅限可信局域网）'),
          info.tls && [h('dt', null, '公钥 pin'), h('dd', null, info.pin || '—')],
          info.tls && [h('dt', null, '证书 SHA-256'), h('dd', null, info.cert_sha256 || '—')]),
        info.tls && h('p', { class: 'hint' }, '浏览器提示证书不受信任是正常的（自签名）。想确认没有中间人：在浏览器的证书详情里核对 SHA-256 指纹，和服务器启动横幅（或 sudo akey info --data /var/lib/akey）里打印的一致即可。'))));
}
