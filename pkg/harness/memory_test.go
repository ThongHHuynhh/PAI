package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

func r(n int, k statefile.Kind, body string) statefile.Record {
	return statefile.Record{Header: statefile.Header{N: n, Kind: k, ID: statefile.NewID()}, Body: []byte(body)}
}

type unavailableEmbedder struct{ fail bool }

func (e *unavailableEmbedder) Name() string { return "test-unavailable" }
func (e *unavailableEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.fail {
		return nil, errors.New("embedding endpoint offline")
	}
	return (memory.Hash{}).Embed(ctx, texts)
}

func TestRecallFailureIsRecorded(t *testing.T) {
	ctx, embed := context.Background(), &unavailableEmbedder{}
	st, err := memory.Open(t.TempDir(), embed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, _, err := st.Remember(ctx, memory.Item{Text: "The staging database uses port 5433"}); err != nil {
		t.Fatal(err)
	}
	embed.fail = true
	h := &Harness{Memory: st, Sessions: statefile.Sessions{Dir: t.TempDir()}}
	f, err := h.Sessions.Start("s1", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	msg := statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("Which port does the staging database use?")}
	if err := f.Append(msg); err != nil {
		t.Fatal(err)
	}
	chain, err := h.Sessions.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	s := &session{id: "s1", f: f, changed: make(chan struct{})}
	if err := h.recallTurn(ctx, s, "s1", statefile.Conf{}, chain); err != nil {
		t.Fatal(err)
	}
	chain, err = h.Sessions.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	last := chain[len(chain)-1]
	if last.Source != "memory-error" || !strings.Contains(string(last.Body), "unavailable") {
		t.Fatalf("failure not recorded: %+v", last)
	}
}

// The cut never falls between a call and its result, and keeps the tail.
func TestCutPointKeepsCallsWhole(t *testing.T) {
	big := strings.Repeat("x", 1000)
	call := r(5, statefile.Call, `{"name":"run"}`)
	res := r(7, statefile.Result, big)
	res.For = call.ID
	chain := []statefile.Record{
		r(1, statefile.Start, ""), r(2, statefile.System, big),
		r(3, statefile.Message, big), r(4, statefile.Text, big), call, r(6, statefile.Step, ""), res,
		r(8, statefile.Message, big), r(9, statefile.Text, big), r(10, statefile.Step, ""),
	}
	got := cutPoint(chain, 1500)
	if got == 5 || got == 6 {
		t.Fatalf("cut %d splits a call from its result", got)
	}
	if got != 7 {
		t.Fatalf("want the cut after the result, 7; got %d", got)
	}
}

// apply runs a model's operations, and stores what no operation names.
func TestApplyOps(t *testing.T) {
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h := &Harness{Memory: st}
	ctx := context.Background()
	old, _, _ := st.Remember(ctx, memory.Item{Text: "Production runs on the VPS named atlas", Topic: "deploy", Source: "test"})
	wrong, _, _ := st.Remember(ctx, memory.Item{Text: "The user's editor is Sublime Text", Topic: "editor", Source: "test"})
	fresh := []candidate{
		{Kind: "fact", Topic: "deploy", Text: "Production deploys run through GitHub Actions"},
		{Kind: "fact", Topic: "chat", Text: "The user said thanks for the help"},
		{Kind: "preference", Topic: "editor", Text: "The user's editor is Helix"},
		{Kind: "fact", Topic: "dns", Text: "DNS for the user's domains is on Cloudflare"},
	}
	ops := []op{
		{Op: "merge", IDs: []string{old.ID}, New: json.RawMessage(`[0]`), Text: "Production runs on the VPS atlas and deploys through GitHub Actions"},
		{Op: "skip", New: json.RawMessage(`1`), Reason: "chatter"},
		{Op: "retire", ID: wrong.ID, Reason: "the user moved to Helix"},
		{Op: "add", New: json.RawMessage(`2`)},
	}
	h.apply(ctx, memory.Item{Session: "s1", Source: "extract"}, ops, fresh, []memory.Item{old, wrong})
	valid, _ := st.List(ctx, memory.Filter{})
	var texts []string
	for _, it := range valid {
		texts = append(texts, it.Text)
		if len(it.Conflicts) > 0 {
			t.Errorf("apply flagged a conflict nobody asked for: %s", it.Text)
		}
	}
	got := strings.Join(texts, "|")
	// merged, added, and stored though no op named it
	for _, want := range []string{"Production runs on the VPS atlas and deploys through GitHub Actions", "editor is Helix", "DNS for the user's domains"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, gone := range []string{"Sublime", "thanks", "VPS named atlas"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q should be retired, merged or skipped: %s", gone, got)
		}
	}
	if len(valid) != 3 {
		t.Fatalf("want 3 valid items, got %d: %s", len(valid), got)
	}
	// Every change is reversible history.
	retired, err := st.Get(ctx, wrong.ID)
	if err != nil || retired.Invalid.IsZero() || !strings.Contains(retired.Reason, "moved to Helix") {
		t.Fatalf("retire: %+v %v", retired, err)
	}
	if back, err := st.Restore(ctx, wrong.ID); err != nil || !back.Invalid.IsZero() {
		t.Fatalf("restore: %v", err)
	}
	merged, _ := st.Get(ctx, old.ID)
	if !strings.HasPrefix(merged.Reason, memory.MergedInto) {
		t.Fatalf("merge lost lineage: %q", merged.Reason)
	}
}

// A retire of one item, or an op naming the same id twice, never makes an
// item conflict with itself: the v0.4.2 bug that blocked every run call.
func TestApplyNeverSelfConflicts(t *testing.T) {
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h := &Harness{Memory: st}
	ctx := context.Background()
	a, _, _ := st.Remember(ctx, memory.Item{Text: "pai-server on jetson1 runs v0.3.1", Topic: "pai", Source: "test"})
	b, _, _ := st.Remember(ctx, memory.Item{Text: "The worker abi-3957 runs Windows", Topic: "pai", Source: "test"})
	ops := []op{
		{Op: "conflict", IDs: []string{b.ID, b.ID}, Reason: "same id twice"},
		{Op: "conflict", ID: b.ID, Reason: "one id"},
		{Op: "retire", ID: a.ID, IDs: []string{a.ID}, Reason: "now v0.4.2"},
		{Op: "retire", ID: "mNOTSHOWN", Reason: "an id the model was not shown"},
	}
	h.apply(ctx, memory.Item{Source: "consolidation"}, ops, nil, []memory.Item{a, b})
	for _, id := range []string{a.ID, b.ID} {
		it, _ := st.Get(ctx, id)
		if len(it.Conflicts) > 0 {
			t.Fatalf("%s conflicts with %v", id, it.Conflicts)
		}
	}
	if it, _ := st.Get(ctx, a.ID); it.Invalid.IsZero() {
		t.Fatal("retire did not retire")
	}
	if err := st.FlagConflict(ctx, []string{b.ID, b.ID}, "self"); err == nil {
		t.Fatal("FlagConflict accepted an item against itself")
	}
}

// A pinned item stays as the person pinned it, whatever the model proposes.
func TestApplyLeavesPinned(t *testing.T) {
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h := &Harness{Memory: st}
	ctx := context.Background()
	p, _, _ := st.Remember(ctx, memory.Item{Text: "Always answer in plain English", Kind: memory.Preference, Source: "person"})
	p, _ = st.Pin(ctx, p.ID, true)
	h.apply(ctx, memory.Item{Source: "consolidation"}, []op{{Op: "retire", ID: p.ID, Reason: "x"}, {Op: "update", ID: p.ID, Text: "Answer in German"}}, nil, []memory.Item{p})
	if it, _ := st.Get(ctx, p.ID); !it.Invalid.IsZero() {
		t.Fatal("a pinned item was changed")
	}
}

// Recall carries the projects a session names, skips what its context
// already holds, and lets a session forget a project's item.
func TestRecallScopesAndRepeats(t *testing.T) {
	ctx := context.Background()
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	proj, _, _ := st.Remember(ctx, memory.Item{Text: "The PAI server listens on port 8080 on jetson1", Scope: "project:PAI", Source: "test"})
	if proj.Scope != "project:pai" {
		t.Fatalf("scope not normalized: %s", proj.Scope)
	}
	other, _, _ := st.Remember(ctx, memory.Item{Text: "The RoboCore server listens on port 8080 on cell4", Scope: "project:robocore", Source: "test"})
	h := orchestrationHarness(t, func(context.Context, string, func(statefile.Record)) (statefile.Header, error) {
		return statefile.Header{}, nil
	})
	h.Memory = st
	id := orchestrationSession(t, h, "", "test", "default")
	msg := statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("Which port does the PAI server listen on?")}
	if err := h.note(h.session(id), id, msg); err != nil {
		t.Fatal(err)
	}
	chain, _ := h.Sessions.Load(id)
	if got := h.sessionScopes(ctx, chain); len(got) != 1 || got[0] != "project:pai" {
		t.Fatalf("scopes %v", got)
	}
	call := func(name, args string) string {
		c := provider.Call{Name: name, Arguments: json.RawMessage(args)}
		body, _ := json.Marshal(c)
		return string(h.memoryCall(ctx, id, statefile.Record{Header: statefile.Header{ID: statefile.NewID()}, Body: body}, c).Body)
	}
	got := call("recall", `{"query":"server port 8080","k":5}`)
	if !strings.Contains(got, proj.ID) || strings.Contains(got, other.ID) {
		t.Fatalf("recall: %s", got)
	}
	if again := call("recall", `{"query":"server port 8080","k":5}`); strings.Contains(again, "- ["+proj.ID) || !strings.Contains(again, "already in your context: "+proj.ID) {
		t.Fatalf("an item in context was recalled again: %s", again)
	}
	if got := call("forget", `{"id":"`+proj.ID+`","reason":"moved"}`); !strings.HasPrefix(got, "forgot") {
		t.Fatalf("a project item could not be forgotten: %s", got)
	}
}

