package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"pai/pkg/provider"
	"pai/pkg/statefile"
	"pai/pkg/worker"
)

// Execution events are append-only session records, excluded from model
// context. Completed includes the durable terminal frame before receipt.
const executionKind = statefile.Execution

type executionEvent struct {
	State  string       `json:"state"`
	Worker string       `json:"worker,omitempty"`
	Frame  worker.Frame `json:"frame"`
}
type executionKey struct{}
type executionTrace struct {
	session, origin string
	append          func(executionEvent) error
}

func trace(ctx context.Context, name, state string, f worker.Frame) error {
	if t, ok := ctx.Value(executionKey{}).(executionTrace); ok {
		return t.append(executionEvent{State: state, Worker: name, Frame: f})
	}
	return nil
}

func (h *Harness) executionRecord(session, origin string, e executionEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s := h.session(session)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Lost receipts may cause any terminal frame to arrive again. Preserve
	// one durable observation, while allowing a later completed outcome after
	// an earlier uncertainty. Never overwrite the earlier evidence.
	if e.State == "completed" || e.State == "failed" || e.State == "uncertain" {
		chain, err := h.Sessions.Load(session)
		if err != nil {
			return err
		}
		for _, r := range chain {
			if r.Kind == executionKind && r.For == origin && bytes.Equal(r.Body, b) {
				return nil
			}
		}
	}
	f := s.f
	if f == nil {
		f, err = h.Sessions.Open(session)
		if err != nil {
			return err
		}
		defer f.Close()
	}
	if _, err := h.write(s, f, statefile.Record{Header: statefile.Header{Kind: executionKind, For: origin, Type: "application/json"}, Body: b}); err != nil {
		return err
	}
	return f.Sync()
}

// ReconcileResult preserves late results without replacing a prior uncertain
// result or waking chat. Returning nil authorizes a durable receipt.
func (h *Harness) ReconcileResult(name string, f worker.Frame) error {
	if !statefile.ValidID(f.Session) || !statefile.ValidID(f.Origin) {
		return nil // non-session maintenance calls have no session projection
	}
	state := "completed"
	if f.Kind == worker.Error {
		state = "failed"
	}
	if f.Kind == worker.Uncertain {
		state = "uncertain"
	}
	return h.executionRecord(f.Session, f.Origin, executionEvent{State: state, Worker: name, Frame: f})
}

func recoveredExecution(chain []statefile.Record, origin string) statefile.Record {
	var call provider.Call
	for _, r := range chain {
		if r.Kind == statefile.Call && r.ID == origin {
			_ = json.Unmarshal(r.Body, &call)
		}
	}
	if call.Name != "run" && call.Name != "read" && call.Name != "write" {
		return result(origin, "interrupted multi-operation tool; outcome uncertain, inspect postconditions before retry")
	}
	for i := len(chain) - 1; i >= 0; i-- {
		r := chain[i]
		if r.Kind != executionKind || r.For != origin {
			continue
		}
		var e executionEvent
		if json.Unmarshal(r.Body, &e) != nil {
			continue
		}
		if e.State != "completed" && e.State != "failed" {
			continue
		}
		f := e.Frame
		if f.Kind == worker.Result {
			r := result(origin, string(f.Body))
			r.Exit, r.FullOutput = &f.Exit, f.Full
			return r
		}
		if f.Kind == worker.Bytes {
			return result(origin, string(f.Body))
		}
		if f.Kind == worker.Done {
			return result(origin, "completed; recovered durable worker result")
		}
		return result(origin, fmt.Sprintf("error: %s", f.Body))
	}
	return result(origin, "interrupted; outcome uncertain, do not blindly replay")
}
