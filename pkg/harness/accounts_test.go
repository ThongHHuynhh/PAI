package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// Real catalog entries, fake envelopes and Run functions: no model or login.
type accountEnvelope struct {
	name  string
	media bool
}

func (e accountEnvelope) Name() string          { return e.name }
func (e accountEnvelope) Accepts(t string) bool { return e.media && t == "image/png" }
func (e accountEnvelope) Owns(k string) bool    { return k == "model" }
func (e accountEnvelope) Compile(r provider.Request) ([]byte, json.RawMessage, error) {
	return []byte(r.Model), nil, nil
}
func (e accountEnvelope) Stream(io.Reader, func(statefile.Record)) (statefile.Header, error) {
	panic("Run is used")
}

func accountHarness(t *testing.T) (*Harness, *Balance, *map[string]int) {
	t.Helper()
	b := &Balance{Mode: "off", Pool: []string{"anthropic/claude-sonnet-5-5", "openai/gpt-6-luna"}}
	calls := map[string]int{}
	h := &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}, Workers: &Pool{}, Balance: func() Balance { return *b }}
	ps := map[string]provider.Provider{}
	for _, name := range []string{"anthropic", "openai"} {
		ps[name] = provider.Provider{Envelope: accountEnvelope{name: name, media: name == "anthropic"}, Run: func(ctx context.Context, body []byte, emit func(statefile.Record)) (statefile.Header, error) {
			calls[name]++
			if name == "anthropic" {
				return statefile.Header{}, errors.New("quota exceeded")
			}
			if string(body) != "gpt-6-luna" {
				t.Errorf("wrong target model: %s", body)
			}
			return statefile.Header{Stop: "completed", Usage: &statefile.Usage{Input: 50}}, nil
		}}
	}
	h.SetProviders(ps)
	return h, b, &calls
}

