// Package handoff carries work between PAI Classic and PAI ULTRA at a safe
// boundary, by checkpoint. The original session or task is kept and linked
// to the new one; nothing is migrated in place. No model is called: a
// Classic session is created empty and the handoff text is saved for the
// person to send, and a Classic transcript becomes an ULTRA task contract by
// rule, not by summarizing.
package handoff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pai/pkg/harness"
	"pai/pkg/statefile"
	"pai/pkg/ultra/coord"
	"pai/pkg/ultra/mode"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/task"
)

const (
	// via is what the handoff writes on records it causes.
	via = "ultra-handoff"
	// Bounds on what a Classic transcript contributes to a task contract.
	objectiveMax = 4000
	contextMax   = 1500
	callTime     = 30 * time.Second
	// HandoffFile is the handoff text saved beside a checkpoint.
	HandoffFile = "handoff.md"
)

// Classic is the part of the PAI server client a handoff uses.
type Classic interface {
	Info(ctx context.Context, id string) (harness.Info, error)
	Records(ctx context.Context, id string, after int, wait string) ([]statefile.Record, error)
	Sessions(ctx context.Context, status string, limit int) ([]harness.Info, error)
	Create(ctx context.Context, cr harness.Create, via string) (string, error)
}

// Unsupported is the state that does not cross into Classic.
var toClassicUnsupported = []string{
	"Classic does not enforce ULTRA acceptance criteria: they travel as text in the handoff",
	"the executor's native session is not transferred; Classic starts fresh from the handoff",
	"ULTRA knowledge stays in ULTRA; only items named in the handoff travel",
	"the ULTRA task is kept, marked handed off, and linked to the new session",
}

// And what does not cross into ULTRA.
var fromClassicUnsupported = []string{
	"only the messages' text is carried; tool calls, results and model reasoning are not",
	"Classic memory recall and compaction summaries are not carried (import memory explicitly with pai ultra memory import)",
	"the model's own context is not transferred; ULTRA starts a fresh executor with the contract and a workspace checkpoint",
	"the Classic session is kept unchanged and linked to the new task",
}

// ToClassic is a handed-off task's new Classic session.
type ToClassic struct {
	Session     string   `json:"session"`
	HandoffPath string   `json:"handoff_path"`
	Text        string   `json:"text"`
	Tokens      int      `json:"tokens"` // estimated
	Unsupported []string `json:"unsupported"`
}

// ExplainToClassic says what a handoff to Classic does not carry.
func ExplainToClassic() []string { return slices.Clone(toClassicUnsupported) }

// ExplainFromClassic says what a handoff from Classic does not carry.
func ExplainFromClassic() []string { return slices.Clone(fromClassicUnsupported) }

