package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// Compaction starts when a step's prompt passes compactAt of the context
// limit, and keeps the newest keepAt of the limit verbatim. A token is about
// four bytes of English or code.
const (
	compactAt     = 0.7
	keepAt        = 0.25
	bytesPerToken = 4
	// A call's arguments and a result are cut to these in what the summarizer
	// reads: their gist is enough to say what was done, and the file keeps all.
	callBytes   = 600
	resultBytes = 1500
	// extractEvery is how many new records a closed session gathers before
	// what it learnt is extracted into memory.
	extractEvery = 12
	// memoryTime bounds a summarize or extract call, which is one model reply.
	memoryTime = 5 * time.Minute
)

var (
	recall   = provider.Tool{Name: "recall", Description: "Search PAI's long-term memory: what was learnt in every earlier session, on every machine and model. Returns items with ids", Schema: schema(`{"type":"object","properties":{"query":{"type":"string"},"k":{"type":"integer","description":"how many items, default 8"}},"required":["query"]}`)}
	remember = provider.Tool{Name: "remember", Description: "Save one lasting item to PAI's long-term memory, as one self-contained sentence. Use for facts about the user, their machines and projects, preferences, decisions and why, todos, and procedures that worked", Schema: schema(`{"type":"object","properties":{"text":{"type":"string"},"kind":{"type":"string","enum":["fact","preference","decision","todo","procedure"]},"scope":{"type":"string","description":"global, or project:<name>; default global"}},"required":["text"]}`)}
	forget   = provider.Tool{Name: "forget", Description: "Mark a memory item as no longer true, by its id, with why. It stops being recalled; it is never deleted", Schema: schema(`{"type":"object","properties":{"id":{"type":"string"},"reason":{"type":"string"}},"required":["id","reason"]}`)}
)

// memoryTools are the tools that run on the server, not on a worker.
var memoryTools = map[string]bool{"recall": true, "remember": true, "forget": true}

type memArgs struct {
	Query, Text, Kind, Scope, ID, Reason string
	K                                    int
}

// memoryCall runs a memory tool for session id. A failure is a result the
// model reads.
func (h *Harness) memoryCall(ctx context.Context, id string, call statefile.Record, c provider.Call) statefile.Record {
	if h.Memory == nil {
		return result(call.ID, "error: this server has no memory store")
	}
	var a memArgs
	if err := json.Unmarshal(c.Arguments, &a); err != nil {
		return result(call.ID, "error: "+err.Error())
	}
	switch c.Name {
	case "recall":
		k := a.K
		if k <= 0 {
			k = 8
		}
		hits, err := h.Memory.Search(ctx, a.Query, id, min(k, 30))
		if err != nil {
			return result(call.ID, "error: "+err.Error())
		}
		if len(hits) == 0 {
			return result(call.ID, "no memory matches")
		}
		var b strings.Builder
		for _, hit := range hits {
			b.WriteString(line(hit.Item))
		}
		return result(call.ID, b.String())
	case "remember":
		it, fresh, err := h.Memory.Remember(ctx, memory.Item{Text: a.Text, Kind: a.Kind, Scope: a.Scope, Session: id, From: call.N, To: call.N, Source: "model"})
		if err != nil {
			return result(call.ID, "error: "+err.Error())
		}
		if !fresh {
			return result(call.ID, "already known as "+it.ID+": "+it.Text)
		}
		return result(call.ID, "remembered as "+it.ID)
	case "forget":
		it, err := h.Memory.Invalidate(ctx, a.ID, a.Reason)
		if err != nil {
			return result(call.ID, "error: "+err.Error())
		}
		return result(call.ID, "forgot "+it.ID+": "+it.Text)
	}
	return result(call.ID, "error: unknown tool "+c.Name)
}

// line is an item as the model reads it.
func line(it memory.Item) string {
	return fmt.Sprintf("- [%s] (%s, %s) %s\n", it.ID, it.Kind, it.Scope, it.Text)
}

// recallTurn adds what memory recalls for the messages a person sent since
// the last step, as a system record after them. It goes in the file, so the
// same file builds the same request and the cached prefix holds.
func (h *Harness) recallTurn(ctx context.Context, s *session, id string, conf statefile.Conf, chain []statefile.Record) error {
	if h.Memory == nil {
		return nil
	}
	seen := 0
	for _, r := range chain {
		if r.Kind == statefile.Step {
			seen = r.Upto
		}
		if r.Kind == statefile.System && r.Source == "memory" {
			seen = max(seen, r.N)
		}
	}
	var asked []string
	for _, r := range chain {
		if r.N > seen && r.Kind == statefile.Message && r.Via != "job" && r.Via != "retry" && strings.HasPrefix(r.Type, "text/") {
			asked = append(asked, string(r.Body))
		}
	}
	if len(asked) == 0 {
		return nil
	}
	fill := 0.0
	if conf.ContextLimit > 0 {
		fill = float64(statefile.ContextUsed(chain)) / float64(conf.ContextLimit)
	}
	rv, err := h.Memory.Recall(ctx, memory.Turn{Session: id, Query: strings.Join(asked, "\n"), Fill: fill})
	if err != nil {
		log.Printf("session %s: recall: %v", id, err)
		return nil // a turn goes on without memory
	}
	if len(rv.Hits) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Memory recalled for this turn (retrieval %s). Use what helps and ignore the rest. Call forget with an id that is no longer true, and recall for more.\n", rv.ID)
	for _, hit := range rv.Hits {
		b.WriteString(line(hit.Item))
	}
	_, err = h.append(s, system("memory", b.String()))
	return err
}

