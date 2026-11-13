package harness

import (
	"context"
	"encoding/json"
	"testing"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

func rec(n int, kind statefile.Kind, id, via string) statefile.Record {
	return statefile.Record{Header: statefile.Header{N: n, Kind: kind, ID: id, Via: via}}
}

// A message the person sent while the step streamed holds its changes; one
// the step saw, or a job's, does not.
func TestSteered(t *testing.T) {
	call := rec(4, statefile.Call, "c1", "")
	call.Step = "s1"
	step := rec(6, statefile.Step, "s1", "")
	step.Upto = 3
	for _, c := range []struct {
		name  string
		chain []statefile.Record
		want  int
	}{
		{"seen", []statefile.Record{rec(3, statefile.Message, "m", "tui"), call, step}, 0},
		{"unseen", []statefile.Record{rec(3, statefile.Message, "m", "tui"), call, rec(5, statefile.Message, "m2", "tui"), step}, 5},
		{"after the step", []statefile.Record{call, step, rec(7, statefile.Message, "m2", "web")}, 7},
		{"a job", []statefile.Record{call, step, rec(7, statefile.Message, "m2", "job")}, 0},
		{"no step yet", []statefile.Record{call, rec(5, statefile.Message, "m2", "tui")}, 0},
	} {
		if got := steered(c.chain, call); got != c.want {
			t.Errorf("%s: steered = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCancelCall(t *testing.T) {
	h := &Harness{}
	if err := h.CancelCall("s", "c1", "tui"); err == nil {
		t.Fatal("cancelled a call that is not running")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	s := h.session("s")
	s.calls = map[string]context.CancelCauseFunc{"c1": cancel}
	if err := h.CancelCall("s", "c1", "tui"); err != nil {
		t.Fatal(err)
	}
	if got := context.Cause(ctx).Error(); got != "cancelled by tui, effect unknown: it may have partly run" {
		t.Fatalf("cause %q", got)
	}
}

// Only the person's newest yes to a switch ask, since the config last
// changed, approves the switch.
func TestApproved(t *testing.T) {
	q := switchQuestion("a/x", "b/y", "hard task")
	args, _ := json.Marshal(map[string]any{"questions": []Question{q}})
	body, _ := json.Marshal(provider.Call{Name: "ask", Arguments: args})
	ask := func(n int, id string) statefile.Record {
		r := rec(n, statefile.Call, id, "")
		r.Body = body
		return r
	}
	answer := func(n int, call, label string) statefile.Record {
		r := rec(n, statefile.Result, "", "")
		r.For, r.Body = call, []byte(answered([]Question{q}, Reply{Answers: [][]string{{label}}}))
		return r
	}
	for _, c := range []struct {
		name  string
		chain []statefile.Record
		want  bool
	}{
		{"never asked", nil, false},
		{"yes", []statefile.Record{ask(1, "k1"), answer(2, "k1", "b/y")}, true},
		{"no", []statefile.Record{ask(1, "k1"), answer(2, "k1", "Keep a/x")}, false},
		{"yes then no", []statefile.Record{ask(1, "k1"), answer(2, "k1", "b/y"), ask(3, "k2"), answer(4, "k2", "Keep a/x")}, false},
		{"yes used by a switch", []statefile.Record{ask(1, "k1"), answer(2, "k1", "b/y"), rec(3, statefile.Config, "", "")}, false},
	} {
		if got := approved(c.chain, "b/y"); got != c.want {
			t.Errorf("%s: approved = %v, want %v", c.name, got, c.want)
		}
	}
}
