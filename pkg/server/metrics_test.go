package server

import (
	"context"
	"math"
	"testing"
	"time"

	"pai/pkg/catalog"
	"pai/pkg/harness"
	"pai/pkg/statefile"
)

func TestPct(t *testing.T) {
	if got := pct(nil, 50); got != 0 {
		t.Errorf("pct of none = %d", got)
	}
	ls := []int64{900, 100, 500, 300, 700, 200, 800, 400, 600, 1000}
	if p50, p95 := pct(ls, 50), pct(ls, 95); p50 != 600 || p95 != 1000 {
		t.Errorf("p50 %d p95 %d", p50, p95)
	}
}

func TestPrice(t *testing.T) {
	cat := catalog.Catalog{Presets: map[string]catalog.Preset{
		"claude-code/claude-sonnet-5-5": {Model: "claude-sonnet-5-5"},
		"anthropic/claude-sonnet-5-5":   {Model: "claude-sonnet-5-5", Cost: catalog.Cost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}},
		"ollama/qwen3":                  {Model: "qwen3"},
	}}
	// a subscription is priced as the API would have charged
	p, ok := price(cat, "claude-code/claude-sonnet-5-5")
	if !ok || p.Model != "anthropic/claude-sonnet-5-5" {
		t.Fatalf("subscription price = %+v, %v", p, ok)
	}
	if p, ok := price(cat, "anthropic/claude-sonnet-5-5"); !ok || p.Model != "anthropic/claude-sonnet-5-5" {
		t.Errorf("list price = %+v, %v", p, ok)
	}
	if _, ok := price(cat, "ollama/qwen3"); ok {
		t.Error("a model with no price anywhere was priced")
	}
	usd := cost(p, &statefile.Usage{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheWrite: 0})
	if want := 3 + 1.5 + 0.6; math.Abs(usd-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", usd, want)
	}
}

// Background calls are counted apart from the agent loop's steps: Totals
// keeps its meaning, and every background call shows by purpose.
func TestMetricsCountBackgroundCallsApart(t *testing.T) {
	h := &harness.Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}}
	id := "8e487b84"
	f, err := h.Sessions.Start(id, []byte(`{"model":"anthropic/claude-sonnet-5-5","tools":"default","workers":[],"context_limit":100000}`))
	if err != nil {
		t.Fatal(err)
	}
	u := func(in, out int) *statefile.Usage { return &statefile.Usage{Input: in, Output: out} }
	for _, r := range []statefile.Record{
		{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("hi")},
		{Header: statefile.Header{Kind: statefile.Step, Model: "claude-sonnet-5-5", Usage: u(1000, 100), Stop: "end_turn"}},
		{Header: statefile.Header{Kind: statefile.Maintenance, Type: "text/plain", Source: "extract", Model: "anthropic/claude-sonnet-5-5", Usage: u(400, 40)}},
		{Header: statefile.Header{Kind: statefile.Maintenance, Type: "text/plain", Source: "grade", Model: "anthropic/claude-sonnet-5-5"}, Body: []byte("provider down")},
		{Header: statefile.Header{Kind: statefile.Verification, Type: "application/json", Model: "anthropic/claude-sonnet-5-5"}, Body: []byte(`{"verdict":"started"}`)},
		{Header: statefile.Header{Kind: statefile.Verification, Type: "application/json", Model: "anthropic/claude-sonnet-5-5", Usage: u(200, 20)}, Body: []byte(`{"verdict":"accept"}`)},
		{Header: statefile.Header{Kind: statefile.Verification, Type: "application/json", Model: "anthropic/claude-sonnet-5-5"}, Body: []byte(`{"verdict":"started"}`)},
		{Header: statefile.Header{Kind: statefile.Verification, Type: "application/json", Model: "anthropic/claude-sonnet-5-5"}, Body: []byte(`{"verdict":"uncertain","reason":"verifier failed: down"}`)},
	} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Info(id); err != nil { // puts it in the index the list serves
		t.Fatal(err)
	}
	m, err := (&webAPI{h: h}).collect(context.Background(), "all", 0, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Totals.Steps != 1 || m.Totals.Usage != *u(1000, 100) {
		t.Fatalf("totals %+v", m.Totals)
	}
	bg := m.Background
	if bg.Calls != 4 || bg.Errors != 2 || bg.Usage != *u(600, 60) {
		t.Fatalf("background %+v", bg.Use)
	}
	for purpose, want := range map[string]Use{
		"extract": {Calls: 1, Usage: *u(400, 40)},
		"grade":   {Calls: 1, Errors: 1},
		"verify":  {Calls: 2, Errors: 1, Usage: *u(200, 20)},
	} {
		got := bg.Purposes[purpose]
		if got == nil || got.Calls != want.Calls || got.Errors != want.Errors || got.Usage != want.Usage {
			t.Errorf("%s: %+v, want %+v", purpose, got, want)
		}
	}
	if len(m.Sessions) != 1 || m.Sessions[0].Background != *u(600, 60) || m.Sessions[0].Usage != *u(1000, 100) {
		t.Fatalf("sessions %+v", m.Sessions)
	}
}
