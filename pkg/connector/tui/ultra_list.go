package tui

import (
	"strconv"
	"strings"

	"pai/pkg/connector/tui/term"
)

// urow is one row of a ULTRA management list. Mark and State are plain
// text, so meaning never rests on color alone.
type urow struct {
	Key    string // stable id
	Mark   string // a short state mark: "[x]", "+", "!", ...
	Label  string
	State  string // a plain-text state label
	Hint   string // muted, right of the state
	Search string // extra text "/" matches: tags, source, compatibility
	Ink    Ink    // optional emphasis, never the only signal
}

// ulist is a keyboard-first list with search and a detail pane. Arrows or
// j/k move, PgUp/PgDn page, Home/End jump, "/" searches, Enter inspects.
// The view that owns it handles the other keys.
type ulist struct {
	rows      []urow
	shown     []int // indexes into rows that match the query
	sel       int   // index into shown
	top       int
	h         int
	query     string
	searching bool
	// States shown in place of rows.
	Loading bool
	Empty   string // shown when there are no rows at all
	Err     string // a load or validation error, shown above the rows
}

func (l *ulist) set(rows []urow) {
	var key string
	if r, ok := l.current(); ok {
		key = r.Key
	}
	l.rows = rows
	l.filter()
	for i, j := range l.shown {
		if l.rows[j].Key == key {
			l.sel = i
		}
	}
}

func (l *ulist) filter() {
	l.shown = l.shown[:0]
	q := strings.ToLower(strings.TrimSpace(l.query))
	for i, r := range l.rows {
		hay := strings.ToLower(r.Label + " " + r.State + " " + r.Hint + " " + r.Search)
		if q == "" || strings.Contains(hay, q) {
			l.shown = append(l.shown, i)
		}
	}
	l.sel = min(max(0, l.sel), max(0, len(l.shown)-1))
}

func (l *ulist) current() (urow, bool) {
	if l.sel < 0 || l.sel >= len(l.shown) {
		return urow{}, false
	}
	return l.rows[l.shown[l.sel]], true
}

func (l *ulist) move(n int) {
	l.sel = min(max(0, l.sel+n), max(0, len(l.shown)-1))
}

// Key handles navigation and search. It reports whether it took the key.
func (l *ulist) Key(a *App, k term.Key) bool {
	if l.searching {
		switch k.String() {
		case "tab", "shift+tab":
			l.searching = false // keep the filter; focus moves on
			return false
		case "esc":
			l.searching, l.query = false, ""
			l.filter()
		case "enter":
			l.searching = false
		case "backspace":
			if r := []rune(l.query); len(r) > 0 {
				l.query = string(r[:len(r)-1])
				l.filter()
			}
		default:
			if k.Code > 0 && (k.Mod == 0 || k.Mod == term.Shift) {
				l.query += string(k.Code)
				l.filter()
				return true
			}
			return false
		}
		return true
	}
	switch k.String() {
	case "up", "k":
		l.move(-1)
	case "down", "j":
		l.move(1)
	case "pgup":
		l.move(-max(1, l.h-1))
	case "pgdn":
		l.move(max(1, l.h-1))
	case "home", "g":
		l.sel = 0
	case "end", "G":
		l.sel = max(0, len(l.shown)-1)
	case "/":
		l.searching, l.query = true, ""
		l.filter()
	default:
		return false
	}
	return true
}

func (l *ulist) Height(int) int { return max(1, len(l.shown)+1) }