// settle tells memory what the model did with the turn's recall, once the
// session closes: everything it wrote and called after the newest recall.
func (h *Harness) settle(ctx context.Context, id string, chain []statefile.Record) {
	if h.Memory == nil {
		return
	}
	from := 0
	for _, r := range chain {
		if r.Kind == statefile.System && r.Source == "memory" {
			from = r.N
		}
	}
	var reply strings.Builder
	called := false
	for _, r := range chain {
		if r.N <= from {
			continue
		}
		switch r.Kind {
		case statefile.Text:
			reply.Write(r.Body)
			reply.WriteByte('\n')
		case statefile.Call:
			var c provider.Call
			if json.Unmarshal(r.Body, &c) == nil {
				called = called || c.Name == "recall"
				reply.Write(c.Arguments)
				reply.WriteByte('\n')
			}
		}
	}
	if err := h.Memory.Settle(ctx, id, reply.String(), called); err != nil {
		log.Printf("session %s: settle: %v", id, err)
	}
}

// afterTurn runs when a session closes: settle the recall, then, once enough
// is new, extract what was learnt, in the background.
func (h *Harness) afterTurn(ctx context.Context, id string) {
	if h.Memory == nil {
		return
	}
	chain, err := h.Sessions.Load(id)
	if err != nil {
		return
	}
	h.settle(ctx, id, chain)
	from := h.Memory.Extracted(ctx, id)
	tip := chain[len(chain)-1].N
	if tip-from < extractEvery {
		return
	}
	conf, err := config(chain)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memoryTime)
		defer cancel()
		text := transcript(chain, from, tip, conf.ContextLimit*bytesPerToken/2)
		out, err := h.complete(ctx, conf, id, extractSystem, text)
		if err != nil {
			log.Printf("session %s: extract: %v", id, err)
			return
		}
		h.keep(ctx, id, from+1, tip, out)
		if err := h.Memory.SetExtracted(ctx, id, tip); err != nil {
			log.Printf("session %s: extract: %v", id, err)
		}
	}()
}

const memoryRules = `Memory: a JSON array in one ` + "```json" + ` fence of lasting items worth knowing in later sessions, each {"kind": "fact" | "preference" | "decision" | "todo" | "procedure", "text": one self-contained sentence that names what it is about, "scope": "global" or "project:<name>"}. Keep only knowledge that outlives this session: the user, their machines, projects, accounts and paths, preferences, decisions and why, open todos, and procedures that worked. Leave out passing steps and anything secret, such as keys and passwords. An empty array when there is none.`

const extractSystem = `You read a stretch of an agent session and list what is worth remembering. Reply with only: ` + memoryRules

const compactSystem = `You compact the history of an agent session, so the agent can go on with this summary in its place. Reply with two sections.

## Summary
What the user wants; what was done and found, with exact file paths, commands, names, ids, URLs and numbers; decisions and why; the state now; what failed; and the next steps. Be specific and complete enough to continue without the history. Say that the full history stays in the session's .state file.

## Memory
` + memoryRules

// fence finds a json fence.
var fence = regexp.MustCompile("(?s)```json\\s*(.*?)```")

// keep saves the memory items a reply lists, learnt from session records
// from through to.
func (h *Harness) keep(ctx context.Context, id string, from, to int, reply string) []memory.Item {
	m := fence.FindStringSubmatch(reply)
	if m == nil {
		return nil
	}
	var items []struct{ Kind, Text, Scope string }
	if err := json.Unmarshal([]byte(m[1]), &items); err != nil {
		log.Printf("session %s: memory reply: %v", id, err)
		return nil
	}
	var kept []memory.Item
	for _, it := range items {
		got, fresh, err := h.Memory.Remember(ctx, memory.Item{Text: it.Text, Kind: it.Kind, Scope: it.Scope, Session: id, From: from, To: to, Source: "extract"})
		if err != nil {
			log.Printf("session %s: remember: %v", id, err)
			continue
		}
		if fresh {
			kept = append(kept, got)
		}
	}
	return kept
}

