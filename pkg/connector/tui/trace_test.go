package tui

import (
	"testing"
	"time"

	"pai/pkg/statefile"
)

// A turn: the model streams a call, the call runs, the model answers. The
// trace puts each step before its calls and times the call from the step.
func TestTurnTrace(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	h := func(kind statefile.Kind, s int) statefile.Header {
		return statefile.Header{Kind: kind, Time: at(s), Via: "tui"}
	}
	var tr turnTrace
	msg := statefile.Record{Header: h(statefile.Message, 0)}
	call := statefile.Record{Header: h(statefile.Call, 9)}
	call.ID = "c1"
	step1 := statefile.Record{Header: h(statefile.Step, 10)}
	step1.Sent, step1.Usage = at(1), &statefile.Usage{Input: 10, CacheRead: 90, Output: 5}
	res := statefile.Record{Header: h(statefile.Result, 14)}
	res.For = "c1"
	step2 := statefile.Record{Header: h(statefile.Step, 20)}
	step2.Sent = at(15)
	steer := statefile.Record{Header: h(statefile.Message, 12)}

	tr.add(msg, nil)
	tr.add(call, &callBlock{name: "run", args: map[string]any{"worker": "admin"}})
	tr.add(step1, nil)
	if tr.done || tr.running() == nil {
		t.Fatal("a step with a call pending ended the turn")
	}
	tr.add(steer, nil) // a message mid-turn steers it, and starts none
	tr.add(res, nil)
	tr.add(step2, nil)
	if !tr.done || !tr.start.Equal(at(0)) {
		t.Fatalf("done %v, start %v", tr.done, tr.start)
	}
	if len(tr.spans) != 3 || tr.spans[0].name != "model" || tr.spans[1].name != "run" || tr.spans[2].name != "model" {
		t.Fatalf("spans %+v", tr.spans)
	}
	model, tools, prep, u := tr.totals(at(30))
	if model != 14*time.Second || tools != 4*time.Second || prep != 2*time.Second {
		t.Fatalf("model %s, tools %s, prep %s", model, tools, prep)
	}
	if cached(u) != "cache 90%" {
		t.Fatalf("got %q", cached(u))
	}
	tr.add(statefile.Record{Header: h(statefile.Message, 40)}, nil)
	if len(tr.spans) != 0 || !tr.start.Equal(at(40)) {
		t.Fatal("a message after the turn ended did not start a new one")
	}
}
