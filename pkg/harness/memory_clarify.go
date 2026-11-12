package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
	"slices"
	"strings"
)

var memoryClarifyTool = provider.Tool{Name: "memory_clarify", Description: "Ask the person which of two disputed memory items holds. Call it with id and peer; it returns the exact ask arguments. Call ask with them, then call memory_clarify again with the same id and peer: it reads the answer from that ask and retires the item the person did not choose. Never invent an answer", Schema: schema(`{"type":"object","properties":{"id":{"type":"string"},"peer":{"type":"string"},"ask_call":{"type":"string","description":"optional: the ask call that answered; by default the newest matching ask"},"answer":{"type":"string"}},"required":["id","peer"]}`)}

// clarifyText is the most of an item's text a clarification question shows:
// enough to tell the two apart in a terminal.
const clarifyText = 300

func (h *Harness) memoryQuestion(ctx context.Context, a, b string) (Question, error) {
	if a == b {
		return Question{}, fmt.Errorf("an item cannot conflict with itself")
	}
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
	return Question{Header: "Memory", Question: fmt.Sprintf("Which memory holds?\nA (%s): %s\nB (%s): %s", x.Created.Format("2006-01-02"), oneLine(x.Text), y.Created.Format("2006-01-02"), oneLine(y.Text)), Options: []Option{{Label: "Use A", Description: "Keep A, retire B"}, {Label: "Use B", Description: "Keep B, retire A"}, {Label: "Unresolved", Description: "Keep both, marked disputed"}}}, nil
}

// oneLine is s on one line, cut to clarifyText characters.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > clarifyText {
		return string(r[:clarifyText]) + "â€¦"
	}
	return s
}

// askedAnswer finds the newest completed ask in chain that asked exactly q,
// and which of options the person picked: empty when they declined or typed
// something else.
func askedAnswer(chain []statefile.Record, q Question, options []string) (call, answer string) {
	expected, _ := json.Marshal([]Question{q})
	asked := map[string]bool{}
	for _, r := range chain {
		switch r.Kind {
		case statefile.Call:
			var c provider.Call
			if json.Unmarshal(r.Body, &c) == nil && c.Name == "ask" {
				qs, err := parseQuestions(c.Arguments)
				actual, _ := json.Marshal(qs)
				asked[r.ID] = err == nil && string(actual) == string(expected)
			}
		case statefile.Result:
			if !asked[r.For] {
				continue
			}
			call, answer = r.For, ""
			for _, o := range options {
				if string(r.Body) == answered([]Question{q}, Reply{Answers: [][]string{{o}}}) {
					answer = o
				}
			}
		}
	}
	return call, answer
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
	chain, err := h.Sessions.Load(id)
	if err != nil {
		return fail(err)
	}
	options := []string{"Use A", "Use B", "Unresolved"}
	found, answer := askedAnswer(chain, q, options)
	if a.AskCall == "" {
		a.AskCall = found
	}
	if a.Answer == "" || a.AskCall == found {
		a.Answer = answer
	}
	if a.AskCall == "" {
		b, _ := json.Marshal(map[string]any{"questions": []Question{q}})
		return result(call.ID, "call ask with these exact arguments, then memory_clarify again with the same id and peer: "+string(b))
	}
	if a.Answer != "Use A" && a.Answer != "Use B" {
		return result(call.ID, "the person kept both items; they stay marked as disputed. Go on without relying on either")
	}
	if err := h.Memory.ResolveConflict(ctx, a.ID, a.Peer, memory.Resolution{Session: id, Call: a.AskCall, Answer: a.Answer}); err != nil {
		return fail(err)
	}
	keep, drop := a.ID, a.Peer
	if a.Answer == "Use B" {
		keep, drop = a.Peer, a.ID
	}
	if _, err := h.Memory.Invalidate(ctx, drop, "the person chose "+keep+" over it"); err != nil {
		return fail(err)
	}
	return result(call.ID, fmt.Sprintf("recorded: %s holds; %s is retired and can be restored", keep, drop))
}
