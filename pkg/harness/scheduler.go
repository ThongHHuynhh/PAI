package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// held answers a call that changes things when the person wrote after the
// step that made it: what they wrote may change or cancel it (11.5).
const held = "not run: the person sent message %d after you planned this call. Read it first; call again only if it still applies"

// steered is the newest message from the person that the step which made
// call c did not see, or 0.
func steered(chain []statefile.Record, c statefile.Record) int {
	upto, n := 0, 0
	for _, r := range chain {
		switch {
		case r.Kind == statefile.Step && r.ID == c.Step:
			upto = r.Upto
		case r.Kind == statefile.Message && r.Via != "job" && r.Via != "retry" && !strings.HasPrefix(r.Via, "session:"):
			n = r.N
		}
	}
	if upto == 0 || n <= upto {
		return 0
	}
	return n
}

// CancelCall cancels one running call of the session: its result says who
// cancelled it, and the step goes on with the next call.
func (h *Harness) CancelCall(id, call, via string) error {
	s := h.session(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	cancel, ok := s.calls[call]
	if !ok {
		return fmt.Errorf("call %s of session %s is not running", call, id)
	}
	cancel(fmt.Errorf("cancelled by %s, effect unknown: it may have partly run", via))
	return nil
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
			if n := steered(latest, c); n > 0 && !slices.Contains(readOnlyTools, parsed.Name) {
				out[i].record = result(c.ID, fmt.Sprintf(held, n))
				return
			}
			s := h.session(id)
			cctx, cancel := context.WithCancelCause(ctx)
			s.mu.Lock()
			if s.calls == nil {
				s.calls = map[string]context.CancelCauseFunc{}
			}
			s.calls[c.ID] = cancel
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				delete(s.calls, c.ID)
				s.mu.Unlock()
				cancel(nil)
			}()
			observer := func(r statefile.Record) { out[i].observations = append(out[i].observations, r) }
			switch {
			case contextBlocked(latest, parsed.Name):
				out[i].record = result(c.ID, "clarification_required: unresolved competing facts; ask the user before dependent actions")
			case parsed.Name == "context_record" || parsed.Name == "context_view" || parsed.Name == "context_clarify" || parsed.Name == "context_expand":
				out[i].record = h.contextCall(cctx, id, conf, c, parsed, latest, observer)
			default:
				out[i].record = h.execute(cctx, id, conf, c, observer)
			}
			switch cause := context.Cause(cctx); {
			case ctx.Err() != nil:
				out[i].record = result(c.ID, "stopped, effect unknown")
			case cctx.Err() != nil && !errors.Is(cause, context.Canceled):
				out[i].record = result(c.ID, cause.Error())
			}
		}()
	}
	wg.Wait()
	return out
}
