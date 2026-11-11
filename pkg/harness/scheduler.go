package harness

import (
	"context"
	"encoding/json"
	"sync"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// parallelReads limits independent read fanout. Every other operation is a
// dependency barrier: shell and persistent interpreters may have undeclared
// effects, so model call order remains authoritative for them.
const parallelReads = 4

func readOnly(c statefile.Record) bool {
	var call provider.Call
	return json.Unmarshal(c.Body, &call) == nil && call.Name == "read"
}

type scheduledResult struct {
	record       statefile.Record
	observations []statefile.Record
}

// executeBatch runs only a bounded contiguous group of independent reads.
// Results retain provider order; each call owns its observations.
func (h *Harness) executeBatch(ctx context.Context, id string, conf statefile.Conf, calls []statefile.Record) []scheduledResult {
	out := make([]scheduledResult, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].record = result(c.ID, "not run")
			if ctx.Err() != nil {
				return
			}
			latest, err := h.Sessions.Load(id)
			if err != nil {
				out[i].record = result(c.ID, "error: "+err.Error())
				return
			}
			var parsed provider.Call
			if err := json.Unmarshal(c.Body, &parsed); err != nil {
				out[i].record = result(c.ID, "error: "+err.Error())
				return
			}
			observer := func(r statefile.Record) { out[i].observations = append(out[i].observations, r) }
			switch {
			case contextBlocked(latest, parsed.Name) || h.memoryBlocked(ctx, latest, parsed.Name):
				out[i].record = result(c.ID, "clarification_required: unresolved competing facts; ask the user before dependent actions")
			case parsed.Name == "context_record" || parsed.Name == "context_view" || parsed.Name == "context_clarify" || parsed.Name == "context_expand":
				out[i].record = h.contextCall(ctx, id, conf, c, parsed, latest, observer)
			default:
				out[i].record = h.execute(ctx, id, conf, c, observer)
			}
			if ctx.Err() != nil {
				out[i].record = result(c.ID, "stopped, effect unknown")
			}
		}()
	}
	wg.Wait()
	return out
}
