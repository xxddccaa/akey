package main

// Core of the panel API, free of syscall/js so it can be unit-tested on the
// host. main.go (js/wasm only) adapts these handlers to JavaScript.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/xxddccaa/akey/internal/render"
	"github.com/xxddccaa/akey/internal/spec"
	"github.com/xxddccaa/akey/internal/vault"
)

// handler takes string arguments and returns the fields of a successful
// result; main.go adds ok:true / ok:false,error.
type handler func(args []string) (map[string]any, error)

var handlers = map[string]handler{
	"version":  fnVersion,
	"derive":   fnDerive,
	"lock":     fnLock,
	"unlocked": fnUnlocked,
	"open":     fnOpen,
	"seal":     fnSeal,
	"validate": fnValidate,
	"decode":   fnDecode,
	"encode":   fnEncode,
	"render":   fnRender,
	"template": fnTemplate,
	"compat":   fnCompat,
	"toYAML":   fnToYAML,
	"fromYAML": fnFromYAML,
}

// version is set with -ldflags "-X main.version=...".
var version = "dev"

// keys holds the derived keys between derive() and lock(). The encryption
// key never leaves this module.
var keys *vault.Keys

var errLocked = errors.New("已锁定：请先输入口令解锁")

const maxJSON = 8 << 20 // cap for YAML->JSON conversion output (alias bombs)

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func fnVersion(args []string) (map[string]any, error) {
	return map[string]any{"version": version, "tools": spec.ToolNames}, nil
}

// derive(passphrase, paramsJSON) -> {id, writeToken}
func fnDerive(args []string) (map[string]any, error) {
	pass, pj := arg(args, 0), arg(args, 1)
	if pass == "" {
		return nil, errors.New("口令不能为空")
	}
	var p vault.Params
	if err := json.Unmarshal([]byte(pj), &p); err != nil {
		return nil, fmt.Errorf("服务器返回的 KDF 参数无法解析: %v", err)
	}
	k, err := vault.Derive(pass, p)
	if err != nil {
		return nil, err
	}
	wipe()
	keys = k
	return map[string]any{"id": k.ID, "writeToken": k.WriteToken}, nil
}

func wipe() {
	if keys != nil {
		for i := range keys.Enc {
			keys.Enc[i] = 0
		}
		keys = nil
	}
}

func fnLock(args []string) (map[string]any, error) {
	wipe()
	return nil, nil
}

func fnUnlocked(args []string) (map[string]any, error) {
	return map[string]any{"unlocked": keys != nil}, nil
}

// open(envelope) -> {yaml}
func fnOpen(args []string) (map[string]any, error) {
	if keys == nil {
		return nil, errLocked
	}
	pt, err := vault.Open(keys, []byte(arg(args, 0)))
	if errors.Is(err, vault.ErrDecrypt) {
		return nil, errors.New("解密失败：口令不对，或者服务器上的数据已损坏")
	}
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(pt) {
		return nil, errors.New("解密后的内容不是 UTF-8 文本")
	}
	return map[string]any{"yaml": string(pt)}, nil
}

// seal(yaml) -> {envelope}. Validates first; seals the exact bytes given.
func fnSeal(args []string) (map[string]any, error) {
	if keys == nil {
		return nil, errLocked
	}
	y := arg(args, 0)
	if _, err := spec.Parse([]byte(y)); err != nil {
		return map[string]any{}, &validationError{splitErrors(err)}
	}
	env, err := vault.Seal(keys, []byte(y))
	if err != nil {
		return nil, err
	}
	return map[string]any{"envelope": string(env)}, nil
}

type validationError struct{ items []string }

func (e *validationError) Error() string {
	return "配置校验失败:\n  - " + strings.Join(e.items, "\n  - ")
}

// validate(yaml) -> {errors:[...], valid}
func fnValidate(args []string) (map[string]any, error) {
	errs := []string{}
	if _, err := spec.Parse([]byte(arg(args, 0))); err != nil {
		errs = splitErrors(err)
	}
	return map[string]any{"errors": errs, "valid": len(errs) == 0}, nil
}

// splitErrors turns spec's multi-line error into individual items.
func splitErrors(err error) []string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	var out []string
	for i, l := range lines {
		l = strings.TrimSpace(l)
		l = strings.TrimPrefix(l, "- ")
		if i == 0 {
			for _, p := range []string{"invalid master config:", "parse master config: yaml: unmarshal errors:", "parse master config: "} {
				l = strings.TrimSpace(strings.TrimPrefix(l, p))
			}
		}
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		out = []string{err.Error()}
	}
	return out
}

// decode(yaml) -> {json: object}. Strict parse, no defaults. Providers keep
// their file order, tools follow the canonical order.
func fnDecode(args []string) (map[string]any, error) {
	src := []byte(arg(args, 0))
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, errors.New("YAML 为空")
	}
	m, err := spec.Decode(src)
	if err != nil {
		return nil, &validationError{splitErrors(err)}
	}
	n, err := masterNode(m, providerOrder(src))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := nodeToJSON(&buf, n); err != nil {
		return nil, err
	}
	return map[string]any{"json": json.RawMessage(buf.Bytes())}, nil
}

