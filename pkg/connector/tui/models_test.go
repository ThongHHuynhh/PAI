package tui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pai/pkg/connector"
	"pai/pkg/connector/tui/term"
	"pai/pkg/harness"
)

var sessionModels = []harness.SessionModel{
	{Model: "anthropic/opus", Name: "Claude Opus", Provider: "anthropic", ContextLimit: 200000, Current: true, OK: true},
	{Model: "openai/mini", Name: "GPT mini", Provider: "openai", ContextLimit: 128000, Reason: "context 150k does not fit 102.4k (80% of 128k context window)"},
	{Model: "openai/max", Name: "GPT max", Provider: "openai", ContextLimit: 400000, OK: true},
}

// drawLayer draws the top layer as a person sees it: its rows, and the
// buffer and rect to read styles from.
func drawLayer(a *App) ([]string, *term.Buffer, Rect) {
	l := a.layers[len(a.layers)-1]
	r := l.Place(a.W, a.H)
	l.Root.Layout(r)
	b := term.NewBuffer(a.W, a.H)
	l.Root.Draw(a, &Canvas{b, Rect{0, 0, a.W, a.H}})
	var out []string
	for y := r.Y; y < r.Y+r.H; y++ {
		var s strings.Builder
		for x := r.X; x < r.X+r.W; x++ {
			s.WriteString(b.At(x, y).G)
		}
		out = append(out, strings.TrimRight(s.String(), " "))
	}
	return out, b, r
}

func TestSessionModelMenuDraws(t *testing.T) {
	a := &App{Theme: themeFor(term.Caps{}, ""), H: 30, Status: &Text{}}
	s := &session{id: "s1", ctx: context.Background()}
	a.OpenMenu("Model for this session", s.modelMenu(sessionModels))
	for _, w := range []int{40, 100} {
		a.W = w
		rows, b, r := drawLayer(a)
		ends := map[int]bool{}
		for _, want := range []string{"Claude Opus", "GPT mini", "GPT max"} {
			y := -1
			for i, row := range rows {
				if strings.Contains(row, want) {
					y = i
				}
			}
			if y < 0 {
				t.Fatalf("width %d lacks %q:\n%s", w, want, strings.Join(rows, "\n"))
			}
			ends[term.Width(rows[y])] = true
			x := r.X + strings.Index(rows[y], want)
			if muted := b.At(x, r.Y+y).FG == a.Theme.Muted; muted != (want == "GPT mini") {
				t.Errorf("width %d: %q muted %v", w, want, muted)
			}
		}
		if len(ends) != 1 {
			t.Errorf("width %d: hints end in columns %v, want one:\n%s", w, ends, strings.Join(rows, "\n"))
		}
		all := strings.Join(rows, "\n")
		if !strings.Contains(all, "current · 200k") || !strings.Contains(all, "context 150k") {
			t.Errorf("width %d lacks the current mark or the reason:\n%s", w, all)
		}
	}
}

func TestSessionModelMenuSwaps(t *testing.T) {
	put := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		put <- r.Method + " " + r.URL.Path + " " + string(b)
	}))
	defer srv.Close()
	a := &App{Theme: themeFor(term.Caps{}, ""), W: 100, H: 30, Status: &Text{}, post: make(chan func(), 8)}
	s := &session{c: &connector.Client{Local: srv.URL}, id: "s1", ctx: context.Background()}
	m := s.modelMenu(sessionModels)
	a.OpenMenu("Model for this session", m)
	if it := m.Items[m.rows[m.sel].item]; it.Label != "Claude Opus" {
		t.Fatalf("opens on %q, not the current model", it.Label)
	}
	m.Key(a, term.Key{Code: term.KeyDown}) // past GPT mini, which cannot take the session
	m.Key(a, term.Key{Code: term.KeyEnter})
	if got := <-put; got != "PUT /sessions/s1/model openai/max" {
		t.Fatalf("sent %q", got)
	}
	(<-a.post)()
	if a.Status.S != "Model: GPT max, applies from the next step" {
		t.Fatalf("status %q", a.Status.S)
	}
}