func TestMentions(t *testing.T) {
	for _, c := range []struct {
		text, name string
		want       bool
	}{{"what is pai's status", "pai", true}, {"was it paid", "pai", false}, {"abi-twin export", "abi-twin", true}, {"pai", "pai", true}} {
		if got := mentions(c.text, c.name); got != c.want {
			t.Errorf("mentions(%q, %q) = %v", c.text, c.name, got)
		}
	}
}

// A clarification finds the person's answer in the session by itself, since
// a model may not see its own call ids, and retires the item not chosen.
func TestClarifyFindsAnswer(t *testing.T) {
	ctx := context.Background()
	h := orchestrationHarness(t, func(context.Context, string, func(statefile.Record)) (statefile.Header, error) {
		return statefile.Header{}, nil
	})
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h.Memory = st
	st.VerifyResolution = h.VerifyMemoryResolution
	id := orchestrationSession(t, h, "", "test", "default")
	a, _, _ := st.Remember(ctx, memory.Item{Text: "Staging database port is 5432", Contract: memory.Contract{Subject: "staging", Property: "port", Value: "5432"}})
	b, _, _ := st.Remember(ctx, memory.Item{Text: "Staging database port is 5433", Contract: memory.Contract{Subject: "staging", Property: "port", Value: "5433"}})
	clarify := func() string {
		args := json.RawMessage(`{"id":"` + a.ID + `","peer":"` + b.ID + `"}`)
		return string(h.clarifyMemory(ctx, id, statefile.Record{Header: statefile.Header{ID: statefile.NewID()}}, provider.Call{Name: "memory_clarify", Arguments: args}).Body)
	}
	if got := clarify(); !strings.Contains(got, "call ask") {
		t.Fatalf("first call should hand out the question: %s", got)
	}
	q, _ := h.memoryQuestion(ctx, a.ID, b.ID)
	args, _ := json.Marshal(map[string]any{"questions": []Question{q}})
	body, _ := json.Marshal(provider.Call{Name: "ask", Arguments: args})
	askID := "toolu_hidden"
	if err := h.note(h.session(id), id, statefile.Record{Header: statefile.Header{Kind: statefile.Call, ID: askID}, Body: body}); err != nil {
		t.Fatal(err)
	}
	if err := h.note(h.session(id), id, result(askID, answered([]Question{q}, Reply{Answers: [][]string{{"Use B"}}}))); err != nil {
		t.Fatal(err)
	}
	if got := clarify(); !strings.Contains(got, b.ID+" holds") {
		t.Fatalf("answer not applied: %s", got)
	}
	x, _ := st.Get(ctx, a.ID)
	y, _ := st.Get(ctx, b.ID)
	if x.Invalid.IsZero() || !y.Invalid.IsZero() || len(y.Conflicts) != 0 {
		t.Fatalf("want A retired, B valid and undisputed: %+v %+v", x, y)
	}
}

