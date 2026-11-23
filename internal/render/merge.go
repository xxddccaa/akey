package render

import (
	"fmt"
	"sort"
)

// deepMerge merges src into dst in place and returns dst (allocated if nil).
//
// Rules:
//   - map + map: merged recursively;
//   - anything else (scalars, arrays, type changes): src replaces dst;
//   - a nil value in src (YAML `~` / `null`) deletes the key from dst, so an
//     override can remove a generated setting.
//
// Values taken from src are deep-copied, so the master config is never
// aliased by (or mutated through) rendered documents.
func deepMerge(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, sv := range src {
		if sv == nil {
			delete(dst, k)
			continue
		}
		sv = normalize(sv)
		if sm, ok := sv.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = deepMerge(dm, sm)
				continue
			}
			dst[k] = deepMerge(nil, sm)
			continue
		}
		dst[k] = sv
	}
	return dst
}

// normalize deep-copies a YAML-decoded value, converting map[any]any into
// map[string]any so every encoder sees the same shapes.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalize(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[fmt.Sprint(k)] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	}
	return v
}

// child returns m[key] as a map, creating it (or replacing a non-map) if needed.
func child(m map[string]any, key string) map[string]any {
	if c, ok := m[key].(map[string]any); ok {
		return c
	}
	c := map[string]any{}
	m[key] = c
	return c
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func strList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
