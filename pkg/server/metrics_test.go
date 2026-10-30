package server

import (
	"math"
	"testing"

	"pai/pkg/catalog"
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
