package harness

import (
	"context"
	"encoding/json"
	"pai/pkg/memory"
	"pai/pkg/statefile"
	"slices"
)

// Memory references track what this session actually consulted. Unrelated
// conflicts elsewhere must not block a task. Values are always rechecked.
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
func (h *Harness) memoryBlocked(ctx context.Context, chain []statefile.Record, name string) bool {
	if h.Memory == nil || slices.Contains([]string{"read", "read_page", "recall", "ask", "diagnose", "compact", "memory_clarify", "context_expand", "context_view", "context_record", "context_clarify"}, name) {
		return false
	}
	seen := map[string]bool{}
	for _, r := range chain {
		if r.Kind != statefile.MemoryRef {
			continue
		}
		var ids []string
		if json.Unmarshal(r.Body, &ids) != nil {
			return true
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			it, err := h.Memory.Get(ctx, id)
			if err != nil || len(it.Conflicts) > 0 {
				return true
			}
		}
	}
	return false
}