func (l *ulist) Draw(a *App, c *Canvas, r Rect) {
	muted, text := a.Theme.muted(), a.Theme.text()
	y := r.Y
	line := func(s string, st term.Style) {
		if y < r.Y+r.H {
			c.Text(r.X, y, cellCut(s, r.W), st)
			y++
		}
	}
	if l.searching || l.query != "" {
		line("/"+l.query+cursorIf(l.searching)+"  "+itoa(len(l.shown))+" of "+itoa(len(l.rows)), term.Style{FG: a.Theme.Accent})
	}
	switch {
	case l.Loading:
		line("loading…", muted)
	case l.Err != "":
		line("error: "+l.Err, term.Style{FG: a.Theme.Error})
	}
	if !l.Loading && len(l.rows) == 0 {
		line(cmpStr(l.Empty, "empty"), muted)
		return
	}
	if !l.Loading && len(l.shown) == 0 {
		line("no match", muted)
		return
	}
	l.h = r.Y + r.H - y
	if l.sel < l.top {
		l.top = l.sel
	}
	if l.sel >= l.top+l.h {
		l.top = l.sel - l.h + 1
	}
	for i := l.top; i < len(l.shown) && y < r.Y+r.H; i++ {
		row := l.rows[l.shown[i]]
		st := text
		if row.Ink != InkNone {
			st = term.Style{FG: a.Theme.ink(row.Ink)}
		}
		pre := "  "
		if i == l.sel {
			pre = "› "
			st.Attr |= term.Bold
			c.Fill(Rect{r.X, y, r.W, 1}, a.Theme.Panel)
		}
		left := pre + padTo(row.Mark, 4) + row.Label
		right := row.State
		if row.Hint != "" && r.W > 70 {
			right += "  " + row.Hint
		}
		w := r.W
		// The state is plain text and never dropped: the label gives way
		// first, then the state is cut.
		right = cellCut(right, max(4, w/2))
		if term.Width(right)+term.Width(left)+2 > w {
			left = cellCut(left, max(8, w-term.Width(right)-2))
		}
		c.Text(r.X, y, left, st)
		if rw := term.Width(right); rw < w-term.Width(left) {
			c.Text(r.X+w-rw, y, right, muted)
		}
		y++
	}
}

func cursorIf(on bool) string {
	if on {
		return "▏"
	}
	return ""
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func itoa(n int) string { return strconv.Itoa(n) }

// cellCut cuts s to w terminal cells with an ellipsis.
func cellCut(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if term.Width(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	term.Graphemes(s, func(g string, gw int) {
		if n+gw > w-1 {
			return
		}
		b.WriteString(g)
		n += gw
	})
	return b.String() + "…"
}

func padTo(s string, w int) string {
	if n := term.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s + " "
}

// textView is lines of text that scroll: the detail pane, help, diffs.
type textView struct {
	lines []string
	top   int
	h     int
	ink   func(line string) Ink // optional per-line emphasis (diff +/-)
}

func (t *textView) set(s string) {
	t.lines = strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	t.top = 0
}

func (t *textView) Height(w int) int { return max(1, len(t.lines)) }

func (t *textView) Key(a *App, k term.Key) bool {
	switch k.String() {
	case "up", "k":
		t.top = max(0, t.top-1)
	case "down", "j":
		t.top = min(max(0, len(t.lines)-t.h), t.top+1)
	case "pgup":
		t.top = max(0, t.top-max(1, t.h-1))
	case "pgdn":
		t.top = min(max(0, len(t.lines)-t.h), t.top+max(1, t.h-1))
	case "home", "g":
		t.top = 0
	case "end", "G":
		t.top = max(0, len(t.lines)-t.h)
	default:
		return false
	}
	return true
}

func (t *textView) Draw(a *App, c *Canvas, r Rect) {
	t.h = r.H
	for i := 0; i < r.H && t.top+i < len(t.lines); i++ {
		l := t.lines[t.top+i]
		st := a.Theme.text()
		if t.ink != nil {
			if ink := t.ink(l); ink != InkNone {
				st = term.Style{FG: a.Theme.ink(ink)}
			}
		}
		c.Text(r.X, r.Y+i, cellCut(strings.ReplaceAll(l, "\t", "  "), r.W), st)
	}
	if len(t.lines) > r.H && r.W > 10 {
		pos := itoa(t.top+1) + "-" + itoa(min(len(t.lines), t.top+r.H)) + "/" + itoa(len(t.lines))
		c.Text(r.X+r.W-term.Width(pos), r.Y+r.H-1, pos, a.Theme.muted())
	}
}

// diffInk colors a diff's added and removed lines; the +/- stays the signal.
func diffInk(l string) Ink {
	switch {
	case strings.HasPrefix(l, "+"):
		return InkAccent
	case strings.HasPrefix(l, "-"):
		return InkError
	case strings.HasPrefix(l, "---"):
		return InkMuted
	}
	return InkNone
}
