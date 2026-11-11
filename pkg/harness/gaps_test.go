package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

func stepAt(used int) statefile.Record {
	return statefile.Record{Header: statefile.Header{Kind: statefile.Step, Usage: &statefile.Usage{Input: used}}}
}

func TestCompactionModes(t *testing.T) {
	h := &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}}
	f, err := h.Sessions.Start("s1", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	s := h.session("s1")
	msg := statefile.Record{Header: statefile.Header{Kind: statefile.Message, Via: "web"}}
	job := statefile.Record{Header: statefile.Header{Kind: statefile.Message, Via: "job"}}
	auto := statefile.Conf{ContextLimit: 1000}
	manual := statefile.Conf{ContextLimit: 1000, Compact: statefile.CompactManual}
	cases := []struct {
		name  string
		conf  statefile.Conf
		chain []statefile.Record
		want  bool
	}{
		{"auto under", auto, []statefile.Record{stepAt(400), msg}, false},
		{"auto new request past half", auto, []statefile.Record{stepAt(600), msg}, true},
		{"auto job past half", auto, []statefile.Record{stepAt(600), job}, false},
		{"auto full", auto, []statefile.Record{stepAt(750)}, true},
		{"manual full asks", manual, []statefile.Record{stepAt(750)}, false},
		{"manual near limit", manual, []statefile.Record{stepAt(950)}, true},
	}
	for _, c := range cases {
		if got := h.compactDue(s, "s1", c.conf, c.chain); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
	chain, _ := h.Sessions.Load("s1")
	notices := 0
	for _, r := range chain {
		if r.Kind == statefile.System && r.Source == "compact" {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("manual mode wrote %d notices, want 1", notices)
	}
	if !asked(chain) {
		t.Fatal("the notice is not seen, so it would repeat every step")
	}
	if _, err := h.SetCompact("s1", "test", "sometimes"); err == nil {
		t.Fatal("a bad mode was accepted")
	}
	if _, err := h.SetCompact("s1", "test", statefile.CompactManual); err != nil {
		t.Fatal(err)
	}
	chain, _ = h.Sessions.Load("s1")
	if conf, _ := config(chain); conf.Compact != statefile.CompactManual {
		t.Fatalf("mode %q", conf.Compact)
	}
}

func TestDiagnoseNamesTheStuckCall(t *testing.T) {
	h := &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}, Workers: &Pool{}}
	f, err := h.Sessions.Start("s1", []byte(`{"workers":[{"name":"gone"}],"context_limit":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(provider.Call{Name: "run", Arguments: json.RawMessage(`{"worker":"gone","code":"sleep 1"}`)})
	for _, r := range []statefile.Record{
		{Header: statefile.Header{Kind: statefile.Call, ID: "c1", Step: "st1"}, Body: body},
		{Header: statefile.Header{Kind: statefile.Step, ID: "st1", Usage: &statefile.Usage{}}},
	} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	d, err := h.Diagnose("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Calls) != 1 || d.Calls[0].Worker != "gone" || !strings.Contains(d.Verdict, "not connected") {
		t.Fatalf("diagnosis %+v", d)
	}
}

func TestCredentialPlaceholdersFilledAndMasked(t *testing.T) {
	ctx := context.Background()
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	const pw = "s3cret-value-123"
	if err := st.SaveCredential(ctx, memory.Credential{Name: "db", Description: "database password", Value: pw}); err != nil {
		t.Fatal(err)
	}
	h := &Harness{Memory: st}
	a := args{Code: "psql --password={{cred:db}}"}
	if err := h.fill(ctx, &a); err != nil || a.Code != "psql --password="+pw {
		t.Fatalf("fill: %q %v", a.Code, err)
	}
	if err := h.fill(ctx, &args{Code: "{{cred:nope}}"}); err == nil {
		t.Fatal("an unknown credential was filled with nothing")
	}
	r := h.mask(ctx, statefile.Record{Header: statefile.Header{Type: "text/plain"}, Body: []byte("echo " + pw)})
	if strings.Contains(string(r.Body), pw) {
		t.Fatal("a value reached the result")
	}
}
