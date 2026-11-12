package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// A turn whose model gave no recall feedback stayed unscored, so recall
// learnt from almost nothing (v0.4.0 to v0.4.4). Grading scores it.
func TestGradeScoresTurnsWithNoFeedback(t *testing.T) {
	ctx := context.Background()
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	it, _, _ := st.Remember(ctx, memory.Item{Text: "The staging database uses port 5433", Source: "test"})
	rv, err := st.Recall(ctx, memory.Turn{Session: "s1", Query: "Which port does the staging database use?"})
	if err != nil || len(rv.Hits) == 0 {
		t.Fatalf("recall: %v %+v", err, rv)
	}
	if err := st.Settle(ctx, "s1", "", false); err != nil {
		t.Fatal(err)
	}
	var asked string
	p := provider.Anthropic("", "")
	p.Run = func(ctx context.Context, body []byte, emit func(statefile.Record)) (statefile.Header, error) {
		asked = string(body)
		reply := fmt.Sprintf("```json\n{%q: {%q: \"used\"}}\n```", rv.ID, it.ID)
		emit(statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain"}, Body: []byte(reply)})
		return statefile.Header{Stop: "end_turn"}, nil
	}
	h := &Harness{Memory: st, Providers: map[string]provider.Provider{"fake": p}}
	rec := func(n int, kind statefile.Kind, source, body string) statefile.Record {
		return statefile.Record{Header: statefile.Header{N: n, Kind: kind, Source: source, Type: "text/plain"}, Body: []byte(body)}
	}
	chain := []statefile.Record{
		rec(1, statefile.Message, "", "Which port does the staging database use?"),
		rec(2, statefile.System, "memory", "Memory recalled for this turn (retrieval "+rv.ID+"): data\n"),
		rec(3, statefile.Text, "", "Port 5433."),
	}
	h.grade(ctx, statefile.Conf{Model: "fake/m"}, "s1", chain, 0, 3)
	for _, want := range []string{"Which port does the staging", it.ID, "Port 5433."} {
		if !strings.Contains(asked, want) {
			t.Fatalf("the grader was not shown %q", want)
		}
	}
	rs, _ := st.Retrievals(ctx, "s1", 10)
	if rs[0].Reward == nil || *rs[0].Reward <= 0 {
		t.Fatalf("graded turn not scored: %+v", rs[0])
	}
}
