package harness

import (
	"strings"
	"testing"

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
