package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pai/pkg/connector"
	"pai/pkg/connector/tui/term"
	"pai/pkg/ultra/ctxsel"
	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/handoff"
	"pai/pkg/ultra/mode"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/runtime"
	"pai/pkg/ultra/task"
)

// The ULTRA screen's views, in tab order.
const (
	vWork = iota
	vSkills
	vInstr
	vMemory
	vRuns
	vContext
	nViews
)

var viewNames = [nViews]string{"Workspace", "Skills", "Instructions", "Memory", "Runs", "Context"}

// viewShort is the tab labels on the bottom line, short enough for one row.
var viewShort = [nViews]string{"Work", "Skill", "Ins", "Mem", "Runs", "Cont"}

const (
	// ultraDetailFrom is the width from which a list and its detail show
	// side by side; narrower, the detail replaces the list.
	ultraDetailFrom = 110
)

// Focus regions, cycled by Tab and Shift+Tab.
const (
	fMain = iota // the input on Workspace, the list elsewhere
	fDetail
	fTabs
)

// ultraAccent is ULTRA's own accent: amber, apart from Classic's.
func ultraAccent(t Theme) term.Color { return ultraAmber(t) }

// ultra is the PAI ULTRA screen. Its management views read and write local
// files only: no model is called from them.
type ultra struct {
	a    *App
	rt   *runtime.Runtime
	proj *project.Project  // the TUI's own copy: a running task never shares it
	c    *connector.Client // nil offline
	dir  string

	view, focus int
	root        *Node
	body        *Node
	bar         *Node // the bottom line: views, status, model, sizes
	status      *Text

	// Workspace.
	transcript *Scrollbox
	input      *Input
	inputRow   *Node
	busy       *Node     // the spinner line while a task runs
	turn       time.Time // when the running task started
	stopping   bool
	work       *Node
	cur        string // the current task
	running    bool
	cancel     context.CancelFunc
	armed      bool
	lastCtx    *ctxsel.Context // the context of the newest run here, for loaded status
	lastCtxFor string          // the task lastCtx was built for
	lastTokens int             // the newest run's tokens, for the bar
	quitting   bool            // /exit during a run: quit once it stops
	taskCache  *task.Task
	taskAt     time.Time
	nextChecks []task.Criterion
	executor   string

	// Management views.
	lists      [nViews]*ulist
	detail     *textView
	showDetail bool
	mgmt       *Node
	listNode   *Node
	detailNode *Node
	stage      *project.Stage
	memAll     bool
	ctxNow     *ctxsel.Context
}

// openUltra makes the ULTRA screen for dir. When ULTRA cannot open, the
// error is returned and Classic carries on untouched.
func openUltra(a *App, c *connector.Client, dir string) (*ultra, error) {
	rt, err := runtime.Open(dir)
	if err != nil {
		return nil, err
	}
	if c != nil {
		rt.Server.Load = func(context.Context) (*connector.Client, error) { return c, nil }
	}
	proj, err := project.Open(dir)
	if err != nil {
		rt.Close()
		return nil, err
	}
	u := &ultra{a: a, rt: rt, proj: proj, c: c, dir: dir, status: &Text{}, detail: &textView{}, transcript: NewScrollbox(), input: NewInput()}
	for i := range u.lists {
		u.lists[i] = &ulist{}
	}
	u.executor = rt.DefaultExecutor()
	u.input.Placeholder = "Describe a task, or /help"
	u.input.OnSubmit = u.submit
	u.input.Before = u.inputKey
	u.input.History = a.History
	u.bar = &Node{Fixed: 1, Pad: [4]int{2, 0, 2, 0}, View: ultraBar{u}}
	u.busy = &Node{Fixed: 1, Pad: [4]int{2, 1, 2, 0}, View: ultraBusy{u}}
	box := ultraInputBox(u.input)
	u.inputRow = Box(&Node{Dir: Row, Pad: [4]int{2, 1, 2, 0}}, box)
	convo := Box(&Node{Dir: Col, Grow: 1}, &Node{Grow: 1, Pad: [4]int{2, 1, 2, 0}, View: u.transcript}, u.busy, u.inputRow)
	u.work = Box(&Node{Dir: Row, Grow: 1}, convo)
	u.listNode = &Node{Grow: 1, Pad: [4]int{2, 1, 1, 0}, View: ultraMgmtKeys{u, false}}
	u.detailNode = &Node{Grow: 1, Paint: PaintPanel, Pad: [4]int{2, 1, 1, 0}, View: ultraMgmtKeys{u, true}}
	u.mgmt = Box(&Node{Dir: Row, Grow: 1}, u.listNode, u.detailNode)
	u.body = Box(&Node{Dir: Col, Grow: 1}, u.work, u.mgmt)
	u.root = Box(&Node{Dir: Col, Paint: PaintScreen}, u.body, u.bar)
	if w := workspaceWarning(proj.Root); w != "" {
		u.say(w, InkError)
	}
	if ids, err := rt.Recover(); err == nil && len(ids) > 0 {
		u.say(fmt.Sprintf("recovered %d task(s) whose owner stopped: see Runs", len(ids)), InkAccent)
	}
	u.pickCurrent()
	u.setView(vWork)
	return u, nil
}

