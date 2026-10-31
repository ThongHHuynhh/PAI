package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

const (
	maxLiveChildren = 4    // a hard ceiling beyond the prompt's preferred three children
	handoffBytes    = 6000 // the prototype keeps this tail of a completed child's reply
	reportBytes     = 4000 // bound the context an automatic report adds to the parent
	childPreview    = 100  // a short table needs only the beginning of a reply
	// These rules supplement the ordinary system prompt only for orchestrators.
	orchestratorSystem = `You are the orchestrator. Talk to the user and keep responsibility for the result.
Do small tasks and research yourself, especially work under about 5 tool calls. Delegate big, parallel, long, or context-heavy work with delegate, listing the files it touches.
Reuse before spawning: delegate routes to a related child, queueing behind its current work or resuming its context. Use tell for a specific child's follow-up; children lists your children; stop_child stops one.
Use one git branch or worktree per child. Never let children share a working tree. Review actual diffs and run checks, not just the child's claims.
Keep at most 3 children working at a time. Children must never spawn other sessions.
Children report automatically via session:<child>; a report wakes you. After delegating, tell the user what is running and end your turn instead of polling.
Worker tools require a worker argument; delegate, tell, children and stop_child run on the server and require none.`
)

var (
	delegate          = provider.Tool{Name: "delegate", Description: "Delegate a task, reusing a related child before spawning. Returns the action, child id and reason", Schema: schema(`{"type":"object","properties":{"name":{"type":"string"},"task":{"type":"string"},"files":{"type":"array","items":{"type":"string"}}},"required":["name","task"]}`)}
	tell              = provider.Tool{Name: "tell", Description: "Post text to one of your own child sessions", Schema: schema(`{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"}},"required":["id","text"]}`)}
	children          = provider.Tool{Name: "children", Description: "List your children with status, records, context percent and last reply", Schema: schema(`{"type":"object","properties":{}}`)}
	stopChild         = provider.Tool{Name: "stop_child", Description: "Stop one of your own child sessions", Schema: schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)}
	orchestratorTools = map[string]bool{"delegate": true, "tell": true, "children": true, "stop_child": true}
)

type orchestratorArgs struct {
	Name, Task, ID, Text string
	Files                []string
}

// orchestratorCall returns model-readable failures, like memoryCall. Child
// ownership and depth are enforced here even if a model invents a tool call.
func (h *Harness) orchestratorCall(ctx context.Context, id string, conf statefile.Conf, call statefile.Record, c provider.Call) statefile.Record {
	fail := func(err error) statefile.Record { return result(call.ID, "error: "+err.Error()) }
	if conf.Tools != "orchestrator" || conf.Parent != "" {
		return fail(fmt.Errorf("only a top-level orchestrator can use %s", c.Name))
	}
	var a orchestratorArgs
	if err := json.Unmarshal(c.Arguments, &a); err != nil {
		return fail(err)
	}
	if c.Name == "delegate" {
		text, err := h.dispatch(ctx, id, conf, call.ID, a)
		if err != nil {
			return fail(err)
		}
		return result(call.ID, text)
	}
	if c.Name == "children" {
		infos, err := h.childInfos(id)
		if err != nil {
			return fail(err)
		}
		var b strings.Builder
		b.WriteString("id\tname\tstatus\trecords\tcontext\tlast reply\n")
		for _, child := range infos {
			chain, err := h.Sessions.Load(child.ID)
			if err != nil {
				return fail(err)
			}
			preview := strings.ReplaceAll(strings.ReplaceAll(byteHead(lastReply(chain), childPreview), "\n", " "), "\t", " ")
			fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%.0f%%\t%s\n", child.ID, child.Name, child.Status, child.N, contextFill(child)*100, preview)
		}
		return result(call.ID, b.String())
	}
	if !statefile.ValidID(a.ID) {
		return fail(fmt.Errorf("bad child session id %q", a.ID))
	}
	child, err := h.Info(a.ID)
	if err != nil {
		return fail(err)
	}
	if child.Parent != id {
		return fail(fmt.Errorf("session %s is not your child", a.ID))
	}
	via := "session:" + id
	switch c.Name {
	case "tell":
		if strings.TrimSpace(a.Text) == "" {
			return fail(fmt.Errorf("tell needs text"))
		}
		if err := h.postChild(ctx, id, a.ID, call.ID, a.Text); err != nil {
			return fail(err)
		}
		return result(call.ID, "posted to child "+a.ID)
	case "stop_child":
		if err := h.Stop(ctx, a.ID, via); err != nil {
			return fail(err)
		}
		return result(call.ID, "stopped child "+a.ID)
	}
	return fail(fmt.Errorf("unknown tool %s", c.Name))
}

// childInfos reads the filesystem registry, including sessions not yet in
// the index after a restart. A broken unrelated file does not block a parent.
func (h *Harness) childInfos(parent string) ([]Info, error) {
	ids, err := h.Sessions.List()
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, id := range ids {
		info, err := h.Info(id)
		if err != nil {
			log.Printf("session %s: children: %v", id, err)
			continue
		}
		if info.Parent == parent {
			out = append(out, info)
		}
	}
	return out, nil
}

func contextFill(info Info) float64 {
	if info.Config.ContextLimit <= 0 {
		return 0
	}
	return float64(info.ContextUsed) / float64(info.Config.ContextLimit)
}

