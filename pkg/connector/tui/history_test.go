package tui

import (
	"fmt"
	"testing"

	"pai/pkg/connector/tui/term"
)

func TestRecall(t *testing.T) {
	s := NewInput()
	s.History = func() []string { return []string{"first", "second\nline two"} }
	s.Paste(nil, "draft")
	steps := []struct {
		dir  int
		ok   bool
		want string
	}{
		{-1, true, "second\nline two"},
		{-1, true, "first"},
		{-1, false, "first"}, // the oldest: stays
		{1, true, "second\nline two"},
		{1, true, "draft"}, // past the newest: the person's own text
		{1, false, "draft"},
	}
	for i, st := range steps {
		if ok := s.recall(st.dir); ok != st.ok || s.Text() != st.want {
			t.Fatalf("step %d: recall(%d) = %v, %q; want %v, %q", i, st.dir, ok, s.Text(), st.ok, st.want)
		}
	}
}

// Up steps back only from the first row, so it still moves within a message
// of several lines; an edit makes the shown entry the person's own text.
func TestUpKey(t *testing.T) {
	s := NewInput()
	s.view.W = 40
	s.History = func() []string { return []string{"old"} }
	s.Paste(nil, "a\nb") // cursor on the second row
	up := term.Key{Code: term.KeyUp}
	s.Key(nil, up)
	if s.Text() != "a\nb" {
		t.Fatalf("Up on the second row recalled: %q", s.Text())
	}
	s.Key(nil, up)
	if s.Text() != "old" {
		t.Fatalf("Up on the first row shows %q, want old", s.Text())
	}
	s.Key(nil, term.Key{Code: '!'})
	if s.hist != -1 || s.Text() != "old!" {
		t.Fatalf("after an edit: hist %d, text %q", s.hist, s.Text())
	}
}

func TestRemember(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	a := &App{}
	for i := range historyMax + 5 {
		a.Remember(fmt.Sprintf("m%d", i))
	}
	a.Remember(fmt.Sprintf("m%d", historyMax+4)) // the same again: kept once
	a.Remember("multi\nline")
	b := &App{}
	h := b.History()
	if len(h) != historyMax || h[0] != "m6" || h[len(h)-1] != "multi\nline" || h[len(h)-2] != fmt.Sprintf("m%d", historyMax+4) {
		t.Fatalf("history read back: %d entries, first %q, last %q", len(h), h[0], h[len(h)-1])
	}
}