// A session out of usage moves along the fallback chain, skips what it
// tried since the last message, and stops at the chain's end.
func TestFallbackChain(t *testing.T) {
	h := &Harness{
		Providers: map[string]provider.Provider{"cc1": {}, "cc2": {}, "api": {}},
		Fallbacks: func() map[string]string {
			return map[string]string{"cc1": "cc2", "cc2": "gone", "gone": "api", "api": "cc1"}
		},
	}
	msg := r(3, statefile.Message, "hi")
	if got := h.fallback("cc1/claude-sonnet-5-5", []statefile.Record{msg}); got != "cc2/claude-sonnet-5-5" {
		t.Fatalf("first fallback: %q", got)
	}
	moved := r(4, statefile.System, "cc1 hit its limit")
	moved.Source = "failover"
	if got := h.fallback("cc2/claude-sonnet-5-5", []statefile.Record{msg, moved}); got != "api/claude-sonnet-5-5" {
		t.Fatalf("second fallback should skip the provider not served: %q", got)
	}
	moved2 := r(5, statefile.System, "cc2 hit its limit")
	moved2.Source = "failover"
	if got := h.fallback("api/claude-sonnet-5-5", []statefile.Record{msg, moved, moved2}); got != "" {
		t.Fatalf("the chain came back round to a tried account: %q", got)
	}
	if !provider.Exhausted(errors.New("claude: You've hit your usage limit")) || !provider.Exhausted(errors.New("claude-code: success: You've hit your session limit · resets 12:30am")) || provider.Exhausted(errors.New("bad request: unknown field")) {
		t.Fatal("Exhausted misreads")
	}
}
