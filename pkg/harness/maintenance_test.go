package harness

import (
	"context"
	"errors"
	"testing"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// Every call the harness makes for a session outside its loop is counted in
// that session: its usage shows apart from the steps', a failed call says
// why, and none of it reorders the session list or nears the next extraction.
func TestMaintenanceCallsCountInTheirSession(t *testing.T) {
	var fail error
	p := provider.Anthropic("", "")
	p.Run = func(ctx context.Context, body []byte, emit func(statefile.Record)) (statefile.Header, error) {
		if fail != nil {
			return statefile.Header{}, fail
		}
		emit(statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain"}, Body: []byte("done")})
		return statefile.Header{Stop: "end_turn", Usage: &statefile.Usage{Input: 100, Output: 7, CacheRead: 50}}, nil
	}
	h := &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}, Workers: NewPool(), Providers: map[string]provider.Provider{"fake": p}}
	ctx := context.Background()
	id, err := h.Create(ctx, Create{Model: "fake/m", ContextLimit: 100000, Tools: "default"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.note(h.session(id), id, statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	if st, err := h.Run(ctx, id); err != nil || st != statefile.StatusClosed {
		t.Fatalf("run: %s %v", st, err)
	}
	before, err := h.Info(id)
	if err != nil {
		t.Fatal(err)
	}
	conf := before.Config

	if out, err := h.complete(ctx, conf, id, "extract", "sys", "text"); err != nil || out != "done" {
		t.Fatalf("complete: %q %v", out, err)
	}
	fail = errors.New("provider down")
	if _, err := h.complete(ctx, conf, id, "grade", "sys", "text"); err == nil {
		t.Fatal("a failed call returned no error")
	}

	chain, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var got []statefile.Record
	for _, r := range chain {
		if r.Kind == statefile.Maintenance {
			got = append(got, r)
		}
	}
	if len(got) != 2 {
		t.Fatalf("%d maintenance records, want 2", len(got))
	}
	if r := got[0]; r.Source != "extract" || r.Model != "fake/m" || r.Usage == nil || r.Usage.Input != 100 || r.Usage.CacheRead != 50 || r.Usage.Output != 7 || len(r.Body) != 0 {
		t.Fatalf("extract record: %+v %q", r.Header, r.Body)
	}
	if r := got[1]; r.Source != "grade" || string(r.Body) != "provider down" {
		t.Fatalf("failed record: %+v %q", r.Header, r.Body)
	}

	after, err := h.Info(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Usage != before.Usage {
		t.Fatalf("step usage changed: %+v -> %+v", before.Usage, after.Usage)
	}
	if want := (statefile.Usage{Input: 100, Output: 7, CacheRead: 50}); after.Background != want {
		t.Fatalf("background %+v, want %+v", after.Background, want)
	}
	if !after.Updated.Equal(before.Updated) {
		t.Fatalf("updated moved %s -> %s", before.Updated, after.Updated)
	}
	if after.Status != statefile.StatusClosed {
		t.Fatalf("status %s, want closed", after.Status)
	}
	if n := gathered(chain, 0); n != len(chain)-2 {
		t.Fatalf("gathered %d of %d records", n, len(chain))
	}
}

// A failed session gets no maintenance record: a release before the kind
// would read it as open and step it again on restart. The call still runs.
func TestNoMaintenanceRecordAfterAFailure(t *testing.T) {
	p := provider.Anthropic("", "")
	p.Run = func(ctx context.Context, body []byte, emit func(statefile.Record)) (statefile.Header, error) {
		emit(statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain"}, Body: []byte("summary")})
		return statefile.Header{Stop: "end_turn", Usage: &statefile.Usage{Input: 10}}, nil
	}
	h := &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}, Workers: NewPool(), Providers: map[string]provider.Provider{"fake": p}}
	ctx := context.Background()
	id, err := h.Create(ctx, Create{Model: "fake/m", ContextLimit: 100000, Tools: "default"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := h.session(id)
	if err := h.note(s, id, statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := h.note(s, id, errorRecord("test", "stopped")); err != nil {
		t.Fatal(err)
	}
	before, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := config(before)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := h.complete(ctx, conf, id, "extract", "sys", "text"); err != nil || out != "summary" {
		t.Fatalf("complete: %q %v", out, err)
	}
	after, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a failed session got %d new records", len(after)-len(before))
	}
}

// A maintenance record after an error leaves the session failed: status
// reads past it.
func TestMaintenanceRecordKeepsAFailedSessionFailed(t *testing.T) {
	chain := []statefile.Record{
		{Header: statefile.Header{N: 1, Kind: statefile.Start}},
		{Header: statefile.Header{N: 2, Kind: statefile.Config}},
		{Header: statefile.Header{N: 3, Kind: statefile.Message}},
		{Header: statefile.Header{N: 4, Kind: statefile.Error}},
		{Header: statefile.Header{N: 5, Kind: statefile.Maintenance, Source: "compact"}},
	}
	if st := statefile.Derive(chain); st != statefile.StatusFailed {
		t.Fatalf("status %s, want failed", st)
	}
}

// A background call that lands after a person's message, or fails with a
// long error, changes neither whether nor where compaction cuts.
func TestMaintenanceRecordLeavesCompactionAsItWas(t *testing.T) {
	rec := func(n int, k statefile.Kind, via string, body int) statefile.Record {
		return statefile.Record{Header: statefile.Header{N: n, Kind: k, Via: via, ID: statefile.NewID()}, Body: make([]byte, body)}
	}
	chain := []statefile.Record{rec(1, statefile.Start, "", 0), rec(2, statefile.Config, "", 0), rec(3, statefile.System, "", 100)}
	for n := 4; n < 40; n += 2 {
		chain = append(chain, rec(n, statefile.Message, "test", 400), rec(n+1, statefile.Step, "", 0))
	}
	chain = append(chain, rec(40, statefile.Message, "test", 400))
	bg := rec(41, statefile.Maintenance, "", 0)
	bg.Source, bg.Body = "extract", make([]byte, 6000)
	with := append(append([]statefile.Record{}, chain...), bg)
	if !newRequest(with) {
		t.Error("a background record after the message hid the new request")
	}
	if a, b := cutPoint(chain, 3000), cutPoint(with, 3000); a != b {
		t.Errorf("cut moved from %d to %d", a, b)
	}
}
