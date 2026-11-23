package client

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const redacted = "********"

var (
	// sk-..., and similar long API-key-looking tokens.
	reKeyLike = regexp.MustCompile(`\b(sk|pk|rk|ak)-[A-Za-z0-9_\-]{8,}`)
	// key = "value", "apiKey": "value", TOKEN=value ... (TOML/JSON/env)
	reAssign = regexp.MustCompile(`(?i)((?:key|token|secret|password|passwd|credential)[A-Za-z0-9_]*"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,}]+)`)
)

// redact hides known secrets plus anything that looks like a credential
// assignment (old files may hold keys that are no longer in the master).
func redact(s string, secrets []string) string {
	sorted := append([]string(nil), secrets...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, sec := range sorted {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	s = reKeyLike.ReplaceAllString(s, redacted)
	s = reAssign.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAssign.FindStringSubmatch(m)
		val := sub[2]
		if strings.HasPrefix(val, `"`) || strings.HasPrefix(val, `'`) {
			if len(val) <= 2 || strings.Trim(val, `"'`) == redacted {
				return m // empty or already redacted
			}
			return sub[1] + val[:1] + redacted + val[:1]
		}
		if val == redacted || isNumber(val) || val == "true" || val == "false" || val == "null" || strings.HasPrefix(val, "{") || strings.HasPrefix(val, "[") {
			return m
		}
		return sub[1] + redacted
	})
	return s
}

// lineDiff returns a compact unified-style diff (+/- lines with ctx lines
// of context). Empty string when equal.
func lineDiff(a, b string, ctx int) string {
	if a == b {
		return ""
	}
	al, bl := splitLines(a), splitLines(b)
	ops := diffOps(al, bl)
	// mark which op indexes to print (changes plus context)
	show := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind != ' ' {
			for j := i - ctx; j <= i+ctx; j++ {
				if j >= 0 && j < len(ops) {
					show[j] = true
				}
			}
		}
	}
	var sb strings.Builder
	gap := false
	for i, op := range ops {
		if !show[i] {
			gap = true
			continue
		}
		if gap && sb.Len() > 0 {
			sb.WriteString("    ...\n")
		}
		gap = false
		fmt.Fprintf(&sb, "  %c %s\n", op.kind, op.text)
	}
	return sb.String()
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// diffOps computes an LCS-based edit script. Inputs are config files
// (small); very large inputs fall back to delete-all/insert-all.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n*m > 4_000_000 {
		var ops []diffOp
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	// lcs[i][j] = LCS length of a[i:], b[j:]
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
