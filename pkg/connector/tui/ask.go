package tui

import (
	"context"
	"strings"

	"pai/pkg/connector/tui/term"
	"pai/pkg/harness"
)

// askDialog is the questions of an ask call, one tab each and a last tab to
// review and submit. Each question lists its options and an Other row that
// takes typed text. Esc declines.
type askDialog struct {
	a     *App
	qs    []harness.Question
	tab   int
	cur   []int    // the row the cursor is on, per question; the last row is Other
	on    [][]bool // per question, per option and then Other: chosen
	other []string // per question, the text typed for Other
	hits  [][2]int // per row drawn: its question and its row, -1 for none
	err   string
	send  func([][]string) // submits the answers
}

func (d *askDialog) q() harness.Question { return d.qs[d.tab] }

// done is whether question i has an answer.
func (d *askDialog) done(i int) bool { return len(d.answer(i)) > 0 }

// answer is what is chosen for question i: the labels, then the typed text.
func (d *askDialog) answer(i int) []string {
	var out []string
	for j, o := range d.qs[i].Options {
		if d.on[i][j] {
			out = append(out, o.Label)
		}
	}
	if t := strings.TrimSpace(d.other[i]); d.on[i][len(d.qs[i].Options)] && t != "" {
		out = append(out, t)
	}
	return out
}

func (d *askDialog) complete() bool {
	for i := range d.qs {
		if !d.done(i) {
			return false
		}
	}
	return true
}

// set marks row j of the current question; on a single choice it clears the
// rest.
func (d *askDialog) set(j int, v bool) {
	on := d.on[d.tab]
	if v && !d.q().MultiSelect {
		clear(on)
	}
	on[j] = v
}

// choose picks row j of the current question; on a multiple choice it flips it.
func (d *askDialog) choose(j int) { d.set(j, !d.q().MultiSelect || !d.on[d.tab][j]) }

func (d *askDialog) next() {
	d.tab = min(d.tab+1, len(d.qs))
}

// rows is the lines of the tab at width w, for Height and Draw.
func (d *askDialog) rows(tab, w int) (out []Line, hits [][2]int) {
	a := d.a
	text, muted := a.Theme.text(), a.Theme.muted()
	bold := term.Style{FG: a.Theme.Text, Attr: term.Bold}
	accent := term.Style{FG: a.Theme.Accent, Attr: term.Bold}
	add := func(l []Line, q, row int) {
		for _, x := range l {
			out, hits = append(out, x), append(hits, [2]int{q, row})
		}
	}
	blank := func() { add([]Line{{}}, -1, -1) }
	var tabs []Glyph
	for i := 0; i <= len(d.qs); i++ {
		label, mark := "Submit", " "
		if i < len(d.qs) {
			label = d.qs[i].Header
			if d.done(i) {
				mark = "✓"
			}
		}
		st := muted
		if i == tab {
			st = accent
		}
		tabs = cat(tabs, run(mark+" "+label+"  ", st))
	}
	add(flow(tabs, w, nil, nil), -1, -1)
	blank()
	if tab == len(d.qs) {
		add(flow(run("Review your answers", bold), w, nil, nil), -1, -1)
		blank()
		for i, q := range d.qs {
			ans := "(no answer)"
			if d.done(i) {
				ans = strings.Join(d.answer(i), ", ")
			}
			add(flow(cat(run(q.Header+": ", muted), run(ans, text)), w, nil, run("  ", text)), -1, -1)
		}
		blank()
		if d.complete() {
			add(flow(run("Press enter to send these answers.", accent), w, nil, nil), -1, -1)
		} else {
			add(flow(run("Answer every question first.", term.Style{FG: a.Theme.ink(InkError)}), w, nil, nil), -1, -1)
		}
	} else {
		q := d.qs[tab]
		add(flow(run(q.Question, bold), w, nil, nil), -1, -1)
		if q.MultiSelect {
			add(flow(run("Pick any number.", muted), w, nil, nil), -1, -1)
		}
		blank()
		for j := 0; j <= len(q.Options); j++ {
			label, desc := "Other", ""
			if j < len(q.Options) {
				label, desc = q.Options[j].Label, q.Options[j].Description
			}
			box := "( ) "
			switch {
			case q.MultiSelect && d.on[tab][j]:
				box = "[x] "
			case q.MultiSelect:
				box = "[ ] "
			case d.on[tab][j]:
				box = "(•) "
			}
			st, pre := text, "  "
			if j == d.cur[tab] {
				st, pre = accent, "› "
			}
			if j == len(q.Options) {
				label = "Other: " + d.other[tab]
			}
			add(flow(run(label, st), w, cat(run(pre, st), run(box, st)), run("      ", st)), tab, j)
			if desc != "" {
				add(flow(run(desc, muted), w, run("      ", muted), run("      ", muted)), tab, j)
			}
		}
	}
	blank()
	hint := "↑↓ choose · space or enter select · ←→ question · esc decline"
	if tab == len(d.qs) {
		hint = "enter send · ←→ question · esc decline"
	}
	add(flow(run(hint, muted), w, nil, nil), -1, -1)
	if d.err != "" {
		add(flow(run(d.err, term.Style{FG: a.Theme.ink(InkError)}), w, nil, nil), -1, -1)
	}
	return out, hits
}

