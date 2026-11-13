package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"pai/pkg/harness"
	"pai/pkg/statefile"
)

// The trace (11.1, 11.2) is where the newest turn's time goes, step by step,
// read from the records alone: no progress is made up. /trace opens it, so
// the transcript stays clean; the info bar under the input sums it up (11.3).

// span is one model step or one tool call of the newest turn.
type span struct {
	name, where string    // "model" and the model, or the tool and its worker
	call        string    // the call's id
	start, end  time.Time // end is zero while it runs
	prep        time.Duration
	attempts    int
	usage       statefile.Usage
	note        string // the call in a line, or why it ended
}

// turnTrace is the spans of the newest turn and when it began.
type turnTrace struct {
	start time.Time
	mark  time.Time // the newest step or result: the next span starts here
	done  bool      // the turn ended: the next message from a person starts one
	spans []*span
	calls map[string]*span
}

// person is whether a message came from a person, not a job, a retry or
// another session.
func person(r statefile.Record) bool {
	return r.Via != "job" && r.Via != "retry" && !strings.HasPrefix(r.Via, "session:")
}

// add folds a record into the trace.
func (t *turnTrace) add(r statefile.Record, b *callBlock) {
	switch r.Kind {
	case statefile.Message:
		if person(r) && (t.calls == nil || t.done) {
			*t = turnTrace{start: r.Time, mark: r.Time, calls: map[string]*span{}}
		}
	case statefile.Step:
		if t.calls == nil {
			return
		}
		sp := &span{name: "model", where: r.Model, start: r.Sent, end: r.Time, attempts: r.Attempts}
		if sp.start.IsZero() {
			sp.start = t.mark
		}
		sp.prep = max(0, sp.start.Sub(t.mark))
		if r.Usage != nil {
			sp.usage = *r.Usage
		}
		// the step's calls came before it, while it streamed; they run after it
		i := len(t.spans)
		for i > 0 && t.spans[i-1].name != "model" && t.spans[i-1].end.IsZero() {
			t.spans[i-1].start = r.Time
			i--
		}
		t.spans = append(t.spans[:i], append([]*span{sp}, t.spans[i:]...)...)
		t.mark, t.done = r.Time, i == len(t.spans)-1
	case statefile.Call:
		if t.calls == nil || b == nil {
			return
		}
		sp := &span{name: b.name, where: b.arg("worker"), call: r.ID, start: r.Time, note: b.summary()}
		t.spans = append(t.spans, sp)
		t.calls[r.ID] = sp
	case statefile.Error:
		t.done = true
	case statefile.Result:
		if sp, ok := t.calls[r.For]; ok {
			sp.start, sp.end = later(sp.start, t.mark), r.Time
			t.mark = r.Time
		}
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// running is the first call with no result yet.
func (t *turnTrace) running() *span {
	for _, sp := range t.spans {
		if sp.name != "model" && sp.end.IsZero() {
			return sp
		}
	}
	return nil
}

// totals is the turn's time on the model, on tools and on building
// requests, and its tokens.
func (t *turnTrace) totals(now time.Time) (model, tools, prep time.Duration, u statefile.Usage) {
	for _, sp := range t.spans {
		end := sp.end
		if end.IsZero() {
			end = now
		}
		if sp.name == "model" {
			model += end.Sub(sp.start)
			prep += sp.prep
			u.Input += sp.usage.Input
			u.Output += sp.usage.Output
			u.CacheRead += sp.usage.CacheRead
			u.CacheWrite += sp.usage.CacheWrite
		} else {
			tools += max(0, end.Sub(sp.start))
		}
	}
	return
}

// cached is the share of the prompt read from the provider's cache.
func cached(u statefile.Usage) string {
	if u.Prompt() == 0 {
		return ""
	}
	return fmt.Sprintf("cache %d%%", u.CacheRead*100/u.Prompt())
}

// infoBar is the turn in one line: how long, on what, and the cache.
func (s *session) infoBar() string {
	t := &s.trace
	if t.start.IsZero() {
		return ""
	}
	now := time.Now()
	end := t.mark
	if s.stepping() {
		end = now
	}
	model, tools, _, u := t.totals(now)
	parts := []string{fmt.Sprintf("turn %s (model %s, tools %s)", dur(end.Sub(t.start)), dur(model), dur(tools))}
	if c := cached(u); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, " · ")
}

// traceText is the trace panel: every span of the turn, what runs now and
// until when, then the totals.
func (s *session) traceText() string {
	t := &s.trace
	if t.start.IsZero() {
		return "No turn yet: send a message."
	}
	now := time.Now()
	ex := map[string]harness.ExecutionInfo{}
	for _, e := range s.info.Executions {
		ex[e.Call] = e
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Turn from %s\n\n", t.start.Local().Format(time.TimeOnly))
	for _, sp := range t.spans {
		start, took := sp.start, ""
		if e, ok := ex[sp.call]; ok && !e.Started.IsZero() {
			start = e.Started
		}
		if sp.end.IsZero() {
			took = "running " + dur(now.Sub(start).Truncate(time.Second))
		} else {
			took = dur(max(0, sp.end.Sub(start)))
		}
		where := sp.where
		if sp.name == "model" {
			where = strings.TrimPrefix(where[strings.LastIndex(where, "/")+1:], "claude-")
		}
		fmt.Fprintf(&b, "%-7s %-12s %8s", cutTo(sp.name, 7), cutTo(where, 12), took)
		switch {
		case sp.name == "model":
			var notes []string
			if sp.attempts > 1 {
				notes = append(notes, fmt.Sprintf("%d attempts", sp.attempts))
			}
			if c := cached(sp.usage); c != "" {
				notes = append(notes, fmt.Sprintf("%s in, %s", tokens(sp.usage.Prompt()), c))
			}
			if len(notes) > 0 {
				b.WriteString("  " + strings.Join(notes, ", "))
			}
		case sp.note != "":
			b.WriteString("  " + cutTo(sp.note, 24))
		}
		b.WriteString("\n")
		if e, ok := ex[sp.call]; ok && e.Active && sp.end.IsZero() {
			fmt.Fprintf(&b, "        %s on %s", e.Stage, e.Worker)
			if e.Deadline > 0 {
				fmt.Fprintf(&b, ", deadline in %s", dur(max(0, time.UnixMilli(e.Deadline).Sub(now)).Truncate(time.Second)))
			}
			b.WriteString("\n")
		}
	}
	if s.stepping() && t.running() == nil {
		fmt.Fprintf(&b, "%-7s %-12s %8s\n", "model", "waiting", dur(now.Sub(t.mark).Truncate(time.Second)))
	}
	model, tools, prep, u := t.totals(now)
	fmt.Fprintf(&b, "\nModel %s · tools %s · building requests %s\n", dur(model), dur(tools), dur(prep))
	fmt.Fprintf(&b, "Tokens %s in, %s out", tokens(u.Prompt()), tokens(u.Output))
	if c := cached(u); c != "" {
		b.WriteString(" · " + c)
	}
	return b.String()
}

// cutTo is s in at most n characters.
func cutTo(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n-1]) + "…"
	}
	return s
}

