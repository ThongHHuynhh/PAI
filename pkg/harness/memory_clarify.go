package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
	"slices"
)

var memoryClarifyTool = provider.Tool{Name: "memory_clarify", Description: "Prepare an ask question for two conflicting memory IDs. Call ask with exact returned arguments, then provide ask_call and the user's exact selected answer. Never invent an answer; source facts are retained.", Schema: schema(`{"type":"object","properties":{"id":{"type":"string"},"peer":{"type":"string"},"ask_call":{"type":"string"},"answer":{"type":"string"}},"required":["id","peer"]}`)}

func (h *Harness) memoryQuestion(ctx context.Context, a, b string) (Question, error) {
	x, err := h.Memory.Get(ctx, a)
	if err != nil {
		return Question{}, err
	}
	y, err := h.Memory.Get(ctx, b)
	if err != nil {
		return Question{}, err
	}
	if x.Scope != y.Scope || !slices.Contains(x.Conflicts, b) {
		return Question{}, fmt.Errorf("no unresolved conflict between these sources")
	}
	return Question{Header: "Memory", Question: fmt.Sprintf("Clarify conflicting memory sources. A [%s]: %s; B [%s]: %s. Which should govern? Originals remain available.", a, x.Text, b, y.Text), Options: []Option{{Label: "Use A", Description: "Use source " + a}, {Label: "Use B", Description: "Use source " + b}, {Label: "Unresolved", Description: "Keep both disputed; no dependent actions"}}}, nil
}

// VerifyMemoryResolution accepts only an actual recorded standard ask result.
// A model-supplied explanation, stale answer or subagent cannot authorize it.
func (h *Harness) VerifyMemoryResolution(ctx context.Context, a, b string, p memory.Resolution) error {
	if p.Session == "" || p.Call == "" || (p.Answer != "Use A" && p.Answer != "Use B") {
		return fmt.Errorf("memory: explicit recorded user selection required")
	}
	chain, err := h.Sessions.Load(p.Session)
	if err != nil {
		return err
	}
	conf, err := config(chain)
	if err != nil {
		return err
	}
	if conf.Parent != "" {
		return fmt.Errorf("memory: subagents cannot authorize clarification")
	}
	q, err := h.memoryQuestion(ctx, a, b)
	if err != nil {
		return err
	}
	x, _ := h.Memory.Get(ctx, a)
	y, _ := h.Memory.Get(ctx, b)
	var asked bool
	for _, r := range chain {
		if r.Kind == statefile.Call && r.ID == p.Call && r.Time.UnixMilli() >= x.Created.UnixMilli() && r.Time.UnixMilli() >= y.Created.UnixMilli() {
			var c provider.Call
			if json.Unmarshal(r.Body, &c) == nil && c.Name == "ask" {
				qs, e := parseQuestions(c.Arguments)
				actual, _ := json.Marshal(qs)
				expected, _ := json.Marshal([]Question{q})
				asked = e == nil && string(actual) == string(expected)
			}
		}
		if asked && r.Kind == statefile.Result && r.For == p.Call && string(r.Body) == answered([]Question{q}, Reply{Answers: [][]string{{p.Answer}}}) {
			return nil
		}
	}
	return fmt.Errorf("memory: answer is not backed by the matching completed user ask")
}

func (h *Harness) clarifyMemory(ctx context.Context, id string, call statefile.Record, c provider.Call) statefile.Record {
	fail := func(e error) statefile.Record { return result(call.ID, "error: "+e.Error()) }
	var a struct {
		ID, Peer, Answer string
		AskCall          string `json:"ask_call"`
	}
	if err := json.Unmarshal(c.Arguments, &a); err != nil {
		return fail(err)
	}
	q, err := h.memoryQuestion(ctx, a.ID, a.Peer)
	if err != nil {
		return fail(err)
	}
	if a.AskCall == "" {
		b, _ := json.Marshal(map[string]any{"questions": []Question{q}})
		return result(call.ID, "clarification_required: call ask with these exact arguments: "+string(b))
	}
	if err := h.Memory.ResolveConflict(ctx, a.ID, a.Peer, memory.Resolution{Session: id, Call: a.AskCall, Answer: a.Answer}); err != nil {
		return fail(err)
	}
	return result(call.ID, "User clarification recorded with source ask provenance; originals retained. Only perform the correction the user selected.")
}
