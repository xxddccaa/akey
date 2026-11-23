//go:build js && wasm

// Command akey-wasm exposes akey's vault, spec and render packages to the web
// panel. It is compiled with GOOS=js GOARCH=wasm, loaded inside a Web Worker
// and registers a global object `akey` whose functions take strings and
// return plain objects: {ok:true, ...} on success, {ok:false, error:"..."}
// on failure (plus errors:[...] for validation failures). Nothing ever
// throws across the JS boundary.
//
// The encryption key derived from the passphrase stays inside this module;
// only the blob id and write token (which the server needs) go back to JS.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall/js"
)

func main() {
	api := js.Global().Get("Object").New()
	for name, h := range handlers {
		api.Set(name, wrap(name, h))
	}
	js.Global().Set("akey", api)
	select {} // keep the Go runtime alive for callbacks
}

// wrap converts a handler into a js.Func that never panics or throws and
// always returns a plain JS object.
func wrap(name string, h handler) js.Func {
	return js.FuncOf(func(_ js.Value, args []js.Value) (ret any) {
		defer func() {
			if r := recover(); r != nil {
				ret = toJS(map[string]any{"ok": false, "error": fmt.Sprintf("akey.%s: internal error: %v", name, r)})
			}
		}()
		sargs := make([]string, len(args))
		for i, a := range args {
			switch a.Type() {
			case js.TypeString:
				sargs[i] = a.String()
			case js.TypeUndefined, js.TypeNull:
			default:
				return toJS(map[string]any{"ok": false, "error": fmt.Sprintf("akey.%s: 第 %d 个参数必须是字符串", name, i+1)})
			}
		}
		out, err := h(sargs)
		if err != nil {
			res := map[string]any{"ok": false, "error": err.Error()}
			var ve *validationError
			if errors.As(err, &ve) {
				res["errors"] = ve.items
			}
			return toJS(res)
		}
		if out == nil {
			out = map[string]any{}
		}
		out["ok"] = true
		return toJS(out)
	})
}

// toJS turns a Go value into a plain JS object via JSON (handles []string,
// nested maps and json.RawMessage, which js.ValueOf would reject).
func toJS(v map[string]any) js.Value {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"ok": false, "error": "encode result: " + err.Error()})
	}
	return js.Global().Get("JSON").Call("parse", string(b))
}
