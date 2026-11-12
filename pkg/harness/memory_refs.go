package harness

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// Memory references track what this session actually consulted: which items
// are in its context, so recall does not carry them again. A disputed item is
// noted where it is shown and never blocks a tool.
func (h *Harness) rememberRefs(id string, items []memory.Item) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	body, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return h.note(h.session(id), id, statefile.Record{Header: statefile.Header{Kind: statefile.MemoryRef, Type: "application/json"}, Body: body})
}

// inContext is the memory items shown to the session since its newest
// compaction: still in its context word for word, so recalling them again
// only costs tokens. Items shown before the compaction are folded into its
// summary and may be recalled again.
func inContext(chain []statefile.Record) []string {
	upto := 0
	for _, r := range chain {
		if r.Kind == statefile.Compaction {
			upto = max(upto, r.Upto)
		}
	}
	var ids []string
	for _, r := range chain {
		if r.Kind != statefile.MemoryRef || r.N <= upto {
			continue
		}
		var got []string
		if json.Unmarshal(r.Body, &got) != nil {
			continue
		}
		for _, id := range got {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// sessionScopes is the project scopes of memory a session works on: each
// project whose name the person wrote in a message, or that the session saved
// memory into. A session about one project then recalls that project's items
// and not another's.
func (h *Harness) sessionScopes(ctx context.Context, chain []statefile.Record) []string {
	if h.Memory == nil {
		return nil
	}
	projects, err := h.Memory.Projects(ctx)
	if err != nil || len(projects) == 0 {
		return nil
	}
	var said strings.Builder
	var out []string
	for _, r := range chain {
		switch {
		case r.Kind == statefile.Message && strings.HasPrefix(r.Type, "text/") && !strings.HasPrefix(r.Via, "session:"):
			said.Write(r.Body)
			said.WriteByte('\n')
		case r.Kind == statefile.Call:
			var c provider.Call
			if json.Unmarshal(r.Body, &c) != nil || c.Name != "remember" {
				continue
			}
			var a struct{ Scope string }
			if json.Unmarshal(c.Arguments, &a) == nil && strings.HasPrefix(memory.NormScope(a.Scope), "project:") && !slices.Contains(out, memory.NormScope(a.Scope)) {
				out = append(out, memory.NormScope(a.Scope))
			}
		}
	}
	text := strings.ToLower(said.String())
	for _, p := range projects {
		if !slices.Contains(out, p) && mentions(text, strings.TrimPrefix(p, "project:")) {
			out = append(out, p)
		}
	}
	return out
}

// mentions is whether lower case text names a project as a whole word:
// "pai" in "PAI's memory" but not in "paid".
func mentions(text, name string) bool {
	if name == "" {
		return false
	}
	return regexp.MustCompile(`(^|[^\pL\pN_])` + regexp.QuoteMeta(name) + `($|[^\pL\pN_])`).MatchString(text)
}

// readable is whether a session may read or forget a memory item by its id:
// anything but another session's own items.
func readable(it memory.Item, session string) bool {
	return !strings.HasPrefix(it.Scope, "session:") || it.Scope == "session:"+session
}
