package project

import (
	"fmt"
	"strings"
)

// maxDiffCells bounds the line diff's table; bigger files show as replaced.
const maxDiffCells = 4_000_000

// Diff is a line diff of a and b: lines prefixed "-", "+" or " ", with
// unchanged runs longer than the context folded.
func Diff(a, b string) string {
	if a == b {
		return ""
	}
	x, y := splitLines(a), splitLines(b)
	if len(x)*len(y) > maxDiffCells {
		return fmt.Sprintf("~ replaced: %d lines before, %d after\n", len(x), len(y))
	}
	// lcs[i][j] is the longest common subsequence of x[i:] and y[j:].
	lcs := make([][]int32, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []string
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			ops = append(ops, " "+x[i])
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			ops = append(ops, "+"+y[j])
			j++
		default:
			ops = append(ops, "-"+x[i])
			i++
		}
	}
	return fold(ops, 2)
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// fold keeps ctx unchanged lines around each change.
func fold(ops []string, ctx int) string {
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o[0] != ' ' {
			for k := max(0, i-ctx); k <= min(len(ops)-1, i+ctx); k++ {
				keep[k] = true
			}
		}
	}
	var b strings.Builder
	skipped := 0
	for i, o := range ops {
		if !keep[i] {
			skipped++
			continue
		}
		if skipped > 0 {
			fmt.Fprintf(&b, "  … %d unchanged\n", skipped)
			skipped = 0
		}
		b.WriteString(o)
		b.WriteByte('\n')
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "  … %d unchanged\n", skipped)
	}
	return b.String()
}