// pickCurrent makes the newest unfinished task current.
func (u *ultra) pickCurrent() {
	ts, _ := u.rt.Store.List()
	for _, t := range ts {
		if !t.State.Terminal() && t.State != task.Accepted {
			u.cur = t.ID
			return
		}
	}
}

func (u *ultra) Root() *Node { return u.root }

func (u *ultra) Size(a *App, w, h int) {
	u.busy.Hidden = !u.running
	if !u.busy.Hidden {
		u.busy.Fixed = 2 // the spinner line and the gap over the input
	}
	u.work.Hidden = u.view != vWork
	u.mgmt.Hidden = u.view == vWork
	u.input.MaxRows = max(1, min(inputRows, h*inputShare/100))
	wide := w >= ultraDetailFrom
	u.detailNode.Hidden = !u.showDetail
	u.listNode.Hidden = u.showDetail && !wide
	if wide {
		u.listNode.Grow, u.detailNode.Grow = 3, 2
	} else {
		u.listNode.Grow, u.detailNode.Grow = 1, 1
	}
	*u.status = *a.Status
}

func (u *ultra) Tick(a *App) {}

func (u *ultra) say(s string, ink Ink) {
	u.transcript.Append(&uline{text: s, ink: ink})
}

// setView switches the view, loading its data. Management views read local
// files; none of them calls a model.
func (u *ultra) setView(v int) {
	u.view = (v + nViews) % nViews
	u.showDetail = false
	u.focus = fMain
	if u.view == vWork {
		u.a.Focus(u.inputRow.Kids[0].Kids[1])
		return
	}
	u.a.Focus(u.listNode)
	u.reload()
}

// reload refreshes the current view's rows.
func (u *ultra) reload() {
	l := u.lists[u.view]
	l.Err = ""
	u.fresh()
	if u.stage == nil || !u.stage.Dirty() {
		// Re-read the configuration, so an apply from the CLI or another
		// window shows; staged edits keep the copy they were made on.
		if p, err := project.Open(u.proj.Root); err == nil {
			u.proj, u.stage = p, nil
		} else {
			l.Err = err.Error()
		}
	}
	switch u.view {
	case vSkills:
		u.loadSkills(l)
	case vInstr:
		u.loadInstructions(l)
	case vMemory:
		u.loadMemory(l)
	case vRuns:
		u.loadRuns(l)
	case vContext:
		u.loadContext(l)
	}
	if u.showDetail {
		u.inspect()
	}
}

// resolved is the configuration with staged edits applied, when there are any.
func (u *ultra) resolved() (project.Resolved, []project.Problem) {
	if u.stage != nil && u.stage.Dirty() {
		pv, err := u.stage.Preview()
		if err == nil {
			p := *u.proj
			p.Manifest = u.stage.Manifest
			return p.Resolve(), pv.Problems
		}
	}
	r := u.proj.Resolve()
	return r, r.Problems
}

// taskTTL bounds how long a drawn task may be stale: draws read it often.
const taskTTL = 300 * time.Millisecond