func (h *Harness) dispatch(ctx context.Context, parent string, conf statefile.Conf, key string, a orchestratorArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Task) == "" {
		return "", fmt.Errorf("delegate needs name and task")
	}
	if err := checkName(a.Name); err != nil {
		return "", err
	}
	infos, err := h.childInfos(parent)
	if err != nil {
		return "", err
	}
	now, live := time.Now(), 0
	var candidates []routeCandidate
	for _, info := range infos {
		if info.Status == statefile.StatusStepping {
			live++
		}
		age := now.Sub(info.Updated)
		if age > candidateTTL {
			continue
		}
		chain, err := h.Sessions.Load(info.ID)
		if err != nil {
			return "", err
		}
		text := []string{info.Name}
		first := true
		for _, r := range chain {
			if r.Kind == statefile.Message && first {
				text = append(text, string(r.Body))
				first = false
			}
			if r.Kind == statefile.Call {
				text = append(text, string(r.Body))
			}
		}
		candidates = append(candidates, routeCandidate{id: info.ID, text: strings.Join(text, " "), status: info.Status, fill: contextFill(info), age: age})
	}
	d := decide(relevance(strings.Join(append([]string{a.Name, a.Task}, a.Files...), " "), candidates))
	via, task := "session:"+parent, a.Task
	// Keep explicit paths in the child's brief so future routing can find them,
	// even when the task itself does not spell out the affected files.
	if len(a.Files) > 0 {
		task += "\n\nFiles:\n" + strings.Join(a.Files, "\n")
	}
	if d.action == "spawn" {
		if live >= maxLiveChildren {
			return "", fmt.Errorf("%d children are already stepping; wait for one to finish", maxLiveChildren)
		}
		if d.handoff != "" {
			chain, err := h.Sessions.Load(d.handoff)
			if err != nil {
				return "", err
			}
			reply := lastReply(chain)
			if len(reply) > handoffBytes {
				from := len(reply) - handoffBytes
				for !utf8.RuneStart(reply[from]) {
					from++
				}
				reply = reply[from:]
			}
			task += fmt.Sprintf("\n\nRelated work already done by session %s (do not redo it):\n%s", d.handoff, reply)
		}
		// A child starts on the freest account of the balance pool, when one is set. Keep
		// provider parameters too: some models require an output limit.
		d.id, err = h.Create(ctx, Create{Parent: parent, Model: h.Pick(conf.Model), Workers: conf.Workers, ContextLimit: conf.ContextLimit, Input: conf.Input, Params: conf.Params, Tools: "default", Name: a.Name}, via)
		if err != nil {
			return "", err
		}
		task = childBrief(parent, task)
	} else {
		lead := "NEW TASK for this session, you already know the area.\n"
		if d.action == "queue" {
			lead = "QUEUED TASK. Finish your current work first, then do this, and report each separately.\n"
		}
		task = lead + task
	}
	if err := h.postChild(ctx, parent, d.id, key, task); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s child %s: %s", d.action, d.id, d.why), nil
}

// Like a background job, a child outlives the parent's current turn. A stop
// may race delivery and its cascade may already have passed this child, so
// check cancellation on both sides of Post, as job does after registering.
func (h *Harness) postChild(ctx context.Context, parent, child, key, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	via := "session:" + parent
	if _, err := h.Post(context.WithoutCancel(ctx), child, via, key, []Part{{Type: "text/plain", Body: []byte(text)}}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, h.Stop(context.WithoutCancel(ctx), child, via))
	}
	return nil
}

func childBrief(parent, task string) string {
	return fmt.Sprintf("You are a child agent. Your parent session is %s; it only reads your final reply.\nTask:\n%s\n\nRules: stay inside this task; work on your own git branch or worktree if you change code; do not start other sessions; do not push to main. When done, end with a short report: what you did, files and branch, test results, anything open or risky.", parent, task)
}

// lastReply is the text after the last message or result, plus a terminal
// error, matching the prototype's result extraction.
func lastReply(chain []statefile.Record) string {
	var text []string
	for _, r := range chain {
		switch r.Kind {
		case statefile.Message, statefile.Result:
			text = nil
		case statefile.Text:
			text = append(text, string(r.Body))
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Kind == statefile.Name {
			continue
		}
		if chain[i].Kind == statefile.Error {
			text = append(text, "[error] "+string(chain[i].Body))
		}
		break
	}
	return strings.Join(text, "\n")
}

// byteHead respects the byte budget without cutting through a UTF-8 rune.
func byteHead(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

func (h *Harness) reportChild(ctx context.Context, id string) error {
	chain, err := h.Sessions.Load(id)
	if err != nil {
		return err
	}
	conf, err := config(chain)
	if err != nil || conf.Parent == "" {
		return err
	}
	st := statefile.Derive(chain)
	if st != statefile.StatusClosed && st != statefile.StatusFailed {
		return nil
	}
	tip := chain[len(chain)-1]
	name, outcome := "", "finished"
	for _, r := range chain {
		if r.Kind == statefile.Name {
			name = string(r.Body)
		}
	}
	// Stops are deliberate, including a parent's cascade with a connector via.
	// Reporting those would wake the parent immediately after it was stopped.
	if h.session(id).stoppedBy() != "" {
		return nil
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Kind == statefile.Name {
			continue
		}
		if chain[i].Kind == statefile.Error && chain[i].Via != "" {
			return nil
		}
		break
	}
	if st == statefile.StatusFailed {
		outcome = "failed"
	}
	text := fmt.Sprintf("[child %s %s %s after %d records. Final reply:]\n%s", id, name, outcome, tip.N, byteHead(lastReply(chain), reportBytes))
	_, err = h.Post(ctx, conf.Parent, "session:"+id, fmt.Sprintf("done-%d", tip.N), []Part{{Type: "text/plain", Body: []byte(text)}})
	return err
}