// encode(json) -> {yaml}. JSON is valid YAML, so spec.Decode checks it
// strictly against the schema; the output is the canonical marshalling.
func fnEncode(args []string) (map[string]any, error) {
	src := []byte(arg(args, 0))
	m, err := spec.Decode(src)
	if err != nil {
		return nil, &validationError{splitErrors(err)}
	}
	if m.Version == 0 {
		m.Version = 1
	}
	n, err := masterNode(m, providerOrder(src))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	return map[string]any{"yaml": buf.String()}, nil
}

// render(yaml, tool) -> {files:[{tool, path, content, mode}]}, keys redacted.
// tool "" renders every enabled tool.
func fnRender(args []string) (map[string]any, error) {
	m, err := spec.Parse([]byte(arg(args, 0)))
	if err != nil {
		return nil, &validationError{splitErrors(err)}
	}
	var files []render.File
	if t := arg(args, 1); t != "" {
		files, err = render.Render(m, t)
	} else {
		files, err = render.RenderAll(m, nil)
	}
	if err != nil {
		return nil, err
	}
	secrets := m.Secrets()
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{
			"tool":    f.Tool,
			"path":    f.RelPath,
			"content": redact(string(f.Content), secrets),
			"mode":    fmt.Sprintf("%04o", f.Mode),
		})
	}
	return map[string]any{"files": out}, nil
}

var reKeyLike = regexp.MustCompile(`\b(sk|pk|rk|ak)-[A-Za-z0-9_\-]{8,}`)

// redact replaces provider keys (and other sk-... looking tokens) with a
// short recognisable prefix, e.g. "sk-t…(redacted)".
func redact(s string, secrets []string) string {
	sorted := append([]string(nil), secrets...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, sec := range sorted {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, maskKey(sec))
		}
	}
	return reKeyLike.ReplaceAllStringFunc(s, maskKey)
}

func maskKey(k string) string {
	if utf8.RuneCountInString(k) >= 12 {
		r := []rune(k)
		return string(r[:4]) + "…(redacted)"
	}
	return "…(redacted)"
}

func fnTemplate(args []string) (map[string]any, error) {
	return map[string]any{"yaml": string(spec.Template())}, nil
}

// compat(yaml) -> {codex:[ids], ..., compatible:{tool:[ids]}, providersApi:{id:[apis]},
// modelApi:{id:api}, errors:[...]}
//
// Top-level tool keys are the tool's catalog (ModelsFor: compatible and
// within tools.<t>.models); `compatible` ignores tools.<t>.models (for the
// models subset picker). Works on partially invalid configs.
func fnCompat(args []string) (map[string]any, error) {
	m, err := spec.Decode([]byte(arg(args, 0)))
	if err != nil {
		return nil, &validationError{splitErrors(err)}
	}
	errs := []string{}
	if err := m.Validate(); err != nil {
		errs = splitErrors(err)
	}
	out := map[string]any{"errors": errs}
	compatible := map[string][]string{}
	for _, t := range spec.ToolNames {
		cat := []string{}
		for _, md := range m.ModelsFor(t) {
			cat = append(cat, md.ID)
		}
		out[t] = cat
		all := []string{}
		for _, md := range m.Models {
			if md != nil && md.ID != "" && m.Providers[md.Provider] != nil && m.ModelAllowedFor(md, t) {
				all = append(all, md.ID)
			}
		}
		compatible[t] = all
	}
	out["compatible"] = compatible
	papi := map[string][]string{}
	for id, p := range m.Providers {
		apis := []string{}
		if p != nil && p.OpenAI != nil {
			apis = append(apis, spec.APIOpenAI)
		}
		if p != nil && p.Anthropic != nil {
			apis = append(apis, spec.APIAnthropic)
		}
		papi[id] = apis
	}
	out["providersApi"] = papi
	mapi := map[string]string{}
	for _, md := range m.Models {
		if md != nil && md.ID != "" {
			mapi[md.ID] = md.API
		}
	}
	out["modelApi"] = mapi
	return out, nil
}

// toYAML(json) -> {yaml}: block-style YAML for an arbitrary JSON value (used
// for the extra / overrides text areas). Key order is preserved.
func fnToYAML(args []string) (map[string]any, error) {
	src := strings.TrimSpace(arg(args, 0))
	if src == "" || src == "null" || src == "{}" {
		return map[string]any{"yaml": ""}, nil
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		return nil, fmt.Errorf("JSON 无法解析: %v", err)
	}
	blockStyle(&n)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	return map[string]any{"yaml": buf.String()}, nil
}

