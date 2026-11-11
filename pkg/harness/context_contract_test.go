package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pai/pkg/contextflow"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

func contextFixture() []statefile.Record {
	var chain []statefile.Record
	for i, v := range []string{"alpha", "beta"} {
		x := contextflow.Item{ID: v, Layer: contextflow.Evidence, Text: v, Key: "target", Value: v, Sources: []contextflow.Ref{{Session: "s", Record: 1}}}
		b, _ := json.Marshal(contextEvent{Item: &x})
		chain = append(chain, statefile.Record{Header: statefile.Header{N: i + 1, Kind: statefile.Context}, Body: b})
	}
	return chain
}
func TestContextClarificationGate(t *testing.T) {
	h := &Harness{}
	chain := contextFixture()
	call := statefile.Record{Header: statefile.Header{ID: "clarify"}}
	invoke := func(raw string) (statefile.Record, []statefile.Record) {
		var added []statefile.Record
		r := h.contextCall(context.Background(), "s", statefile.Conf{}, call, provider.Call{Name: "context_clarify", Arguments: json.RawMessage(raw)}, chain, func(r statefile.Record) { added = append(added, r) })
		return r, added
	}
	if !contextBlocked(chain, "run") || !contextBlocked(chain, "remember") || contextBlocked(chain, "read_page") {
		t.Fatal("unsafe gate")
	}
	r, _ := invoke(`{"key":"target"}`)
	_, raw, ok := strings.Cut(string(r.Body), "ID: ")
	if !ok {
		t.Fatalf("no ask arguments: %s", r.Body)
	}
	qs, err := parseQuestions(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(provider.Call{Name: "ask", Arguments: json.RawMessage(raw)})
	chain = append(chain, statefile.Record{Header: statefile.Header{Kind: statefile.Call, ID: "ask1"}, Body: b})
	for _, answer := range []string{"The user declined to answer the questions.", "error: no answer", answered(qs, Reply{Answers: [][]string{{"perhaps"}}})} {
		chain = append(chain, statefile.Record{Header: statefile.Header{Kind: statefile.Result, For: "ask1"}, Body: []byte(answer)})
		_, added := invoke(`{"key":"target","ask_call":"ask1"}`)
		if len(added) != 0 || !contextBlocked(chain, "write") {
			t.Fatal("unanswered conflict cleared")
		}
	}
	chain = append(chain, statefile.Record{Header: statefile.Header{Kind: statefile.Result, For: "ask1"}, Body: []byte(answered(qs, Reply{Answers: [][]string{{"beta"}}}))})
	_, added := invoke(`{"key":"target","ask_call":"ask1"}`)
	if len(added) != 1 {
		t.Fatal("explicit user answer not recorded")
	}
	chain = append(chain, added...)
	if contextBlocked(chain, "write") {
		t.Fatal("explicit decision not recognized")
	}
	// New evidence invalidates the old decision's coverage, even on same key.
	extra := contextFixture()[0]
	var e contextEvent
	_ = json.Unmarshal(extra.Body, &e)
	e.Item.ID = "new-evidence"
	extra.Body, _ = json.Marshal(e)
	chain = append(chain, extra)
	if !contextBlocked(chain, "write") {
		t.Fatal("old decision resolved unseen evidence")
	}
	if !strings.Contains(string(contextProjection(chain).Body), "alpha") {
		t.Fatal("lost competing original")
	}
}
func TestProtectedOriginalsAndSummaryLinks(t *testing.T) {
	chain := []statefile.Record{
		{Header: statefile.Header{N: 1, Kind: statefile.Message, ID: "intent", Type: "text/plain"}, Body: []byte("do not deploy")},
		{Header: statefile.Header{N: 2, Kind: statefile.Result, ID: "evidence", Type: "text/plain"}, Body: []byte("contradiction and failed attempt")},
		{Header: statefile.Header{N: 3, Kind: statefile.Compaction, ID: "summary1", Upto: 2}, Body: []byte("lossy summary")},
	}
	out := archiveIndex(2)
	if strings.Contains(out, "do not deploy") || !strings.Contains(out, "context_expand") || !strings.Contains(out, "1..2") {
		t.Fatal("original protection failed")
	}
	r := statefile.Record{Header: statefile.Header{Upto: 3}}
	linkSummary(&r, chain)
	if len(r.Originals) != 2 || len(r.Parents) != 1 || r.Parents[0] != "summary1" {
		t.Fatalf("missing hierarchical links: %+v", r)
	}
}
func TestContextEventIsMetadata(t *testing.T) {
	chain := []statefile.Record{{Header: statefile.Header{Kind: statefile.Start, N: 1}}, {Header: statefile.Header{Kind: statefile.Context, N: 2}, Body: []byte(`{}`)}}
	if statefile.Derive(chain) != statefile.StatusClosed {
		t.Fatal("metadata reopened session")
	}
	built, tip := statefile.Build(chain)
	if len(built) != 0 || tip != 2 {
		t.Fatalf("metadata leaked: %+v", built)
	}
}
func decodeEnvelope(t *testing.T, r statefile.Record) toolEnvelope {
	t.Helper()
	var e toolEnvelope
	if err := json.Unmarshal(r.Body, &e); err != nil {
		t.Fatalf("invalid envelope: %v: %s", err, r.Body)
	}
	return e
}
func TestPlanApplyPreconditions(t *testing.T) {
	w := &instructionWorker{content: []byte("old content")}
	observe := func(string, []byte) {}
	r, _ := structuredFile(context.Background(), w, "plan_edit", args{Path: "file", Old: "old", New: "new"}, observe)
	plan := decodeEnvelope(t, r).Plan
	if plan == nil || w.writes != 0 {
		t.Fatal("planning mutated file")
	}
	w.content = []byte("concurrent edit")
	r, _ = structuredFile(context.Background(), w, "apply_edit", args{Plan: plan}, observe)
	if e := decodeEnvelope(t, r); e.Error == nil || e.Error.Code != "precondition_failed" || w.writes != 0 {
		t.Fatalf("stale plan wrote: %+v", e)
	}
	w.content = []byte("old content")
	r, _ = structuredFile(context.Background(), w, "apply_edit", args{Plan: plan}, observe)
	if e := decodeEnvelope(t, r); e.Error != nil || string(w.content) != "new content" {
		t.Fatalf("apply failed %+v", e)
	}
	// Replaying a plan must fail, rather than write again.
	r, _ = structuredFile(context.Background(), w, "apply_edit", args{Plan: plan}, observe)
	if decodeEnvelope(t, r).Error == nil || w.writes != 1 {
		t.Fatal("plan replay accepted")
	}
}
func TestReadPagesAndChangedArtifact(t *testing.T) {
	w := &instructionWorker{content: []byte("a\u20acb")}
	observe := func(string, []byte) {}
	r, _ := structuredFile(context.Background(), w, "read_page", args{Path: "f", Limit: 3}, observe)
	e := decodeEnvelope(t, r)
	if e.Text != "a" || e.Next == nil || *e.Next != 1 {
		t.Fatalf("split UTF8: %+v", e)
	}
	r, _ = structuredFile(context.Background(), w, "read_page", args{Path: "f", Offset: *e.Next, Limit: 4, ExpectedSHA256: e.Evidence[0].SHA256}, observe)
	if next := decodeEnvelope(t, r); next.Text != "\u20acb" || next.Truncated {
		t.Fatalf("wrong expansion %+v", next)
	}
	w.content = []byte("changed")
	r, _ = structuredFile(context.Background(), w, "read_page", args{Path: "f", ExpectedSHA256: e.Evidence[0].SHA256}, observe)
	if decodeEnvelope(t, r).Error == nil {
		t.Fatal("changed artifact accepted")
	}
}

func TestSameStepContextGate(t *testing.T) {
	var sessionID string
	steps := 0
	h := orchestrationHarness(t, func(ctx context.Context, model string, emit func(statefile.Record)) (statefile.Header, error) {
		steps++
		if steps == 1 {
			for _, v := range []string{"alpha", "beta"} {
				x := contextflow.Item{ID: v, Layer: contextflow.Evidence, Text: v, Key: "target", Value: v, Sources: []contextflow.Ref{{Session: sessionID, Record: 1}}}
				raw, _ := json.Marshal(contextEvent{Item: &x})
				b, _ := json.Marshal(provider.Call{Name: "context_record", Arguments: raw})
				emit(statefile.Record{Header: statefile.Header{ID: v, Kind: statefile.Call}, Body: b})
			}
			b, _ := json.Marshal(provider.Call{Name: "run", Arguments: json.RawMessage(`{"worker":"missing","code":"mutate"}`)})
			emit(statefile.Record{Header: statefile.Header{ID: "mutation", Kind: statefile.Call}, Body: b})
		}
		return statefile.Header{Usage: &statefile.Usage{Input: 100}}, nil
	})
	sessionID = orchestrationSession(t, h, "", "test", "default")
	if err := h.note(h.session(sessionID), sessionID, statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("work")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Run(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	chain, err := h.Sessions.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range chain {
		if r.Kind == statefile.Result && r.For == "mutation" {
			found = strings.Contains(string(r.Body), "clarification_required")
		}
	}
	if !found {
		t.Fatal("same-step conflicting context did not gate mutation")
	}
}
