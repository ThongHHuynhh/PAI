package tui

import (
	"strings"
	"testing"
	"time"

	"pai/pkg/connector"
	"pai/pkg/connector/tui/term"
	"pai/pkg/harness"
	"pai/pkg/statefile"
)

func TestSessionTreeSidebar(t *testing.T) {
	now := time.Now()
	child := harness.Info{ID: "child", Parent: "parent", Name: "Child", Updated: now, Config: statefile.Conf{Model: "test/model"}}
	parent := harness.Info{ID: "parent", Name: "Parent", Updated: now}
	orphan := harness.Info{ID: "orphan", Parent: "missing", Name: "Orphan", Updated: now}
	l := &sessionList{infos: []harness.Info{child, parent, orphan}, seen: map[string]time.Time{}, loaded: true}
	l.cur = &session{id: parent.ID, info: parent, list: l}
	a := &App{Theme: dark, MouseX: -1, MouseY: -1}
	t.Cleanup(func() { a.Ticking(false) })
	r := Rect{0, 0, sidebarWidth, 8}
	b := term.NewBuffer(r.W, r.H)
	l.Draw(a, &Canvas{b: b, clip: r}, r)
	line := func(y int) string {
		var s strings.Builder
		for x := range r.W {
			s.WriteString(b.At(x, y).G)
		}
		return s.String()
	}
	if !strings.HasPrefix(line(0), "┃ ◆ Parent") || !strings.HasPrefix(line(3), "    Child") || !strings.HasPrefix(line(4), "    test/model") || !strings.HasPrefix(line(6), "  Orphan") {
		t.Fatalf("wrong indentation or marker:\n%s\n%s\n%s\n%s", line(0), line(3), line(4), line(6))
	}
	if !strings.HasSuffix(strings.TrimSpace(line(3)), "now ●") {
		t.Fatalf("child's unread mark or right alignment lost: %q", line(3))
	}
	if b.At(0, 0).FG != a.Theme.Accent {
		t.Fatal("open session lost its selection mark")
	}
	// The same ordered index drives drawing, hit testing and keyboard steps,
	// including when only part of a session is visible.
	hitRect := Rect{0, 2, r.W, 4}
	for top := 0; top <= l.maxTop(hitRect.H); top++ {
		l.top = top
		for y := hitRect.Y; y < hitRect.Y+hitRect.H; y++ {
			at := top + y - hitRect.Y
			want := at / sessionRows
			if at%sessionRows == sessionRows-1 || want >= len(l.infos) {
				want = -1
			}
			if got := l.row(y, hitRect); got != want {
				t.Fatalf("top %d, row %d: got %d, want %d", top, y, got, want)
			}
		}
	}
	// A list refresh moves the open child but keeps it selected by ID and
	// requests a reveal. Raw infos stay newest first for configuration defaults.
	l.cur.id, l.cur.info = child.ID, child
	l.update(a, []harness.Info{orphan, parent, child}, nil)
	if l.cur.id != child.ID || l.index(child.ID) != 2 || !l.reveal || l.infos[0].ID != orphan.ID {
		t.Fatal("refresh lost the selection, display order, reveal or raw order")
	}
	rows := connector.SessionTree(l.infos)
	if rows[l.index(child.ID)-1].ID != parent.ID {
		t.Fatal("keyboard predecessor of the child is not its parent")
	}
	l.top = 0
	l.Draw(a, &Canvas{b: b, clip: hitRect}, hitRect)
	if l.top != 4 || l.row(hitRect.Y+2, hitRect) != l.index(child.ID) || l.reveal {
		t.Fatal("refresh did not scroll the selected child into view")
	}
	child.Status = statefile.StatusStepping
	l.update(a, []harness.Info{parent, child}, nil)
	if total, stepping := l.children(parent.ID); total != 1 || stepping != 0 {
		t.Fatal("child count did not use the open session's current status")
	}
	l.cur.id, l.cur.info = parent.ID, parent
	if total, stepping := l.children(parent.ID); total != 1 || stepping != 1 {
		t.Fatal("stepping child was not counted")
	}
	child.Status = statefile.StatusFailed
	l.update(a, []harness.Info{parent, child}, nil)
	if total, stepping := l.children(parent.ID); total != 1 || stepping != 0 {
		t.Fatal("refresh after a cascade stop left the child stepping")
	}
}