func (u *ultra) curTask() *task.Task {
	if u.cur == "" {
		return nil
	}
	if u.taskCache != nil && u.taskCache.ID == u.cur && time.Since(u.taskAt) < taskTTL {
		return u.taskCache
	}
	t, err := u.rt.Store.Get(u.cur)
	if err != nil {
		return nil
	}
	u.taskCache, u.taskAt = t, time.Now()
	return t
}

// fresh drops the cached task, after anything that may change it.
func (u *ultra) fresh() { u.taskCache = nil }

// loadedStatus says whether a skill or instruction is in the newest run's
// context: from the context PAI built, or unknown for files an executor
// reads on its own.
func (u *ultra) loadedStatus(id string) string {
	if u.lastCtx == nil || u.lastCtxFor != u.cur {
		return "unknown (no run of this task here)"
	}
	for _, it := range u.lastCtx.Items {
		if it.ID == id {
			return it.Loaded
		}
	}
	return "not in the newest run's context"
}

func (u *ultra) stageOrNew() *project.Stage {
	if u.stage == nil {
		u.stage = u.proj.Begin()
	}
	return u.stage
}

func (u *ultra) staged() int {
	if u.stage == nil {
		return 0
	}
	return len(u.stage.Log)
}

// Key is the screen's: keys the focused view did not take.
func (u *ultra) Key(a *App, k term.Key) bool {
	s := k.String()
	switch s {
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6":
		u.setView(int(s[4] - '1'))
	case "tab":
		u.cycleFocus(1)
	case "shift+tab":
		u.cycleFocus(-1)
	case "f1", "ctrl+p":
		u.help()
	case "pgup":
		u.transcript.Scroll(-u.transcript.Page())
	case "pgdn":
		u.transcript.Scroll(u.transcript.Page())
	case "esc":
		switch {
		case u.view != vWork:
			u.setView(vWork)
		case u.running && a.Again(escTwice):
			u.stop()
		case u.running:
			u.armed = true
		}
	default:
		return false
	}
	return true
}

// cycleFocus moves between the tabs, the main region and the detail.
func (u *ultra) cycleFocus(d int) {
	ring := []int{fTabs, fMain}
	if u.showDetail {
		ring = append(ring, fDetail)
	}
	i := slices.Index(ring, u.focus)
	u.focus = ring[(i+d+len(ring))%len(ring)]
	switch {
	case u.focus == fTabs:
		u.a.Focus(u.bar)
	case u.view == vWork:
		u.a.Focus(u.inputRow.Kids[0].Kids[1])
	case u.focus == fDetail:
		u.a.Focus(u.detailNode)
	default:
		u.a.Focus(u.listNode)
	}
}

// inputKey takes the Workspace keys the input should not: focus, views, help.
func (u *ultra) inputKey(a *App, k term.Key) bool {
	switch k.String() {
	case "tab", "shift+tab", "f1", "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "pgup", "pgdn":
		return u.Key(a, k)
	case "esc":
		if u.running {
			return u.Key(a, k)
		}
	}
	return false
}

// submit runs a typed command or starts work on the text as a task.
func (u *ultra) submit(a *App, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	a.Remember(text)
	u.input.Clear()
	if strings.HasPrefix(text, "/") {
		u.command(text)
		return
	}
	if u.running {
		a.Say("A task is running: Esc Esc stops it; Runs shows it")
		u.input.insert(textElems(text)...)
		return
	}
	t := u.curTask()
	if t == nil || t.State.Terminal() || t.State == task.Accepted || t.State == task.Failed {
		nt, err := u.rt.Create(runtimeNewTask(text, u.nextChecks, u.executor))
		if err != nil {
			a.SayError(err)
			return
		}
		u.nextChecks = nil
		u.cur = nt.ID
		u.say("› "+text, InkText)
	} else {
		// A follow-up to a waiting task becomes a recorded constraint: the
		// task's criteria stay as they are.
		_, err := u.rt.Store.Update(t.ID, task.Guard{}, func(t *task.Task) (task.Event, error) {
			t.Constraints = append(t.Constraints, text)
			return task.Event{Type: "constraint", Actor: "person", Note: text}, nil
		})
		if err != nil {
			a.SayError(err)
			return
		}
		u.say("› "+text, InkText)
	}
	u.run()
}

