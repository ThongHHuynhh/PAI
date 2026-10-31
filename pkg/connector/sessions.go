package connector

import "pai/pkg/harness"

// SessionRow is a session's place in the displayed tree. Children have depth 1;
// the harness permits only one level of delegation.
type SessionRow struct {
	harness.Info
	Depth int
}

// SessionTree groups children under their parents without changing infos.
// The input is newest updated first; roots and siblings keep that order.
// A session whose parent is absent (for example, after filtering) is a root.
func SessionTree(infos []harness.Info) []SessionRow {
	present := make(map[string]bool, len(infos))
	children := make(map[string][]harness.Info)
	for _, in := range infos {
		present[in.ID] = true
		if in.Parent != "" {
			children[in.Parent] = append(children[in.Parent], in)
		}
	}
	rows := make([]SessionRow, 0, len(infos))
	for _, in := range infos {
		if in.Parent != "" && present[in.Parent] {
			continue
		}
		rows = append(rows, SessionRow{Info: in})
		for _, child := range children[in.ID] {
			rows = append(rows, SessionRow{Info: child, Depth: 1})
		}
	}
	return rows
}
