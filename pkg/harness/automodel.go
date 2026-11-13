package harness

import (
	"encoding/json"
	"fmt"
	"strings"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// Auto model switching, /auto-switch-model in the TUI. While a session has it
// on, its model proposes a model for each new task and when it is stuck, and
// the person approves each switch through ask. switch_model reads that answer
// from the session file, so no switch happens without the person's yes.

var switchModelTool = provider.Tool{Name: "switch_model", Description: "Switch this session to another model from the next step, only while auto model switching is on. Call it with model and reason; it returns the exact ask arguments. Call ask with them, then switch_model again with the same model: it switches only if the person chose that model", Schema: schema(`{"type":"object","properties":{"model":{"type":"string","description":"provider/model"},"reason":{"type":"string","description":"why it fits the task better, in a few words"}},"required":["model","reason"]}`)}

// switchHeader marks the ask switch_model needs a yes to.
const switchHeader = "Model switch"

const (
	autoModelOn = `Auto model switching is on, turned on by the person. Models this session may use:
%s
At the start of each new task from the person, judge its difficulty and risk. If another listed model fits clearly better, call switch_model with it and a short reason: a stronger model for hard or risky work, such as code changes, debugging or machines; a faster, cheaper one for status checks, questions and summaries. During a task, when you are stuck, such as the same check failing three times, propose a stronger one the same way. switch_model gives the ask to put to the person and switches only on their yes, from the next step. A switch costs the prompt cache once: never propose one for a small gain. After a no, keep the model for the rest of that task.`
	autoModelOff = "Auto model switching is off, turned off by the person: do not call switch_model."
)

// SetAutoModel turns auto model switching on or off from the next step, and
// tells the model so in a system record, which lists the models it may use.
func (h *Harness) SetAutoModel(id, via string, on bool) error {
	note := autoModelOff
	if on {
		models, err := h.Models(id)
		if err != nil {
			return err
		}
		var lines []string
		for _, m := range models {
			if m.OK {
				lines = append(lines, fmt.Sprintf("- %s (%s)%s", m.Model, m.Name, map[bool]string{true: ", current"}[m.Current]))
			}
		}
		if len(lines) < 2 {
			return fmt.Errorf("auto model switching needs at least two models this session can use; it has %d", len(lines))
		}
		note = fmt.Sprintf(autoModelOn, strings.Join(lines, "\n"))
	}
	if _, err := h.setConf(id, via, func(c *statefile.Conf, _ []statefile.Record) error {
		c.AutoModel = on
		return nil
	}); err != nil {
		return err
	}
	return h.note(h.session(id), id, system("automodel", note))
}

func switchQuestion(from, to, reason string) Question {
	return Question{Header: switchHeader, Question: fmt.Sprintf("Switch from %s to %s for this task?", from, to), Options: []Option{
		{Label: to, Description: reason},
		{Label: "Keep " + from, Description: "stay on the current model"},
	}}
}

// switchModel asks for, then makes, a switch the person approved.
func (h *Harness) switchModel(id string, conf statefile.Conf, call statefile.Record, c provider.Call) statefile.Record {
	fail := func(msg string) statefile.Record { return result(call.ID, "error: "+msg) }
	var a struct{ Model, Reason string }
	if err := json.Unmarshal(c.Arguments, &a); err != nil {
		return fail(err.Error())
	}
	switch {
	case !conf.AutoModel:
		return fail("auto model switching is off; the person turns it on with /auto-switch-model")
	case conf.Parent != "":
		return fail("a child session cannot switch its model")
	case a.Model == conf.Model:
		return result(call.ID, "already on "+a.Model)
	}
	chain, err := h.Sessions.Load(id)
	if err != nil {
		return fail(err.Error())
	}
	if reason := h.modelReason(conf, chain, a.Model); reason != "" {
		return fail(fmt.Sprintf("model %q: %s", a.Model, reason))
	}
	if !approved(chain, a.Model) {
		b, _ := json.Marshal(map[string]any{"questions": []Question{switchQuestion(conf.Model, a.Model, a.Reason)}})
		return result(call.ID, "call ask with these exact arguments, then switch_model again with the same model: "+string(b))
	}
	if _, err := h.SetModel(id, "switch_model", a.Model); err != nil {
		return fail(err.Error())
	}
	return result(call.ID, "switched to "+a.Model+" from the next step")
}

// approved is whether the person's newest answer to a switch ask, since the
// config last changed, chose to.
func approved(chain []statefile.Record, to string) bool {
	asks, yes := map[string][]Question{}, false
	for _, r := range chain {
		switch r.Kind {
		case statefile.Config:
			yes = false
		case statefile.Call:
			var c provider.Call
			if json.Unmarshal(r.Body, &c) != nil || c.Name != "ask" {
				continue
			}
			if qs, err := parseQuestions(c.Arguments); err == nil && len(qs) == 1 && qs[0].Header == switchHeader && len(qs[0].Options) == 2 && qs[0].Options[0].Label == to {
				asks[r.ID] = qs
			}
		case statefile.Result:
			if qs, ok := asks[r.For]; ok {
				yes = string(r.Body) == answered(qs, Reply{Answers: [][]string{{to}}})
			}
		}
	}
	return yes
}