// run works on the current task on a goroutine; progress comes back on the loop.
func (u *ultra) run() {
	if u.running {
		u.a.Say("a task is running: Esc Esc stops it")
		return
	}
	u.fresh()
	t := u.curTask()
	if t == nil {
		u.a.Say("no current task")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.running, u.cancel, u.armed, u.stopping, u.turn = true, cancel, false, false, time.Now()
	u.a.Ticking(true) // the spinner turns
	id, executor := t.ID, u.executor
	if t.Executor != "" {
		executor = t.Executor
	}
	u.a.Go(func() {
		out, err := u.rt.Run(ctx, id, executor, func(e exec.Event) {
			u.a.Post(func() { u.event(e) })
		})
		u.a.Post(func() { u.finished(out, err) })
	})
}

func (u *ultra) event(e exec.Event) {
	u.fresh()
	switch e.Kind {
	case "text":
		// The executor's reply is Markdown, rendered and word-wrapped as in
		// Classic; its pai-result block is PAI's, shown as checked results.
		if s := exec.StripResult(e.Text); s != "" {
			u.transcript.Append(&textBlock{s: s})
		}
	case "tool":
		u.say("  · "+cellCut(e.Text, 200), InkMuted)
	case "error":
		u.say("  ! "+e.Text, InkError)
	}
}

func (u *ultra) finished(out runtimeOutcome, err error) {
	u.running, u.cancel, u.stopping = false, nil, false
	u.a.Ticking(false)
	u.fresh()
	if out.Context.Items != nil {
		cx := out.Context
		u.lastCtx = &cx
		if out.Task != nil {
			u.lastCtxFor = out.Task.ID
		}
	}
	if u.quitting {
		u.rt.Close()
		u.a.Quit()
		return
	}
	if out.Task != nil && out.Result.Status == "" {
		// No attempt ran: the error below says why.
		out.Task = nil
	}
	if out.Task != nil {
		u.lastTokens = out.Result.Usage.Total()
	}
	// One mark for the outcome: ✓ and the time when PAI's checks passed,
	// else ✗ and why. The checks behind it are in Runs.
	took := dur(time.Since(u.turn).Truncate(time.Second))
	switch {
	case err == nil && out.Task != nil && out.Task.State == task.Accepted:
		u.say("✓ "+took, InkAccent)
	case err != nil:
		u.say("✗ "+strings.SplitN(err.Error(), "\n", 2)[0], InkError)
	case out.Task != nil:
		u.say("✗ "+string(out.Task.State), InkError)
	}
	for _, w := range append(out.Context.Warnings, out.Warnings...) {
		u.say("  ! "+w, InkError)
	}
}

// ultraBusy is the line over the input while a task runs: Classic's
// spinner, the time so far, and how to stop.
type ultraBusy struct{ u *ultra }

func (b ultraBusy) Draw(a *App, c *Canvas, r Rect) {
	u := b.u
	if !u.running {
		return
	}
	muted := a.Theme.muted()
	if u.stopping {
		c.Text(r.X, r.Y, "Stopping...", muted)
		return
	}
	key := term.Style{FG: a.Theme.Text, Attr: term.Bold}
	again := ""
	if u.armed {
		again = " again"
	}
	x := c.Text(r.X, r.Y, Spin()+" Working "+dur(time.Since(u.turn).Truncate(time.Second))+"  ", muted)
	x = c.Text(x, r.Y, "esc", key)
	c.Text(x, r.Y, again+" to stop", muted)
}

func (u *ultra) stop() {
	if u.cancel != nil {
		u.cancel()
		u.stopping = true // the task pauses at a checkpoint
	}
}

// command runs a Workspace slash command. They take text after them.
func (u *ultra) command(text string) {
	name, arg, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	arg = strings.TrimSpace(arg)
	a := u.a
	switch name {
	case "help", "?":
		u.help()
	case "mode":
		u.modeMenu(arg)
	case "skills":
		u.setView(vSkills)
	case "rules", "instructions":
		u.setView(vInstr)
	case "memory":
		u.setView(vMemory)
	case "runs", "tasks":
		u.setView(vRuns)
	case "context":
		u.setView(vContext)
	case "new":
		u.cur = ""
		u.say("new task: describe it", InkMuted)
	case "check":
		if arg == "" {
			a.Say("/check <command>: a criterion for the next task")
			return
		}
		u.nextChecks = append(u.nextChecks, task.Criterion{ID: fmt.Sprintf("check%d", len(u.nextChecks)+1), Kind: task.CriterionCommand, Command: arg})
		u.say("next task's criterion: `"+arg+"` exits 0", InkMuted)
	case "executor":
		if _, ok := u.rt.Adapters[arg]; !ok {
			a.Say("/executor claude, codex or pai")
			return
		}
		u.executor = arg
		u.say("executor for new tasks: "+arg, InkMuted)
	case "resume", "task":
		if arg != "" {
			if _, err := u.rt.Store.Get(arg); err != nil {
				a.SayError(err)
				return
			}
			u.cur = arg
			u.fresh()
		}
		if name == "resume" {
			u.run()
		} else {
			u.say("current task: "+u.cur, InkMuted)
		}
	case "checkpoint":
		if t := u.curTask(); t != nil {
			if cp, err := u.rt.Store.Checkpoint(t.ID); err != nil {
				a.SayError(err)
			} else {
				u.say("checkpoint "+cp.ID, InkMuted)
			}
		}
	case "handoff":
		u.handoffMenu()
	case "exit", "quit":
		if u.running {
			u.quitting = true
			u.stop()
			return
		}
		u.rt.Close()
		a.Quit()
	default:
		a.Say("unknown command /" + name + ": /help lists them")
	}
}

// help is the keys of the view in focus.
func (u *ultra) help() {
	common := []string{
		"Alt+1–6, or [ and ] in a list   switch view", "Tab / Shift+Tab   move focus", "Esc   close detail, back to Workspace",
		"/   search (lists)", "↑↓ or j/k   move", "Enter   inspect", "?   these keys",
	}
	per := map[int][]string{
		vWork: {"Enter   send", "Ctrl+Enter   new line", "Esc Esc   stop the running task", "/check CMD   criterion for the next task",
			"/executor NAME   claude, codex or pai", "/new  /resume  /checkpoint  /handoff  /mode  /exit"},
		vSkills: {"Space   enable/disable (staged)", "m   activation: auto, always, manual", "s   path scope", "u   take the library's update",
			"P   pin to the locked version", "o   project override of the skill", "r   apply a profile", "p   preview staged changes", "a   apply   d   discard"},
		vInstr: {"n   new project rule", "e   edit (project-owned only)", "o   project override of a package or global rule", "c   conflicts and duplicates",
			"p   preview   a   apply   d   discard"},
		vMemory: {"v   verify (you, as a person)", "x   invalidate with a reason", "n   add a preference, decision, fact or hypothesis", "s   show all statuses", "i   preview a Classic memory import"},
		vRuns:   {"o   make current", "r   resume", "c   checkpoint", "A   approve a review criterion", "u   reconcile an unknown operation", "h   hand off to Classic", "R   recover dead owners"},
		vContext: {"Enter   the item's text", "the context of the current task (or the input's text as a draft)",
			"+ included   n loaded by the executor (status unknown)   - left out"},
	}
	tv := &textView{}
	tv.set(strings.Join(append(per[u.view], append([]string{""}, common...)...), "\n"))
	u.a.Dialog(viewNames[u.view]+" keys", &Node{View: tv})
}

// modeMenu is /mode in ULTRA: Classic for new sessions, or this task
// continued in Classic. Choosing makes no model call.
func (u *ultra) modeMenu(arg string) {
	switch arg {
	case "ultra":
		u.a.Say("already PAI ULTRA")
		return
	case "classic":
		u.toClassic()
		return
	}
	m := &Menu{Items: []MenuItem{
		{Label: "PAI ULTRA", Hint: "this screen", Disabled: true},
		{Label: "PAI Classic", Hint: "default for new sessions in this project; ULTRA tasks stay as they are", Pick: func(*App, *MenuItem) { u.toClassic() }},
	}}
	if t := u.curTask(); t != nil {
		m.Items = append(m.Items, MenuItem{Label: "Continue task " + t.ID + " in Classic", Hint: "checkpoint, new Classic session, both kept and linked",
			Pick: func(*App, *MenuItem) { u.handoffMenu() }})
	}
	u.a.OpenMenu("Mode", m)
}

func (u *ultra) toClassic() {
	if err := u.proj.SetDefault(mode.Classic); err != nil {
		u.a.SayError(err)
		return
	}
	if u.running {
		u.a.Say("Classic is the default now; the running ULTRA task keeps running here")
		return
	}
	u.rt.Close()
	if u.c == nil {
		u.a.SetScreen(newLogin(u.a))
		return
	}
	u.a.SetScreen(newSession(u.a, newSessionList(u.a, u.c), ""))
	u.a.Say("PAI Classic: default for new sessions in this project")
}

// handoffMenu explains what does not travel, then hands the current task to
// a new Classic session on confirmation.
func (u *ultra) handoffMenu() {
	if u.running {
		u.a.Say("a task is running: stop it (Esc Esc) before handing off")
		return
	}
	t := u.curTask()
	if t == nil {
		u.a.Say("no current task")
		return
	}
	if u.c == nil {
		u.a.Say("offline: a handoff needs the PAI server Classic runs on")
		return
	}
	tv := &textView{}
	tv.set("Not carried:\n- " + strings.Join(handoff.ExplainToClassic(), "\n- ") + "\n\nEnter hands off · Esc cancels")
	var l *Layer
	l = u.a.Dialog("Continue "+t.ID+" in PAI Classic?", &Node{View: confirm{tv, func() {
		u.a.Pop(l)
		out, err := handoff.TaskToClassic(context.Background(), u.c, u.rt.Coordinator, t.ID)
		if err != nil {
			u.a.SayError(err)
			return
		}
		u.rt.Close()
		s := newSession(u.a, newSessionList(u.a, u.c), out.Session)
		u.a.SetScreen(s)
		u.a.Say("Handed off: the handoff is in the input; Enter sends it")
	}}})
}

// confirm is a text with Enter to go on.
type confirm struct {
	*textView
	ok func()
}

func (c confirm) Key(a *App, k term.Key) bool {
	if k.String() == "enter" {
		c.ok()
		return true
	}
	return c.textView.Key(a, k)
}

// uline is one transcript line, wrapped to the width.
type uline struct {
	text string
	ink  Ink
}

func (l *uline) Rows(a *App, w int) []Line {
	st := term.Style{FG: a.Theme.ink(cmpInk(l.ink, InkText))}
	var out []Line
	for _, para := range strings.Split(l.text, "\n") {
		// Wrap at spaces; a word longer than the width is cut where it must be.
		var cur []Glyph
		n := 0
		flush := func() {
			out = append(out, Line{Gs: cur})
			cur, n = nil, 0
		}
		for i, word := range strings.Split(para, " ") {
			ww := term.Width(word)
			if i > 0 {
				if n+1+ww > w && n > 0 {
					flush()
				} else {
					cur = append(cur, Glyph{G: " ", W: 1, St: st})
					n++
				}
			}
			term.Graphemes(word, func(g string, gw int) {
				if n+gw > w && n > 0 {
					flush()
				}
				cur = append(cur, Glyph{G: g, W: gw, St: st})
				n += gw
			})
		}
		flush()
	}
	return out
}

func cmpInk(a, b Ink) Ink {
	if a != InkNone {
		return a
	}
	return b
}

// ultraBar is the one line at the bottom: the ULTRA mark and the views on
// the left, the status message after them, and on the right what needs
// attention (only when it does), then the model, the context PAI sends and
// the newest run's tokens. The current view is bracketed, not only colored.
type ultraBar struct{ u *ultra }

func (b ultraBar) Draw(a *App, c *Canvas, r Rect) {
	u := b.u
	acc := term.Style{FG: ultraAccent(a.Theme), Attr: term.Bold}
	muted := a.Theme.muted()
	x := c.Text(r.X, r.Y, "◆ ULTRA", acc) + 1 // Text returns the column after the text
	for i, n := range viewShort {
		label, st := " "+n+" ", muted
		if i == u.view {
			label, st = "["+n+"]", acc
			if u.focus == fTabs {
				st.Attr |= term.Underline
			}
		}
		if x+term.Width(label) > r.X+r.W {
			return
		}
		x = c.Text(x, r.Y, label, st)
	}
	right := cellCut(strings.Join(u.barItems(), " · "), max(0, r.X+r.W-x-2))
	end := r.X + r.W - term.Width(right)
	if right != "" {
		c.Text(end, r.Y, right, muted)
	}
	if s := strings.TrimSpace(u.status.S); s != "" {
		st := muted
		if u.status.Ink == InkError {
			st = term.Style{FG: a.Theme.Error}
		}
		c.Text(x+2, r.Y, cellCut(s, max(0, end-x-4)), st)
	}
}

// barItems is the right of the bar. Attention items come first and only
// when they apply; the model and sizes are always there, as in Classic.
func (u *ultra) barItems() []string {
	var items []string
	if n := u.staged(); n > 0 {
		items = append(items, fmt.Sprintf("%d staged: a apply, d discard", n))
	}
	if u.proj.ManifestEdited {
		items = append(items, "project.yaml edited")
	}
	latest := u.proj.Revision()
	if t := u.curTask(); t != nil && t.ConfigRev != latest && (t.State.Held() || u.running) {
		items = append(items, "restart to apply "+project.RevisionLabel(latest))
	}
	if u.c == nil {
		items = append(items, "offline")
	}
	items = append(items, cmpStr(u.proj.Manifest.Executor.Model, u.executor))
	if u.lastCtx != nil {
		items = append(items, "ctx "+shortCount(u.lastCtx.Tokens))
	}
	if u.lastTokens > 0 {
		items = append(items, shortCount(u.lastTokens)+" tok")
	}
	return items
}

// shortCount is 950, 1.2k or 372k.
func shortCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%dk", n/1000)
}

