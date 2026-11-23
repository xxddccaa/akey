// akey panel worker: loads akey.wasm (Go: vault + spec + render) and answers
// RPC calls {id, fn, args} with {id, result}. Running here keeps Argon2id and
// rendering off the page's main thread. The encryption key lives only inside
// the wasm module of this worker.
'use strict';

importScripts('/assets/wasm_exec.js');

const ready = (async () => {
  const go = new Go();
  let inst;
  try {
    inst = await WebAssembly.instantiateStreaming(fetch('/assets/akey.wasm'), go.importObject);
  } catch (e) {
    // Older browsers / wrong MIME type: fall back to an ArrayBuffer.
    const resp = await fetch('/assets/akey.wasm');
    if (!resp.ok) throw new Error('加载 akey.wasm 失败：HTTP ' + resp.status + '（服务端可能没有编译面板，运行 make wasm）');
    inst = await WebAssembly.instantiate(await resp.arrayBuffer(), go.importObject);
  }
  go.run(inst.instance); // returns once main() blocks; the global is set by then
  if (!self.akey) throw new Error('akey.wasm 初始化失败');
  return self.akey;
})();

ready.then(
  () => self.postMessage({ ready: true }),
  (e) => self.postMessage({ ready: false, error: String((e && e.message) || e) }),
);

self.onmessage = async (ev) => {
  const { id, fn, args } = ev.data || {};
  let result;
  try {
    const akey = await ready;
    const f = akey[fn];
    result = typeof f === 'function'
      ? f(...(Array.isArray(args) ? args : []))
      : { ok: false, error: '未知函数 ' + fn };
  } catch (e) {
    result = { ok: false, error: String((e && e.message) || e) };
  }
  self.postMessage({ id, result });
};