// fromYAML(yaml) -> {json: value}: parse a YAML snippet (extra / overrides).
// Empty input gives null.
func fnFromYAML(args []string) (map[string]any, error) {
	src := []byte(arg(args, 0))
	if len(bytes.TrimSpace(src)) == 0 {
		return map[string]any{"json": nil}, nil
	}
	var n yaml.Node
	if err := yaml.Unmarshal(src, &n); err != nil {
		return nil, fmt.Errorf("YAML 无法解析: %v", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var buf bytes.Buffer
	if err := nodeToJSON(&buf, &n); err != nil {
		return nil, err
	}
	return map[string]any{"json": json.RawMessage(buf.Bytes())}, nil
}

// ---------------------------------------------------------------------------
// YAML node helpers

// providerOrder returns the keys of the top-level `providers` mapping in
// document order (works for YAML and JSON input).
func providerOrder(src []byte) []string {
	var n yaml.Node
	if yaml.Unmarshal(src, &n) != nil || len(n.Content) == 0 {
		return nil
	}
	root := n.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "providers" && root.Content[i+1].Kind == yaml.MappingNode {
			var keys []string
			pm := root.Content[i+1]
			for j := 0; j+1 < len(pm.Content); j += 2 {
				keys = append(keys, pm.Content[j].Value)
			}
			return keys
		}
	}
	return nil
}

// masterNode marshals m into a YAML node with providers in the given order
// (unknown ones after, sorted) and tools in canonical order.
func masterNode(m *spec.Master, order []string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := doc.Encode(m); err != nil {
		return nil, err
	}
	flowShortLists(&doc)
	toolOrder := append([]string(nil), spec.ToolNames...)
	for i := 0; i+1 < len(doc.Content); i += 2 {
		switch doc.Content[i].Value {
		case "providers":
			reorder(doc.Content[i+1], order)
		case "tools":
			reorder(doc.Content[i+1], toolOrder)
		}
	}
	return &doc, nil
}

func reorder(mn *yaml.Node, order []string) {
	if mn.Kind != yaml.MappingNode || len(order) == 0 {
		return
	}
	rank := map[string]int{}
	for i, k := range order {
		if _, ok := rank[k]; !ok {
			rank[k] = i
		}
	}
	type pair struct{ k, v *yaml.Node }
	pairs := make([]pair, 0, len(mn.Content)/2)
	for i := 0; i+1 < len(mn.Content); i += 2 {
		pairs = append(pairs, pair{mn.Content[i], mn.Content[i+1]})
	}
	sort.SliceStable(pairs, func(a, b int) bool {
		ra, oka := rank[pairs[a].k.Value]
		rb, okb := rank[pairs[b].k.Value]
		switch {
		case oka && okb:
			return ra < rb
		case oka != okb:
			return oka
		}
		return pairs[a].k.Value < pairs[b].k.Value
	})
	mn.Content = mn.Content[:0]
	for _, p := range pairs {
		mn.Content = append(mn.Content, p.k, p.v)
	}
}

// flowShortLists writes short lists of plain scalars in flow style, e.g.
// reasoning_efforts: [low, medium, high], like the hand-written examples.
func flowShortLists(n *yaml.Node) {
	if n.Kind == yaml.SequenceNode && len(n.Content) > 0 {
		width := 0
		flat := true
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode || strings.ContainsAny(c.Value, "\n,[]{}#:\"'") {
				flat = false
				break
			}
			width += len(c.Value) + 2
		}
		if flat && width <= 72 {
			n.Style = yaml.FlowStyle
			return
		}
	}
	for _, c := range n.Content {
		flowShortLists(c)
	}
}

// blockStyle clears flow/quoted styles (from JSON input) so the encoder
// emits readable block YAML; multi-line strings become literal blocks.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" && strings.Contains(n.Value, "\n") {
		n.Style = yaml.LiteralStyle
	}
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// nodeToJSON writes n as JSON, preserving mapping order and resolving
// aliases. Merge keys (<<) are not supported.
func nodeToJSON(buf *bytes.Buffer, n *yaml.Node) error {
	if buf.Len() > maxJSON {
		return errors.New("YAML 展开后过大")
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			buf.WriteString("null")
			return nil
		}
		return nodeToJSON(buf, n.Content[0])
	case yaml.AliasNode:
		return nodeToJSON(buf, n.Alias)
	case yaml.MappingNode:
		buf.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if k.Tag == "!!merge" {
				return fmt.Errorf("第 %d 行：不支持 << 合并键", k.Line)
			}
			if k.Kind != yaml.ScalarNode {
				return fmt.Errorf("第 %d 行：键必须是普通值", k.Line)
			}
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k.Value)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := nodeToJSON(buf, n.Content[i+1]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case yaml.SequenceNode:
		buf.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := nodeToJSON(buf, c); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return fmt.Errorf("第 %d 行: %v", n.Line, err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("第 %d 行：无法表示为 JSON 的值 %q", n.Line, n.Value)
		}
		buf.Write(b)
	default:
		return fmt.Errorf("第 %d 行：无法识别的 YAML 节点", n.Line)
	}
	return nil
}
