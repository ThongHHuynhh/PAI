package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// askTime is how long an ask waits for a person before it tells the model to
// decide by itself.
var askTime = 30 * time.Minute

var ask = provider.Tool{Name: "ask", Description: "Ask the person one to four multiple choice questions and wait for the answers. Use it when the request is ambiguous or a choice is theirs, before you guess. Each question has two to four options; put the one you recommend first. The person can always type another answer. The call waits up to 30 minutes", Schema: schema(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{"header":{"type":"string","description":"a label of at most 12 characters"},"question":{"type":"string"},"multiSelect":{"type":"boolean","description":"the person may pick more than one option"},"options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"label":{"type":"string","description":"a short choice"},"description":{"type":"string","description":"what the choice means or costs"}},"required":["label","description"]}}},"required":["header","question","options"]}}},"required":["questions"]}`)}

// Question is one question of an ask call, as the call's arguments hold it.
type Question struct {
	Header      string   `json:"header"`
	Question    string   `json:"question"`
	MultiSelect bool     `json:"multiSelect"`
	Options     []Option `json:"options"`
}

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Reply is the body of POST /sessions/{id}/answer: for each question, the
// labels picked or the text typed; or declined.
type Reply struct {
	Call     string     `json:"call"`
	Answers  [][]string `json:"answers,omitempty"`
	Declined bool       `json:"declined,omitempty"`
}

// ErrNoAsk is an answer to a call that waits for none.
var ErrNoAsk = errors.New("no question is waiting for an answer")

type pending struct {
	qs []Question
	ch chan Reply
}

// Questions is the questions in an ask call record, none when it has none.
func Questions(call statefile.Record) []Question {
	var c provider.Call
	var a struct{ Questions []Question }
	if json.Unmarshal(call.Body, &c) != nil || json.Unmarshal(c.Arguments, &a) != nil {
		return nil
	}
	return a.Questions
}

func parseQuestions(raw json.RawMessage) ([]Question, error) {
	var a struct{ Questions []Question }
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if n := len(a.Questions); n < 1 || n > 4 {
		return nil, fmt.Errorf("questions has %d items, want 1 to 4", n)
	}
	for i, q := range a.Questions {
		switch {
		case strings.TrimSpace(q.Question) == "":
			return nil, fmt.Errorf("question %d is empty", i+1)
		case strings.TrimSpace(q.Header) == "" || utf8.RuneCountInString(q.Header) > 12:
			return nil, fmt.Errorf("question %d needs a header of 1 to 12 characters", i+1)
		case len(q.Options) < 2 || len(q.Options) > 4:
			return nil, fmt.Errorf("question %d has %d options, want 2 to 4", i+1, len(q.Options))
		}
		var seen []string
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || slices.Contains(seen, o.Label) {
				return nil, fmt.Errorf("question %d needs a distinct label on each option", i+1)
			}
			seen = append(seen, o.Label)
		}
	}
	return a.Questions, nil
}

// check is whether r answers qs: one non-empty answer per question, and one
// only for a question that is not multiple choice.
func check(qs []Question, r Reply) error {
	if r.Declined {
		return nil
	}
	if len(r.Answers) != len(qs) {
		return fmt.Errorf("%d answers for %d questions", len(r.Answers), len(qs))
	}
	for i, a := range r.Answers {
		if len(a) == 0 || len(a) > 1 && !qs[i].MultiSelect || slices.ContainsFunc(a, func(s string) bool { return strings.TrimSpace(s) == "" }) {
			return fmt.Errorf("question %d needs %s", i+1, map[bool]string{false: "one answer", true: "at least one answer"}[qs[i].MultiSelect])
		}
	}
	return nil
}

func answered(qs []Question, r Reply) string {
	if r.Declined {
		return "The user declined to answer the questions."
	}
	var parts []string
	for i, q := range qs {
		parts = append(parts, fmt.Sprintf("%q=%q", q.Question, strings.Join(r.Answers[i], ", ")))
	}
	return "User has answered your questions: " + strings.Join(parts, ", ") + ". You can now continue with the user's answers in mind."
}

// ask blocks until a person answers the call's questions, the wait ends, or
// the step is stopped. The questions are in the call record and the answers
// in the result, so no record kind is new and a reload shows both.
func (h *Harness) ask(ctx context.Context, id string, call statefile.Record, c provider.Call) statefile.Record {
	qs, err := parseQuestions(c.Arguments)
	if err != nil {
		return result(call.ID, "error: "+err.Error())
	}
	key, p := id+"/"+call.ID, &pending{qs, make(chan Reply, 1)}
	h.mu.Lock()
	if h.asks == nil {
		h.asks = map[string]*pending{}
	}
	h.asks[key] = p
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.asks, key)
		h.mu.Unlock()
	}()
	select {
	case r := <-p.ch:
		return result(call.ID, answered(qs, r))
	case <-time.After(askTime):
		return result(call.ID, fmt.Sprintf("error: no answer in %s. Decide by yourself and say what you assumed, or ask in plain text.", askTime))
	case <-ctx.Done():
		return result(call.ID, "stopped, effect unknown")
	}
}

// Answer hands a person's reply to the ask call that waits for it. A reply
// that does not fit the questions is refused and the call keeps waiting.
func (h *Harness) Answer(id string, r Reply) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.asks[id+"/"+r.Call]
	if !ok {
		return ErrNoAsk
	}
	if err := check(p.qs, r); err != nil {
		return err
	}
	select {
	case p.ch <- r:
		return nil
	default:
		return ErrNoAsk // answered already
	}
}
