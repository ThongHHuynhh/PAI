package harness

import (
	"bytes"
	"context"
	"testing"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// A maintenance record must never change what a provider is sent, on this
// build or on an older one that does not know the kind: a server rolled back
// past the version that wrote it must keep its sessions and their cache.
func TestMaintenanceRecordLeavesEveryRequestUnchanged(t *testing.T) {
	p := provider.Anthropic("", "")
	p.Run = func(ctx context.Context, body []byte, emit func(statefile.Record)) (statefile.Header, error) {
		emit(statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain"}, Body: []byte("done")})
		return statefile.Header{Stop: "end_turn", Usage: &statefile.Usage{Input: 100}}, nil
	}
	h := &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}, Workers: NewPool(), Providers: map[string]provider.Provider{"fake": p}}
	id, err := h.Create(context.Background(), Create{Model: "fake/m", ContextLimit: 100000, Tools: "default"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	msg := func(text string) statefile.Record {
		return statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte(text)}
	}
	s := h.session(id)
	if err := h.note(s, id, msg("first task")); err != nil {
		t.Fatal(err)
	}
	if st, err := h.Run(context.Background(), id); err != nil || st != statefile.StatusClosed {
		t.Fatalf("run: %s %v", st, err)
	}
	// The kind is spelled out, so this test means the same on a build that
	// has no constant for it.
	bg := statefile.Record{Header: statefile.Header{Kind: "maintenance", Type: "text/plain", Source: "extract", Model: "fake/m", Usage: &statefile.Usage{Input: 900, Output: 40}}}
	if err := h.note(s, id, bg); err != nil {
		t.Fatal(err)
	}
	if err := h.note(s, id, msg("second task")); err != nil {
		t.Fatal(err)
	}
	with, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var without []statefile.Record
	for _, r := range with {
		if r.Kind != "maintenance" {
			without = append(without, r)
		}
	}
	if len(without) != len(with)-1 {
		t.Fatalf("the maintenance record is not in the file")
	}
	if st := statefile.Derive(with); st != statefile.StatusOpen {
		t.Fatalf("status with the record is %s, want open", st)
	}
	conf, err := config(with)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, e := range []provider.Provider{provider.Anthropic("", ""), provider.OpenAI("", ""), provider.OpenAIChat("", ""), provider.ChatGPT("", dir), provider.ClaudeCode(dir)} {
		compile := func(chain []statefile.Record) []byte {
			records, _ := statefile.Build(chain)
			records = clock(records, chain[0].Time, conf.ContextLimit, conf.Input)
			body, _, err := e.Envelope.Compile(provider.Request{Model: "m", Key: id, Tools: sessionTools(conf), Context: records, Params: conf.Params})
			if err != nil {
				t.Fatalf("%s: %v", e.Envelope.Name(), err)
			}
			return body
		}
		if a, b := compile(with), compile(without); !bytes.Equal(a, b) {
			t.Errorf("%s: the maintenance record changed the request\nwith:    %s\nwithout: %s", e.Envelope.Name(), a, b)
		}
	}
}
