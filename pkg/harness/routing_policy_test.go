package harness

import (
	"context"
	"encoding/json"
	"pai/pkg/statefile"
	"strings"
	"testing"
)

func routingFixture(t *testing.T) (*Harness, statefile.Conf) {
	h, _, _ := accountHarness(t)
	c := accountConf()
	c.Routing = &statefile.Routing{AllowedModels: []string{c.Model, "openai/gpt-6-luna"}, Difficulty: 1, Consequence: 1, Levels: []statefile.RouteLevel{{Models: []string{c.Model}}, {Models: []string{"openai/gpt-6-luna"}}}, FailuresPerLevel: 1, MaxSteps: 3}
	return h, c
}
func TestRoutingOptInAndApproval(t *testing.T) {
	h, c := routingFixture(t)
	off := c
	off.Routing = nil
	got, d, err := h.chooseRoute(off, nil)
	if err != nil || got.Model != off.Model || !strings.Contains(d.Reason, "disabled") {
		t.Fatal(got, d, err)
	}
	c.Routing.AllowedModels = c.Routing.AllowedModels[:1]
	if h.validateRouting(c) == nil {
		t.Fatal("unapproved destination accepted")
	}
}
func TestRoutingEscalationBudgetAndSyntheticMessages(t *testing.T) {
	h, c := routingFixture(t)
	exit := 1
	chain := []statefile.Record{{Header: statefile.Header{Kind: statefile.Message, Via: "test"}}, {Header: statefile.Header{Kind: statefile.Call, ID: "failed-call"}}, {Header: statefile.Header{Kind: statefile.Result, For: "failed-call", Exit: &exit}}, {Header: statefile.Header{Kind: statefile.Message, Via: "job"}}}
	got, _, err := h.chooseRoute(c, chain)
	if err != nil || got.Model != "openai/gpt-6-luna" {
		t.Fatal(got.Model, err)
	}
	for range c.Routing.MaxSteps {
		body, _ := json.Marshal(RouteDecision{Event: "request", Model: c.Model})
		chain = append(chain, statefile.Record{Header: statefile.Header{Kind: statefile.Route}, Body: body})
	}
	chain = append(chain, statefile.Record{Header: statefile.Header{Kind: statefile.Message, Via: "retry"}})
	if _, _, err := h.chooseRoute(c, chain); err == nil || !strings.Contains(err.Error(), "max_steps") {
		t.Fatal("retry reset spending bound", err)
	}
}
func TestRoutingInputBudgetAndNoProviderCall(t *testing.T) {
	h, c := routingFixture(t)
	c.Routing.Verify = &statefile.VerifyPolicy{Model: "openai/gpt-6-luna", MinConsequence: 1}
	c.Routing.Levels = c.Routing.Levels[:1]
	err := h.verifyRoute(context.Background(), nil, c, c.Model, []statefile.Record{{Header: statefile.Header{Kind: statefile.Start, ID: "s"}}}, "step", strings.Repeat("x", verificationBytes+1))
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatal("oversized reviewer input was sent", err)
	}
}
func TestRoutingNestedEffortMerge(t *testing.T) {
	original := map[string]json.RawMessage{"reasoning": json.RawMessage(`{"effort":"low","summary":"auto"}`)}
	got := mergeRouteParams(original, map[string]json.RawMessage{"reasoning": json.RawMessage(`{"effort":"high"}`)})
	if !strings.Contains(string(got["reasoning"]), "high") || !strings.Contains(string(got["reasoning"]), "auto") || strings.Contains(string(original["reasoning"]), "high") {
		t.Fatal("effort merge lost fields or mutated input")
	}
}
