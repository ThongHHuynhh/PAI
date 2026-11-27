package tui

import (
	"math"
	"testing"

	"pai/pkg/catalog"
	"pai/pkg/harness"
	"pai/pkg/statefile"
)

// The session's cost counts the harness's background calls for it, which run
// on the session's model, beside its steps.
func TestCostCountsBackgroundCalls(t *testing.T) {
	price := catalog.Cost{Input: 3, Output: 15}
	s := &session{price: &price, info: harness.Info{
		Usage:      statefile.Usage{Input: 1_000_000},
		Background: statefile.Usage{Input: 1_000_000, Output: 100_000},
	}}
	c, ok := s.cost()
	if want := 3 + 3 + 1.5; !ok || math.Abs(c-want) > 1e-9 {
		t.Fatalf("cost %v %v, want %v", c, ok, want)
	}
}
