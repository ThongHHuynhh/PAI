package tui

import (
	"reflect"
	"strings"
	"testing"

	"pai/pkg/connector/tui/term"
	"pai/pkg/harness"
)

func dialog(multi bool) (*App, *askDialog, *[][]string) {
	a := &App{Theme: themeFor(term.Caps{}, ""), W: 80, H: 30}
	opts := []harness.Option{{Label: "Small", Description: "just the fix"}, {Label: "Big", Description: "a rewrite"}}
	qs := []harness.Question{{Header: "Scope", Question: "Which scope?", Options: opts}, {Header: "Extras", Question: "Which extras?", MultiSelect: multi, Options: opts}}
	d := &askDialog{a: a, qs: qs}
	for _, q := range qs {
		d.cur, d.other, d.on = append(d.cur, 0), append(d.other, ""), append(d.on, make([]bool, len(q.Options)+1))
	}
	sent := new([][]string)
	d.send = func(ans [][]string) { *sent = ans }
	return a, d, sent
}

func press(a *App, d *askDialog, keys ...term.Key) {
	for _, k := range keys {
		d.Key(a, k)
	}
}

func typed(s string) (ks []term.Key) {
	for _, r := range s {
		ks = append(ks, term.Key{Code: r})
	}
	return ks
}

// screen draws the dialog as a person sees it.
func screen(a *App, d *askDialog) string {
	l := a.Dialog("PAI asks", &Node{View: d})
	defer a.Pop(l)
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
	return strings.Join(out, "\n")
}

func TestAskDialogAnswers(t *testing.T) {
	a, d, sent := dialog(true)
	enter, down, space := term.Key{Code: term.KeyEnter}, term.Key{Code: term.KeyDown}, term.Key{Code: ' '}
	press(a, d, down, enter) // Big, a single choice: next question
	if d.tab != 1 || !reflect.DeepEqual(d.answer(0), []string{"Big"}) {
		t.Fatalf("tab %d, answer %v", d.tab, d.answer(0))
	}
	press(a, d, space, down, space, down)          // both options
	press(a, d, typed("and docs")...)              // Other
	press(a, d, term.Key{Code: term.KeyBackspace}) // one letter less
	if got := d.answer(1); !reflect.DeepEqual(got, []string{"Small", "Big", "and doc"}) {
		t.Fatalf("got %v", got)
	}
	press(a, d, enter) // to the review
	if d.tab != 2 || !d.complete() {
		t.Fatalf("tab %d complete %v", d.tab, d.complete())
	}
	press(a, d, enter)
	if want := [][]string{{"Big"}, {"Small", "Big", "and doc"}}; !reflect.DeepEqual(*sent, want) {
		t.Fatalf("sent %v", *sent)
	}
}

func TestAskDialogNeedsEveryAnswer(t *testing.T) {
	a, d, sent := dialog(false)
	press(a, d, term.Key{Code: term.KeyRight}, term.Key{Code: term.KeyRight}, term.Key{Code: term.KeyEnter})
	if *sent != nil || d.complete() {
		t.Fatal("sent with no answers")
	}
	d.tab, d.cur[0] = 0, 2
	press(a, d, typed("mine")...) // Other, single: clears nothing else
	press(a, d, term.Key{Code: term.KeyUp}, term.Key{Code: ' '})
	if got := d.answer(0); !reflect.DeepEqual(got, []string{"Big"}) {
		t.Fatalf("a single choice kept two: %v", got)
	}
}

func TestAskDialogDraws(t *testing.T) {
	a, d, _ := dialog(false)
	for _, w := range []int{80, 40, 24} {
		a.W = w
		s := screen(a, d)
		for _, want := range []string{"Which scope?", "Small", "just the fix", "Other:"} {
			if !strings.Contains(strings.ReplaceAll(s, "\n", " "), want[:min(len(want), w/4)]) {
				t.Errorf("width %d lacks %q:\n%s", w, want, s)
			}
		}
	}
}