// live is text made anew at every frame, so a dialog follows the session.
type live struct{ f func() string }

func (l live) Height(w int) int { return (&Text{S: l.f()}).Height(w) }

func (l live) Draw(a *App, c *Canvas, r Rect) { (&Text{S: l.f(), Ink: InkText}).Draw(a, c, r) }

// traceDialog opens the trace until Esc.
func (s *session) traceDialog(a *App) {
	a.Dialog("Trace: where the time goes", &Node{View: live{s.traceText}})
}

// usageEvery is how often the provider usage bar is read again.
const usageEvery = time.Minute

// usageBar is the session's provider account: whether it is limited, and how
// much of its 5 hour and 7 day windows is used.
func (s *session) usageBar() string {
	p, _, _ := strings.Cut(s.info.Config.Model, "/")
	for _, ac := range s.accounts {
		if ac.Name == p || strings.HasPrefix(ac.Name, p+"/") {
			return p + " · " + usageHint(ac)
		}
	}
	return ""
}

// readAccounts reads the accounts' usage now and then while the screen is
// on the session.
func (s *session) readAccounts(a *App) {
	a.Go(func() {
		for s.ctx.Err() == nil {
			ctx, cancel := context.WithTimeout(s.ctx, callTime)
			var accounts []harness.Account
			err := s.c.Call(ctx, "GET", "/accounts", "", nil, &accounts)
			cancel()
			if err == nil {
				a.Post(func() { s.accounts = accounts })
			}
			select {
			case <-s.ctx.Done():
			case <-time.After(usageEvery):
			}
		}
	})
}

// recoverMenu is what can be done about the session now, and whether each
// can repeat an action (11.4).
func (s *session) recoverMenu(a *App) {
	if s.id == "" {
		a.Say("No session yet")
		return
	}
	m, id := &Menu{}, s.id
	for _, b := range s.running {
		call := b.call.ID
		m.Items = append(m.Items, MenuItem{Section: "Running", Label: "Cancel " + b.name + " " + b.arg("worker"), Hint: "may have partly run; the task goes on", Pick: func(a *App, _ *MenuItem) {
			a.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), callTime)
				defer cancel()
				err := s.c.CancelCall(ctx, id, call, via)
				a.Post(func() {
					if err != nil {
						a.SayError(err)
					}
				})
			})
		}})
	}
	add := func(label, hint string, ok bool, why string, f func(*session, *App)) {
		if !ok {
			hint = why
		}
		m.Items = append(m.Items, MenuItem{Section: "Task", Label: label, Hint: hint, Disabled: !ok, Pick: func(a *App, _ *MenuItem) { f(s, a) }})
	}
	add("Stop the task", "ends its programs; finished actions stay done", s.stepping(), "not working", (*session).halt)
	add("Retry or resume", "steps on from the last record; repeats nothing", !s.stepping(), "working now", (*session).retry)
	add("Change model", "from the next step; history kept", true, "", (*session).models)
	add("Narrow the task", "write what to keep; a change it planned waits for it", true, "", func(s *session, a *App) { s.input.Paste(a, "Narrow the task to: ") })
	add("Fork from here", "a copy to try another way; this one stays", !s.stepping(), "working now", (*session).fork)
	add("Roll back to before a message", "a copy from then; done actions are not undone", len(s.asked) > 0, "no message yet", (*session).rollback)
	a.OpenMenu("Recover", m)
}

// retry steps the session on from its tip.
func (s *session) retry(a *App) {
	id := s.id
	a.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), callTime)
		defer cancel()
		err := s.c.Retry(ctx, id)
		a.Post(func() {
			if err != nil {
				a.SayError(err)
			}
		})
	})
}

// autoModel turns auto model switching on or off for the session (7.1): the
// model proposes a model per task and asks before each switch.
func (s *session) autoModel(a *App) {
	if s.id == "" || s.info.ID != s.id {
		a.Say("Send a message first: auto model switching is per session")
		return
	}
	id, on := s.id, !s.info.Config.AutoModel
	a.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), callTime)
		defer cancel()
		err := s.c.SetAutoModel(ctx, id, via, on)
		a.Post(func() {
			switch {
			case err != nil:
				a.SayError(err)
			case on:
				a.Say("Auto model switching on: PAI asks before each switch")
			default:
				a.Say("Auto model switching off")
			}
		})
	})
}