func startAccountSession(t *testing.T, h *Harness, conf statefile.Conf, used int) string {
	t.Helper()
	id := statefile.NewName(h.Sessions.Exists)
	body, _ := json.Marshal(conf)
	f, err := h.Sessions.Start(id, body)
	if err != nil {
		t.Fatal(err)
	}
	if used > 0 {
		if err := f.Append(statefile.Record{Header: statefile.Header{Kind: statefile.Step, Upto: 2, Usage: &statefile.Usage{Input: used}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Append(statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return id
}

func accountConf() statefile.Conf {
	return statefile.Conf{Model: "anthropic/claude-sonnet-5-5", ContextLimit: 1000000, Tools: "default", Input: []string{"image/png"}, Params: map[string]json.RawMessage{"old_vendor_field": json.RawMessage(`true`)}}
}

func TestAccountFailoverAcrossVendors(t *testing.T) {
	h, _, calls := accountHarness(t)
	h.Fallbacks = func() map[string]string { return map[string]string{"anthropic": "openai/gpt-6-luna"} }
	id := startAccountSession(t, h, accountConf(), 1000)
	st, err := h.Step(context.Background(), id)
	if err != nil || st != statefile.StatusOpen {
		t.Fatalf("step: %s %v", st, err)
	}
	chain, _ := h.Sessions.Load(id)
	conf, _ := config(chain)
	if conf.Model != "openai/gpt-6-luna" || conf.ContextLimit != modelCatalog.Presets[conf.Model].ContextLimit {
		t.Fatalf("wrong transition: %+v", conf)
	}
	if _, ok := conf.Params["old_vendor_field"]; ok || len(conf.Input) != 0 {
		t.Fatalf("old vendor config survived: %+v", conf)
	}
	if (*calls)["anthropic"] != 1 || (*calls)["openai"] != 0 {
		t.Fatalf("unexpected calls: %v", *calls)
	}
	if !time.Now().Before(h.account("anthropic").Until) {
		t.Fatal("exhaustion not recorded")
	}
	st, err = h.Step(context.Background(), id)
	if err != nil || st != statefile.StatusClosed || (*calls)["openai"] != 1 {
		t.Fatalf("target step: %s %v %v", st, err, *calls)
	}
}

func TestAccountPreStepAndStickiness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		util      float64
		exhausted bool
		move      bool
	}{
		{"healthy", .4, false, false}, {"five hour near limit", .97, false, true}, {"exhausted", .4, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, b, calls := accountHarness(t)
			b.Mode = "auto"
			h.health("anthropic", provider.Health{Util: map[string]float64{"five_hour": tc.util}})
			h.health("openai", provider.Health{Util: map[string]float64{"seven_day": .1}})
			if tc.exhausted {
				h.exhausted("anthropic", errors.New("quota"))
			}
			id := startAccountSession(t, h, accountConf(), 1000)
			if _, err := h.Step(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if tc.move && ((*calls)["anthropic"] != 0 || (*calls)["openai"] != 1) {
				t.Fatalf("pre-step did not move: %v", *calls)
			}
			if !tc.move && (*calls)["anthropic"] != 1 {
				t.Fatalf("healthy session moved for headroom: %v", *calls)
			}
		})
	}
}

func TestAccountEligibilityAndFallbackSkip(t *testing.T) {
	h, _, _ := accountHarness(t)
	h.Providers["local"] = provider.Provider{Envelope: accountEnvelope{name: "local"}}
	h.Fallbacks = func() map[string]string {
		return map[string]string{"anthropic": "openai/gpt-6-luna", "openai": "local/different-model"}
	}
	conf := accountConf()
	id := startAccountSession(t, h, conf, 210000)
	chain, _ := h.Sessions.Load(id)
	if h.eligible(conf, chain, "openai/gpt-6-luna") {
		t.Fatal("window too small was accepted")
	}
	if got := h.fallback(conf.Model, chain); got != "local/different-model" {
		t.Fatalf("ineligible target not skipped: %q", got)
	}
	if h.eligible(conf, chain, "gone/model") || h.eligible(conf, chain, "openai/unknown-model") {
		t.Fatal("unserved/unknown target accepted")
	}
	media := statefile.Record{Header: statefile.Header{N: 99, Kind: statefile.Message, Type: "image/png"}, Body: []byte("image")}
	if !h.eligible(conf, nil, "openai/gpt-6-luna") {
		t.Fatal("unused input capability prevents switch")
	}
	if h.eligible(conf, []statefile.Record{media}, "openai/gpt-6-luna") {
		t.Fatal("unsupported media accepted")
	}
}

func TestAccountPickAndCreate(t *testing.T) {
	h, b, _ := accountHarness(t)
	original := b.Pool[0]
	if got := h.Pick(original); got != original {
		t.Fatal("off changed model")
	}
	b.Mode = "auto"
	h.health("anthropic", provider.Health{Util: map[string]float64{"five_hour": .1, "seven_day": .9}})
	if got := h.Pick(original); got != b.Pool[1] {
		t.Fatalf("unknown headroom should beat .1: %q", got)
	}
	if got := h.Pick("outside/model"); got != "outside/model" {
		t.Fatal("outside pool changed")
	}
	if got := h.Pick("anthropic/claude-opus-5-5"); got != "anthropic/claude-opus-5-5" {
		t.Fatal("a model outside the pool changed despite sharing its provider")
	}
	id, err := h.Create(context.Background(), Create{Model: original, ContextLimit: 1000000, Input: []string{"image/png"}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	chain, _ := h.Sessions.Load(id)
	c, _ := config(chain)
	if c.Model != b.Pool[1] || c.ContextLimit != modelCatalog.Presets[c.Model].ContextLimit {
		t.Fatalf("create: %+v", c)
	}
	if !slices.ContainsFunc(chain, func(r statefile.Record) bool { return r.Kind == statefile.Config && r.Via == "balance" }) {
		t.Fatal("missing balance provenance")
	}
	h.exhausted("anthropic", errors.New("quota"))
	h.exhausted("openai", errors.New("quota"))
	if got := h.Pick(original); got != original {
		t.Fatalf("all exhausted: %s", got)
	}
	id = startAccountSession(t, h, accountConf(), 0)
	if _, err := h.Step(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestAccountBackoffAndHeadroom(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	a := accountState{Health: provider.Health{Util: map[string]float64{"five_hour": .99, "seven_day": .2}, Resets: map[string]time.Time{"five_hour": reset}, Updated: now}}
	for _, tc := range []struct {
		msg   string
		state accountState
		want  time.Time
	}{
		{"quota", accountState{}, now.Add(exhaustedBackoff)}, {"quota", a, reset},
		{"reset at " + reset.Format(time.RFC3339), a, reset},
		{fmt.Sprintf("resetsAt: %d", reset.Unix()), accountState{}, reset},
		{"retry in 20 minutes", accountState{}, now.Add(20 * time.Minute)},
		{"usage limit; resets 2pm (UTC)", accountState{}, now.Add(2 * time.Hour)},
	} {
		if got := exhaustedUntil(errors.New(tc.msg), tc.state, now); !got.Equal(tc.want) {
			t.Errorf("%q: %s want %s", tc.msg, got, tc.want)
		}
	}
	if got := headroom(a, now); got < .009 || got > .011 {
		t.Fatalf("max window headroom: %f", got)
	}
	a.Until = reset
	if headroom(a, now) != -1 || !limited(a, now) {
		t.Fatal("exhausted is selectable")
	}
	if headroom(a, reset) != unknownHeadroom || limited(a, reset) {
		t.Fatal("expired backoff did not permit a probe")
	}
	if headroom(accountState{}, now) != unknownHeadroom {
		t.Fatal("unknown headroom")
	}
}
