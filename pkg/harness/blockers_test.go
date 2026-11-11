package harness

import (
	"context"
	"encoding/json"
	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
	"pai/pkg/worker"
	"strings"
	"testing"
)

func TestArchiveIsBoundedAndExpandable(t *testing.T) {
	original := strings.Repeat("untrusted tool output ", 100000)
	chain := []statefile.Record{{Header: statefile.Header{N: 1, Kind: statefile.Result, Type: "text/plain"}, Body: []byte(original)}, {Header: statefile.Header{N: 2, Kind: statefile.Compaction, Upto: 1}, Body: []byte("summary")}}
	archive := archiveIndex(1)
	if len(archive) > 1024 || strings.Contains(archive, "untrusted tool output") {
		t.Fatal("compaction reinjected source bodies")
	}
	h := &Harness{}
	r := h.contextCall(context.Background(), "s", statefile.Conf{}, statefile.Record{}, provider.Call{Name: "context_expand", Arguments: json.RawMessage(`{"record":1}`)}, chain, func(statefile.Record) {})
	var page struct {
		Text      string
		Next      int `json:"next_offset"`
		Truncated bool
		Trust     string
	}
	if err := json.Unmarshal(r.Body, &page); err != nil {
		t.Fatal(err)
	}
	if page.Text != original[:4096] || page.Next != 4096 || !page.Truncated || page.Trust == "" {
		t.Fatal("bad archive expansion")
	}
}

func TestExecutionMetadataNeverEntersContext(t *testing.T) {
	chain := []statefile.Record{{Header: statefile.Header{Kind: statefile.Start, N: 1}}, {Header: statefile.Header{Kind: statefile.Execution, N: 2}, Body: []byte("private wire data")}}
	built, _ := statefile.Build(chain)
	if len(built) != 0 || statefile.Derive(chain) != statefile.StatusClosed {
		t.Fatal("execution metadata leaked or changed status")
	}
}

func TestMemoryResolutionRequiresRealAsk(t *testing.T) {
	ctx := context.Background()
	h := orchestrationHarness(t, func(context.Context, string, func(statefile.Record)) (statefile.Header, error) {
		return statefile.Header{}, nil
	})
	// Use the same deterministic hash embedder as the store's offline tests.
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h.Memory = st
	st.VerifyResolution = h.VerifyMemoryResolution
	id := orchestrationSession(t, h, "", "test", "default")
	a, _, err := st.Remember(ctx, memory.Item{Text: "Staging database port is 5432", Contract: memory.Contract{Subject: "staging", Property: "port", Value: "5432"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := st.Remember(ctx, memory.Item{Text: "Staging database port is 5433", Contract: memory.Contract{Subject: "staging", Property: "port", Value: "5433"}})
	if err != nil {
		t.Fatal(err)
	}
	proof := memory.Resolution{Session: id, Call: "ask-proof", Answer: "Use B"}
	if err := st.ResolveConflict(ctx, a.ID, b.ID, proof); err == nil {
		t.Fatal("fabricated answer accepted")
	}
	if err := h.rememberRefs(id, []memory.Item{a}); err != nil {
		t.Fatal(err)
	}
	refs, _ := h.Sessions.Load(id)
	if !h.memoryBlocked(ctx, refs, "run") || h.memoryBlocked(ctx, refs, "memory_clarify") {
		t.Fatal("memory conflict failed to gate mutation")
	}
	if h.memoryBlocked(ctx, nil, "run") {
		t.Fatal("unrelated task blocked")
	}
	q, err := h.memoryQuestion(ctx, a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"questions": []Question{q}})
	body, _ := json.Marshal(provider.Call{Name: "ask", Arguments: args})
	if err := h.note(h.session(id), id, statefile.Record{Header: statefile.Header{Kind: statefile.Call, ID: proof.Call}, Body: body}); err != nil {
		t.Fatal(err)
	}
	if err := h.note(h.session(id), id, result(proof.Call, answered([]Question{q}, Reply{Declined: true}))); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolveConflict(ctx, a.ID, b.ID, proof); err == nil {
		t.Fatal("decline accepted")
	}
	// A separate exact ask with an actual answer can authorize clarification.
	proof.Call = "ask-proof2"
	if err := h.note(h.session(id), id, statefile.Record{Header: statefile.Header{Kind: statefile.Call, ID: proof.Call}, Body: body}); err != nil {
		t.Fatal(err)
	}
	if err := h.note(h.session(id), id, result(proof.Call, answered([]Question{q}, Reply{Answers: [][]string{{"Use B"}}}))); err != nil {
		t.Fatal(err)
	}
	if err := st.ResolveConflict(ctx, a.ID, b.ID, proof); err != nil {
		t.Fatal(err)
	}
	x, _ := st.Get(ctx, a.ID)
	y, _ := st.Get(ctx, b.ID)
	if !x.Invalid.IsZero() || !y.Invalid.IsZero() || x.Text != a.Text || y.Text != b.Text {
		t.Fatal("clarification erased evidence")
	}
}

func TestDurableLateResultDeduplicatedAndStatusVisible(t *testing.T) {
	ctx := context.Background()
	h := orchestrationHarness(t, func(context.Context, string, func(statefile.Record)) (statefile.Header, error) {
		return statefile.Header{}, nil
	})
	id := orchestrationSession(t, h, "", "test", "default")
	origin := statefile.NewID()
	attempt := statefile.NewID()
	created := worker.Frame{Header: worker.Header{ID: attempt, Job: "job", Session: id, Origin: origin, Kind: worker.Run}}
	if err := h.executionRecord(id, origin, executionEvent{State: "created", Worker: "test", Frame: created}); err != nil {
		t.Fatal(err)
	}
	started := worker.Frame{Header: worker.Header{ID: attempt, Kind: worker.Started, Pid: 99, Path: "job.log"}}
	if err := h.executionRecord(id, origin, executionEvent{State: "started", Worker: "test", Frame: started}); err != nil {
		t.Fatal(err)
	}
	if err := h.executionRecord(id, origin, executionEvent{State: "recorded"}); err != nil {
		t.Fatal(err)
	}
	info, err := h.Info(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Executions) != 1 || !info.Executions[0].Active || !info.Executions[0].Background || info.Executions[0].Log != "job.log" {
		t.Fatalf("invisible background %+v", info.Executions)
	}
	terminal := worker.Frame{Header: worker.Header{ID: attempt, Session: id, Origin: origin, Kind: worker.Result}, Body: []byte("private output")}
	if err := h.ReconcileResult("test", terminal); err != nil {
		t.Fatal(err)
	}
	before, _ := h.Sessions.Load(id)
	if err := h.ReconcileResult("test", terminal); err != nil {
		t.Fatal(err)
	}
	after, _ := h.Sessions.Load(id)
	if len(after) != len(before) {
		t.Fatal("duplicate terminal persisted twice")
	}
	info, err = h.Info(id)
	if err != nil || info.Executions[0].Active {
		t.Fatal("terminal still running", err)
	}
	data, _ := json.Marshal(info.Executions)
	if strings.Contains(string(data), "private output") {
		t.Fatal("status leaked body")
	}
	_ = ctx
}