func (b ultraBar) Key(a *App, k term.Key) bool {
	switch k.String() {
	case "left", "h":
		b.u.setView(b.u.view - 1)
	case "right", "l":
		b.u.setView(b.u.view + 1)
	default:
		return false
	}
	b.u.focus = fTabs
	a.Focus(b.u.bar)
	return true
}

// runtimeOutcome is coord.Outcome, named here to keep imports in one place.
type runtimeOutcome = coordOutcome

// ustamp is a time for lists.
func ustamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("01-02 15:04")
}

// workspaceWarning says when ULTRA was opened somewhere that is not a
// project: checkpoints there cannot restore changes, and a home folder is
// far too big to treat as one workspace.
func workspaceWarning(root string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.EqualFold(filepath.Clean(h), filepath.Clean(root)) {
		return "! This is your home folder, not a project. Quit (/exit), cd into a git repository and run pai there."
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return "! Not a git repository: checkpoints record file sizes and times only and cannot restore changes."
	}
	return ""
}

// ULTRA's amber: bright on dark themes, deeper on light ones so text in it
// stays readable.
var (
	amberOnDark  = term.Hex("#f5a623")
	amberOnLight = term.Hex("#b7791f")
)

// inputTint is how far the input's background moves from the panel toward
// amber: enough to set it apart from the screen, not enough to tire.
const inputTint = 0.18