// compact replaces the oldest records of a full context with a summary, when
// the last step's prompt passed compactAt of the limit. The cut falls where
// no call waits for its result and keeps keepAt of the limit verbatim. The
// summary is a compaction record; the records stay in the file. What the
// stretch taught goes to memory.
func (h *Harness) compact(ctx context.Context, s *session, id string, conf statefile.Conf, chain []statefile.Record) (bool, error) {
	if conf.ContextLimit <= 0 || float64(statefile.ContextUsed(chain)) < compactAt*float64(conf.ContextLimit) {
		return false, nil
	}
	cut := cutPoint(chain, int(keepAt*float64(conf.ContextLimit))*bytesPerToken)
	if cut == 0 {
		return false, nil
	}
	prev := 0
	for _, r := range chain {
		if r.Kind == statefile.Compaction {
			prev = r.Upto
		}
	}
	if cut <= prev {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, memoryTime)
	defer cancel()
	text := transcript(chain, 0, cut, conf.ContextLimit*bytesPerToken/2)
	out, err := h.complete(ctx, conf, id, compactSystem, text)
	if err != nil {
		return false, fmt.Errorf("compaction: %w", err)
	}
	summary := out
	if i := strings.Index(out, "## Memory"); i >= 0 {
		summary = strings.TrimSpace(out[:i])
	}
	r := statefile.Record{Header: statefile.Header{Kind: statefile.Compaction, Type: "text/markdown", Source: "compaction", Upto: cut}, Body: []byte("Summary of this session through record " + fmt.Sprint(cut) + ", which stays in the session's .state file:\n\n" + summary)}
	if _, err := h.append(s, r); err != nil {
		return false, err
	}
	if h.Memory != nil {
		kept := h.keep(ctx, id, prev+1, cut, out)
		if _, _, err := h.Memory.Remember(ctx, memory.Item{Kind: memory.Summary, Text: summary, Scope: "session:" + id, Session: id, From: prev + 1, To: cut, Source: "compaction"}); err != nil {
			log.Printf("session %s: summary to memory: %v", id, err)
		}
		if h.Memory.OnEvent != nil {
			h.Memory.OnEvent(memory.Event{Type: "compacted", Session: id, Items: kept, Detail: map[string]any{"upto": cut, "used": statefile.ContextUsed(chain), "limit": conf.ContextLimit}})
		}
	}
	return true, nil
}

// cutPoint is the last record a compaction may cover: one after which no
// call waits for its result, with at least keep bytes after it, or half of
// what follows the opening when that is less. 0 when none.
func cutPoint(chain []statefile.Record, keep int) int {
	tail := make([]int, len(chain)+1) // bytes from i to the end
	for i := len(chain) - 1; i >= 0; i-- {
		tail[i] = tail[i+1] + len(chain[i].Body)
	}
	// A large system prompt in a small limit leaves less than keep to cut
	// from: keep half of what follows the opening then.
	opening := 0
	for opening < len(chain) && chain[opening].Kind != statefile.Message {
		opening++
	}
	keep = min(keep, tail[min(opening, len(chain))]/2)
	open := map[string]bool{}
	best := 0
	for i, r := range chain {
		switch r.Kind {
		case statefile.Call:
			open[r.ID] = true
		case statefile.Result:
			delete(open, r.For)
		}
		boundary := r.Kind == statefile.Result || r.Kind == statefile.Message || r.Kind == statefile.Step
		if boundary && len(open) == 0 && tail[i+1] >= keep && i+1 < len(chain) {
			best = r.N
		}
	}
	return best
}

// transcript is records from through to as plain text for a summarizer,
// within max bytes: the newest kept when it is over.
func transcript(chain []statefile.Record, from, to, max int) string {
	var parts []string
	for _, r := range chain {
		if r.N <= from || r.N > to {
			continue
		}
		body := string(r.Body)
		switch r.Kind {
		case statefile.Message:
			if !strings.HasPrefix(r.Type, "text/") {
				body = "[" + r.Type + "]"
			}
			parts = append(parts, "USER via "+r.Via+":\n"+body)
		case statefile.Text:
			parts = append(parts, "ASSISTANT:\n"+body)
		case statefile.Call:
			parts = append(parts, "CALL: "+clipText(body, callBytes))
		case statefile.Result:
			parts = append(parts, "RESULT: "+clipText(body, resultBytes))
		case statefile.Error:
			parts = append(parts, "ERROR: "+clipText(body, resultBytes))
		case statefile.Compaction:
			parts = append(parts, "EARLIER SUMMARY:\n"+body)
		}
	}
	out := strings.Join(parts, "\n\n")
	if max > 0 && len(out) > max {
		out = "[earlier records cut]\n" + out[len(out)-max:]
	}
	return out
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf(" [... %d bytes cut]", len(s)-n)
}

// complete is one reply of the session's model to text, with system as its
// system prompt and no tools: what compaction and extraction ask.
func (h *Harness) complete(ctx context.Context, conf statefile.Conf, id, sys, text string) (string, error) {
	p, model, err := h.provider(conf.Model)
	if err != nil {
		return "", err
	}
	records := []statefile.Record{
		system("memory", sys),
		{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "pai", ID: statefile.NewID()}, Body: []byte(text)},
	}
	var out strings.Builder
	_, err = p.Step(ctx, provider.Request{Model: model, Key: "memory-" + id, Context: records, Params: conf.Params}, func(r statefile.Record) {
		if r.Kind == statefile.Text {
			out.Write(r.Body)
		}
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out.String()) == "" {
		return "", fmt.Errorf("the model replied with no text")
	}
	return out.String(), nil
}