// TaskToClassic checkpoints an ULTRA task at a safe boundary (nobody holds
// it), creates an empty Classic session, saves the handoff text for the
// person to send, marks the task handed off, and links both. It makes no
// model call.
func TaskToClassic(ctx context.Context, api Classic, c *coord.Coordinator, taskID string) (ToClassic, error) {
	t, err := c.Store.Get(taskID)
	if err != nil {
		return ToClassic{}, err
	}
	switch {
	case t.State.Held():
		return ToClassic{}, fmt.Errorf("task is %s: stop it or let the attempt finish before handing off", t.State)
	case t.State == task.Accepted || t.State == task.Failed || t.State.Terminal():
		return ToClassic{}, fmt.Errorf("task is %s: nothing to hand off", t.State)
	}
	// 1. Hold the task at the boundary: a blocker keeps any owner from
	// claiming it while the handoff runs.
	before := t.State
	t, err = c.Store.Update(taskID, task.Guard{Rev: t.Rev}, func(t *task.Task) (task.Event, error) {
		t.Blockers = append(t.Blockers, handoffBlocker)
		t.State = task.Blocked
		return task.Event{Type: "handoff_started", Actor: "person"}, nil
	})
	if err != nil {
		return ToClassic{}, fmt.Errorf("the task changed; try again: %w", err)
	}
	undo := func(cause error) (ToClassic, error) {
		_, uerr := c.Store.Update(taskID, task.Guard{}, func(t *task.Task) (task.Event, error) {
			t.Blockers = slices.DeleteFunc(t.Blockers, func(b string) bool { return b == handoffBlocker })
			if t.State == task.Blocked && len(t.Blockers) == 0 && before != task.Blocked {
				t.State = before
			}
			return task.Event{Type: "handoff_abandoned", Note: cause.Error()}, nil
		})
		return ToClassic{}, errors.Join(cause, uerr)
	}
	// 2. Checkpoint and save the handoff text before anything is created.
	cp, err := c.Store.Checkpoint(taskID)
	if err != nil {
		return undo(fmt.Errorf("checkpoint: %w", err))
	}
	_, resume := c.Project.Manifest.Budgets()
	shown := *cp
	shown.Blockers = slices.DeleteFunc(slices.Clone(cp.Blockers), func(b string) bool { return b == handoffBlocker })
	h := shown.Render(resume, nil)
	text := "Continuing a PAI ULTRA task in Classic. The handoff below is the task's state at a checkpoint; " +
		"its acceptance criteria are not checked automatically here.\n\n" + h.Text
	path := filepath.Join(cp.Dir, HandoffFile)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return undo(err)
	}
	// 3. Create the Classic session last, from the newest Classic session's
	// model and workers.
	cctx, cancel := context.WithTimeout(ctx, callTime)
	defer cancel()
	cr := harness.Create{Name: "from ULTRA " + taskID}
	if infos, err := api.Sessions(cctx, "", sessionsScan); err == nil {
		for _, in := range infos {
			if mode.Of(in.Config) == mode.Classic {
				b := in.Config
				cr.Model, cr.Tools, cr.ContextLimit, cr.Workers, cr.Input, cr.Params = b.Model, b.Tools, b.ContextLimit, b.Workers, b.Input, b.Params
				break
			}
		}
	}
	sid, err := api.Create(cctx, cr, via) // a Classic session: no mode
	if err != nil {
		return undo(fmt.Errorf("create Classic session: %w", err))
	}
	// 4. Mark the task handed off, then link both ways.
	link := mode.Link{From: "ultra:" + taskID, To: "classic:" + sid, Reason: "handoff at checkpoint " + cp.ID}
	if _, err := c.Store.Update(taskID, task.Guard{}, func(t *task.Task) (task.Event, error) {
		t.Blockers = slices.DeleteFunc(t.Blockers, func(b string) bool { return b == handoffBlocker })
		t.Links = append(t.Links, link.To)
		t.State = task.HandedOff
		return task.Event{Type: "handed_off", Actor: "person", Note: "to " + link.To + " at " + cp.ID}, nil
	}); err != nil {
		return ToClassic{Session: sid, HandoffPath: path}, fmt.Errorf("Classic session %s was created but the task could not be marked handed off: %w", sid, err)
	}
	if err := c.Project.AddLink(link); err != nil {
		return ToClassic{Session: sid, HandoffPath: path}, fmt.Errorf("handed off to %s, but the link was not saved: %w", sid, err)
	}
	return ToClassic{Session: sid, HandoffPath: path, Text: text, Tokens: project.EstimateTokens(text), Unsupported: ExplainToClassic()}, nil
}

// handoffBlocker holds a task while it is being handed off.
const handoffBlocker = "handoff to Classic in progress"

// sessionsScan is how many recent sessions are searched for a Classic one to copy.
const sessionsScan = 20

// offeredFile marks a saved handoff as already offered to the person.
const offeredFile = "handoff.offered"

