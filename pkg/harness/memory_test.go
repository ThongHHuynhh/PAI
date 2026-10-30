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
	h.apply(ctx, memory.Item{Session: "s1", Source: "extract"}, ops, fresh)
	valid, _ := st.List(ctx, memory.Filter{})
	var texts []string
	for _, it := range valid {
		texts = append(texts, it.Text)
	}
	got := strings.Join(texts, "|")
	for _, want := range []string{"deploys through GitHub Actions", "editor is Helix", "DNS for the user's domains"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, gone := range []string{"Sublime", "thanks", "Production runs on the VPS named atlas"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q should not be valid: %s", gone, got)
		}
	}
	if len(valid) != 3 {
		t.Fatalf("want 3 valid items, got %d: %s", len(valid), got)
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
	if !provider.Exhausted(errors.New("claude: You've hit your usage limit")) || provider.Exhausted(errors.New("bad request: unknown field")) {
		t.Fatal("Exhausted misreads")
	}
}
