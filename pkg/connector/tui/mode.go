package tui

import (
	"context"
	"os"
	"strings"

	"pai/pkg/connector"
	"pai/pkg/connector/tui/term"
	"pai/pkg/ultra/handoff"
	"pai/pkg/ultra/mode"
)

func projectOf(dir string) (mode.Project, error) { return mode.Find(dir) }

// RunUltra opens the TUI on PAI ULTRA for dir. The client may be nil: ULTRA
// works offline with native executors. When ULTRA cannot open, the TUI opens
// on Classic (or its login) and says why.
func RunUltra(c *connector.Client, dir, note string) (update string, err error) {
	t, err := term.Open()
	if err != nil {
		return "", err
	}
	defer t.Close()
	defer t.Recover()
	a := NewApp(t)
	u, uerr := openUltra(a, c, dir)
	switch {
	case uerr == nil:
		a.SetScreen(u)
	case c == nil:
		a.SetScreen(newLogin(a))
	default:
		a.SetScreen(newSession(a, newSessionList(a, c), ""))
		askHarness(a, c)
	}
	if uerr != nil {
		a.Say("PAI ULTRA could not open (" + uerr.Error() + "); this is PAI Classic")
	} else if note != "" {
		a.Say(note)
	}
	a.Run()
	return a.Update, nil
}

// modeMenu is /mode in Classic. "/mode classic" and "/mode ultra" act at
// once; "/mode" alone opens the picker. Choosing makes no model call and
// does not touch a running session: it keeps the mode it was made in.
func (s *session) modeMenu(a *App) {
	arg := strings.TrimSpace(s.input.Text())
	switch arg {
	case "classic", "ultra":
		s.input.Clear()
		s.setMode(a, mode.Mode(arg))
		return
	}
	dir, _ := os.Getwd()
	def, derr := mode.Default(dir)
	pinned := mode.Of(s.info.Config)
	hint := func(m mode.Mode) string {
		h := ""
		if m == def {
			h = "default for new sessions"
		}
		if s.id != "" && m == pinned {
			h = strings.TrimPrefix(h+", this session", ", ")
		}
		return h
	}
	m := &Menu{Items: []MenuItem{
		{Label: "PAI Classic", Hint: hint(mode.Classic), Pick: func(a *App, _ *MenuItem) { s.setMode(a, mode.Classic) }},
		{Label: "PAI ULTRA", Hint: hint(mode.Ultra), Pick: func(a *App, _ *MenuItem) { s.setMode(a, mode.Ultra) }},
	}}
	if s.id != "" && pinned == mode.Classic {
		m.Items = append(m.Items, MenuItem{Label: "Continue this session in ULTRA", Hint: "a new ULTRA task from its messages; this session is kept and linked",
			Pick: func(a *App, _ *MenuItem) { s.continueInUltra(a) }})
	}
	if derr != nil {
		a.Say("mode settings unreadable (" + derr.Error() + "): Classic is in use")
	}
	a.OpenMenu("Mode", m)
}

// setMode makes m the project's default for new sessions. ULTRA opens its
// screen; this session is left as it is.
func (s *session) setMode(a *App, m mode.Mode) {
	dir, err := os.Getwd()
	if err == nil {
		var p mode.Project
		if p, err = mode.Find(dir); err == nil {
			err = p.SetDefault(m)
		}
	}
	if err != nil {
		a.SayError(err)
		return
	}
	if m == mode.Classic {
		a.Say("PAI Classic is the default for new sessions here")
		return
	}
	u, err := openUltra(a, s.c, dir)
	if err != nil {
		a.Say("PAI ULTRA could not open (" + err.Error() + "); staying in Classic")
		return
	}
	a.SetScreen(u)
}

// continueInUltra explains what does not travel, then makes an ULTRA task
// from this session's messages and opens it. The session is not changed.
func (s *session) continueInUltra(a *App) {
	dir, _ := os.Getwd()
	plan, err := handoff.PlanFromClassic(context.Background(), s.c, s.id)
	if err != nil {
		a.SayError(err)
		return
	}
	tv := &textView{}
	tv.set("Objective:\n" + plan.Objective + "\n\nNot carried:\n- " + strings.Join(plan.Unsupported, "\n- ") + "\n\nEnter creates the ULTRA task (it does not start) · Esc cancels")
	var l *Layer
	l = a.Dialog("Continue in PAI ULTRA?", &Node{View: confirm{tv, func() {
		a.Pop(l)
		u, err := openUltra(a, s.c, dir)
		if err != nil {
			a.Say("PAI ULTRA could not open (" + err.Error() + "); staying in Classic")
			return
		}
		t, err := handoff.ApplyFromClassic(u.rt.Coordinator, plan)
		if err != nil {
			a.SayError(err)
			if t == nil {
				return
			}
		}
		u.cur = t.ID
		u.say("task "+t.ID+" from Classic session "+s.id+" (kept and linked). Enter a message, or /resume to start it.", InkAccent)
		a.SetScreen(u)
	}}})
}

// offerHandoff puts a pending ULTRA handoff in an empty input of the Classic
// session it was made for. The person sends it; nothing is sent for them.
func (s *session) offerHandoff(a *App) {
	if s.id == "" || len(s.input.els) > 0 {
		return
	}
	text, offer, ok := ultraPending(s.id)
	if !ok {
		return
	}
	s.input.insert(textElems(text)...)
	offer()
	a.Say("The handoff from PAI ULTRA is in the input: Enter sends it")
}

// modeLabel is the session's pinned mode, for the status line.
func (s *session) modeLabel() string {
	if s.id != "" && s.info.ID == s.id {
		return mode.Of(s.info.Config).Label()
	}
	return mode.Classic.Label()
}