func safeID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\.:`) && filepath.IsLocal(id)
}

// PendingFor is the saved handoff text for Classic session sid, if one was
// made for it and not yet offered. Offer marks it offered.
func PendingFor(mp mode.Project, sid string) (text string, offer func(), ok bool) {
	pr, err := mp.LoadPrefs()
	if err != nil {
		return "", nil, false
	}
	for _, l := range pr.LinksOf("classic:" + sid) {
		if l.To != "classic:"+sid || !strings.HasPrefix(l.From, "ultra:") {
			continue
		}
		cpID, ok := strings.CutPrefix(l.Reason, "handoff at checkpoint ")
		taskID := strings.TrimPrefix(l.From, "ultra:")
		if !ok || !safeID(cpID) || !safeID(taskID) {
			continue
		}
		dir := filepath.Join(mp.State, "tasks", taskID, "checkpoints", cpID)
		if _, err := os.Stat(filepath.Join(dir, offeredFile)); err == nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, HandoffFile))
		if err == nil {
			return string(b), func() { os.WriteFile(filepath.Join(dir, offeredFile), nil, 0o600) }, true
		}
	}
	return "", nil, false
}

// FromClassic is the plan for a Classic session's work as an ULTRA task.
type FromClassic struct {
	Session     string   `json:"session"`
	Objective   string   `json:"objective"`
	Messages    int      `json:"messages"`
	Unsupported []string `json:"unsupported"`
}

func isText(t string) bool { return t == "" || strings.HasPrefix(t, "text/") }

// PlanFromClassic reads a Classic session and builds a task contract from it
// by rule: the person's first message is the objective, later ones are
// added as follow-ups, and the newest assistant reply is noted as the state
// so far (unverified). Nothing is written.
func PlanFromClassic(ctx context.Context, api Classic, sid string) (FromClassic, error) {
	cctx, cancel := context.WithTimeout(ctx, callTime)
	defer cancel()
	info, err := api.Info(cctx, sid)
	if err != nil {
		return FromClassic{}, err
	}
	if mode.Of(info.Config) != mode.Classic {
		return FromClassic{}, fmt.Errorf("session %s is pinned to %s", sid, mode.Of(info.Config).Label())
	}
	if info.Status == statefile.StatusStepping {
		return FromClassic{}, errors.New("the session is working: stop it or let it finish before handing off")
	}
	recs, err := api.Records(cctx, sid, 0, "0")
	if err != nil {
		return FromClassic{}, err
	}
	var asks []string
	last := ""
	for _, r := range recs {
		switch {
		case r.Kind == statefile.Message && isText(r.Type):
			asks = append(asks, strings.TrimSpace(string(r.Body)))
		case r.Kind == statefile.Text:
			last = strings.TrimSpace(string(r.Body))
		}
	}
	if len(asks) == 0 {
		return FromClassic{}, errors.New("the session has no message to carry")
	}
	var b strings.Builder
	b.WriteString(asks[0])
	for i, a := range asks[1:] {
		fmt.Fprintf(&b, "\n\nFollow-up %d: %s", i+1, a)
	}
	obj := cutRunes(b.String(), objectiveMax)
	if last != "" {
		obj += "\n\nState in the Classic session so far (the assistant's last reply, unverified): " + cutRunes(last, contextMax)
	}
	return FromClassic{Session: sid, Objective: obj, Messages: len(asks), Unsupported: ExplainFromClassic()}, nil
}

// ApplyFromClassic creates the task the plan describes, checkpoints the
// workspace, and links the task and the session. It does not run the task.
func ApplyFromClassic(c *coord.Coordinator, p FromClassic) (*task.Task, error) {
	t, err := c.Create(coord.NewTask{Objective: p.Objective, Links: []string{"classic:" + p.Session},
		Constraints: []string{"Continue the work begun in PAI Classic session " + p.Session + "; check the workspace before relying on earlier claims."}})
	if err != nil {
		return nil, err
	}
	if _, err := c.Store.Checkpoint(t.ID); err != nil {
		return t, fmt.Errorf("checkpoint: %w", err)
	}
	if err := c.Project.AddLink(mode.Link{From: "classic:" + p.Session, To: "ultra:" + t.ID, Reason: "continue in ULTRA"}); err != nil {
		return t, err
	}
	return c.Store.Get(t.ID)
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