func ultraAmber(t Theme) term.Color {
	if isDark(t.BG) {
		return amberOnDark
	}
	return amberOnLight
}

// isDark is whether a background is dark, by its perceived brightness.
func isDark(c term.Color) bool {
	r, g, b := uint8(c>>16), uint8(c>>8), uint8(c)
	return 0.299*float64(r)+0.587*float64(g)+0.114*float64(b) < 128
}

// ultraInputBox is ULTRA's input: an amber edge and an amber-tinted field,
// distinct from the screen and from Classic's input.
func ultraInputBox(in *Input) *Node {
	return Box(&Node{Dir: Row, Grow: 1, View: amberFill{}}, // Grow: a row gives a child neither Fixed nor Grow no width
		&Node{Fixed: 1, View: amberEdge{}},
		&Node{Grow: 1, Pad: [4]int{1, 1, 1, 1}, View: in})
}

type amberFill struct{}

func (amberFill) Draw(a *App, c *Canvas, r Rect) {
	c.Fill(r, a.Theme.Panel.Blend(ultraAmber(a.Theme), inputTint))
}

type amberEdge struct{}

func (amberEdge) Draw(a *App, c *Canvas, r Rect) {
	for y := range r.H {
		c.Set(r.X, r.Y+y, "┃", 1, term.Style{FG: ultraAmber(a.Theme)})
	}
}