func (d *askDialog) Height(w int) int {
	h := 0
	for t := 0; t <= len(d.qs); t++ {
		rs, _ := d.rows(t, w)
		h = max(h, len(rs))
	}
	return h
}

func (d *askDialog) Draw(a *App, c *Canvas, r Rect) {
	rs, hits := d.rows(d.tab, r.W)
	d.hits = hits
	for y, l := range rs {
		if y >= r.H {
			break
		}
		x := r.X
		for _, g := range l.Gs {
			c.Set(x, r.Y+y, g.G, g.W, g.St)
			x += g.W
		}
		if d.tab < len(d.qs) && hits[y] == [2]int{d.tab, len(d.q().Options)} && d.cur[d.tab] == hits[y][1] && (y+1 == len(hits) || hits[y+1] != hits[y]) {
			c.Cursor(x, r.Y+y) // after the typed text
		}
	}
}

func (d *askDialog) Key(a *App, k term.Key) bool {
	d.err = ""
	last := len(d.qs)
	switch k.String() {
	case "left", "shift+tab":
		d.tab = max(d.tab-1, 0)
	case "right", "tab":
		d.next()
	case "up":
		if d.tab < last {
			d.cur[d.tab] = max(d.cur[d.tab]-1, 0)
		}
	case "down":
		if d.tab < last {
			d.cur[d.tab] = min(d.cur[d.tab]+1, len(d.q().Options))
		}
	case "enter":
		d.enter()
	case "space":
		if d.tab < last && d.cur[d.tab] < len(d.q().Options) {
			d.choose(d.cur[d.tab])
		} else if d.tab < last {
			d.typed(" ")
		}
	case "backspace":
		if d.tab < last && d.cur[d.tab] == len(d.q().Options) {
			rs := []rune(d.other[d.tab])
			if len(rs) > 0 {
				d.setOther(string(rs[:len(rs)-1]))
			}
		}
	case "esc":
		return false
	default:
		if t := k.Text(); t != "" && d.tab < last {
			d.typed(t)
		}
	}
	return true
}

// typed adds text to Other, when the cursor is on it.
func (d *askDialog) typed(t string) {
	if d.cur[d.tab] == len(d.q().Options) {
		d.setOther(d.other[d.tab] + t)
	}
}

// setOther sets the typed text; text chooses Other, and none unchooses it.
func (d *askDialog) setOther(t string) {
	d.other[d.tab] = t
	d.set(len(d.q().Options), strings.TrimSpace(t) != "")
}

func (d *askDialog) enter() {
	switch {
	case d.tab == len(d.qs):
		if d.complete() {
			d.send(d.answers())
		}
	case d.cur[d.tab] == len(d.q().Options):
		if d.done(d.tab) {
			d.next()
		}
	case d.q().MultiSelect:
		if !d.done(d.tab) {
			d.choose(d.cur[d.tab])
		}
		d.next()
	default:
		d.choose(d.cur[d.tab])
		d.next()
	}
}

func (d *askDialog) answers() [][]string {
	out := make([][]string, len(d.qs))
	for i := range d.qs {
		out[i] = d.answer(i)
	}
	return out
}

func (d *askDialog) Paste(_ *App, t string) {
	if d.tab < len(d.qs) {
		line, _, _ := strings.Cut(t, "\n")
		d.typed(line)
	}
}

func (d *askDialog) Mouse(a *App, m term.Mouse, r Rect) bool {
	if m.Action != term.Press || m.Button != term.Left || m.Y < r.Y || m.Y-r.Y >= len(d.hits) {
		return false
	}
	if h := d.hits[m.Y-r.Y]; h[0] == d.tab && h[1] >= 0 {
		d.cur[d.tab] = h[1]
		if h[1] < len(d.q().Options) {
			d.choose(h[1])
		}
	}
	return true
}

// asking is the dialog of the session's open ask call.
type asking struct {
	call  string
	layer *Layer
}

// syncAsk opens the dialog for an ask call that waits while the session steps,
// and closes it when the call has its result, or the session no longer steps.
// A declined call is not asked again.
func (s *session) syncAsk(a *App) {
	if s.dlg != nil && (s.askCall == nil || s.dlg.call != s.askCall.ID || !s.stepping()) {
		s.closeAsk(a)
	}
	if s.askCall == nil || s.dlg != nil || !s.stepping() || s.askCall.ID == s.declined {
		return
	}
	qs := harness.Questions(*s.askCall)
	if len(qs) == 0 {
		return
	}
	call, id := s.askCall.ID, s.id
	d := &askDialog{a: a, qs: qs}
	for _, q := range qs {
		d.cur, d.other, d.on = append(d.cur, 0), append(d.other, ""), append(d.on, make([]bool, len(q.Options)+1))
	}
	reply := func(answers [][]string) {
		a.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), callTime)
			defer cancel()
			if err := s.c.Answer(ctx, id, via, call, answers); err != nil {
				a.Post(func() { d.err = err.Error() })
			}
		})
	}
	d.send = reply
	l := a.Dialog("PAI asks", &Node{View: d})
	l.Esc = func() {
		s.dlg, s.declined = nil, call
		reply(nil)
	}
	s.dlg = &asking{call, l}
}

func (s *session) closeAsk(a *App) {
	if s.dlg != nil {
		a.Pop(s.dlg.layer)
		s.dlg = nil
	}
}
