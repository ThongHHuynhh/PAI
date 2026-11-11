package harness

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// stripCache drops cache_control marks: they move to the newest block each
// step, and the cache matches the content, not where the mark sits.
func stripCache(v any) any {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "cache_control")
		for k, e := range x {
			x[k] = stripCache(e)
		}
	case []any:
		for i, e := range x {
			x[i] = stripCache(e)
		}
	}
	return v
}

// After a compaction, with typed context recorded, each request must extend
// the one before it. A note added to a request but not saved in the file
// breaks that, and every step then misses the provider cache (v0.4.0, v0.4.1).
func TestStepsAfterCompactionExtendThePreviousRequest(t *testing.T) {
	var bodies [][]byte
	p := provider.Anthropic("", "")
	p.Run = func(ctx context.Context, body []byte, emit func(statefile.Record)) (statefile.Header, error) {
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			call, _ := json.Marshal(provider.Call{Name: "recall", Arguments: json.RawMessage(`{"query":"x"}`)})
			emit(statefile.Record{Header: statefile.Header{Kind: statefile.Call, Type: "application/json"}, Body: call})
			return statefile.Header{Stop: "tool_use", Usage: &statefile.Usage{Input: 100}}, nil
		}
		emit(statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain"}, Body: []byte("done")})
		return statefile.Header{Stop: "end_turn", Usage: &statefile.Usage{Input: 100}}, nil
	}
	h := &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}, Workers: NewPool(), Providers: map[string]provider.Provider{"fake": p}}
	id, err := h.Create(context.Background(), Create{Model: "fake/m", ContextLimit: 100000, Tools: "default"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := h.session(id)
	msg := func(text string) statefile.Record {
		return statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte(text)}
	}
	if err := h.note(s, id, msg("old task")); err != nil {
		t.Fatal(err)
	}
	chain, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	upto := chain[len(chain)-1].N
	for _, r := range append(contextFixture()[:1], statefile.Record{Header: statefile.Header{Kind: statefile.Compaction, Type: "text/markdown", Source: "compaction", Upto: upto}, Body: []byte("summary\n\n" + archiveIndex(upto))}, msg("new task")) {
		r.N = 0
		if err := h.note(s, id, r); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := h.Run(context.Background(), id); err != nil || st != statefile.StatusClosed {
		t.Fatalf("run: %s %v", st, err)
	}
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	var first, second struct{ Messages []any }
	if err := json.Unmarshal(bodies[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bodies[1], &second); err != nil {
		t.Fatal(err)
	}
	a, b := stripCache(first.Messages).([]any), stripCache(second.Messages).([]any)
	if len(b) <= len(a) || !reflect.DeepEqual(a, b[:len(a)]) {
		t.Fatalf("the second request does not extend the first:\nfirst:  %s\nsecond: %s", bodies[0], bodies[1])
	}
	if n := strings.Count(string(bodies[1]), "remain in this session archive"); n != 1 {
		t.Fatalf("archive note appears %d times, want once, in the summary", n)
	}
}
