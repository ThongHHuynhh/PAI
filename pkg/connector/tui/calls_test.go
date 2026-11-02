package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"pai/pkg/connector/tui/term"
	"pai/pkg/statefile"
)

func callRec(id, code string) statefile.Record {
	return statefile.Record{Header: statefile.Header{Kind: statefile.Call, ID: id, Time: time.Now()}, Body: []byte(fmt.Sprintf(`{"name":"run","arguments":{"worker":"admin","code":%q}}`, code))}
}

func resRec(id string, exit int, out string) statefile.Record {
	return statefile.Record{Header: statefile.Header{Kind: statefile.Result, For: id, Exit: &exit}, Body: []byte(out)}
}

// callSession is a session that has shown a message, three calls of which
// one failed, thinking and steps between them, then text.
func callSession() *session {
	s := &session{transcript: NewScrollbox(), calls: map[string]*callBlock{}}
	s.transcript.Gap = func(prev, next Block) bool { return !isCall(prev) || !isCall(next) }
	for _, r := range []statefile.Record{
		{Header: statefile.Header{Kind: statefile.Message, Via: "tui"}, Body: []byte("check the repo")},
		{Header: statefile.Header{Kind: statefile.Thinking}, Body: []byte("hmm")},
		callRec("c1", "git status"), resRec("c1", 0, "clean"),
		{Header: statefile.Header{Kind: statefile.Step}},
		callRec("c2", "git push"), resRec("c2", 1, "exit 1\nrejected"),
		callRec("c3", "ls -la /home/tom/projects/pai"), resRec("c3", 0, "go.mod"),
		{Header: statefile.Header{Kind: statefile.Text}, Body: []byte("done")},
	} {
		s.show(r)
	}
	return s
}

func drawTranscript(a *App, s *session, w int) []string {
	b := term.NewBuffer(w, a.H)
	s.transcript.Draw(a, &Canvas{b, Rect{0, 0, w, a.H}}, Rect{0, 0, w, a.H})
	var out []string
	for y := range a.H {
		var l strings.Builder
		for x := range w {
			l.WriteString(b.At(x, y).G)
		}
		out = append(out, strings.TrimRight(l.String(), " "))
	}
	return out
}

func rowOf(rows []string, sub string) int {
	for i, r := range rows {
		if strings.Contains(r, sub) {
			return i
		}
	}
	return -1
}

func TestCallsFoldToOneLine(t *testing.T) {
	a := &App{Theme: themeFor(term.Caps{}, ""), H: 30}
	s := callSession()
	rows := drawTranscript(a, s, 100)
	all := strings.Join(rows, "\n")
	if rowOf(rows, "  ▸ 3 tool calls · 1 failed · run on admin: ls -la /home/tom/projects/pai") < 0 {
		t.Fatalf("no group line:\n%s", all)
	}
	if strings.Contains(all, "git status") || strings.Contains(all, "rejected") || !strings.Contains(all, "check the repo") || !strings.Contains(all, "done") {
		t.Fatalf("closed group shows its calls, or lost the message or text:\n%s", all)
	}
	s.show(callRec("c4", "git log"))
	if len(s.transcript.items) != 8 { // message, group, 3 calls, text, and the new run's group and call
		t.Fatalf("text did not end the run: %d items", len(s.transcript.items))
	}
	s.info.Status = statefile.StatusStepping
	rows = drawTranscript(a, s, 100)
	if y := rowOf(rows, "▸ 1 tool call · run on admin: git log"); y < 0 || !strings.HasSuffix(rows[y], "0s") {
		t.Fatalf("running call not shown on its group line:\n%s", strings.Join(rows, "\n"))
	}
}

func TestCallsGroupOpens(t *testing.T) {
	a := &App{Theme: themeFor(term.Caps{}, ""), H: 30}
	s := callSession()
	before := drawTranscript(a, s, 100)
	y := rowOf(before, "▸ 3 tool calls")
	s.transcript.click(a, s.transcript.items[1])
	rows := drawTranscript(a, s, 100)
	all := strings.Join(rows, "\n")
	if rowOf(rows, "▾ 3 tool calls · 1 failed") != y || strings.Contains(rows[y], "ls -la") {
		t.Fatalf("open group line moved from row %d or still shows the latest call:\n%s", y, all)
	}
	for _, want := range []string{"run admin  git status", "└ clean", "run admin  git push", "exit 1 · rejected", "run admin  ls -la", "done"} {
		if !strings.Contains(all, want) {
			t.Fatalf("open group lacks %q:\n%s", want, all)
		}
	}
	for i := range y {
		if rows[i] != before[i] {
			t.Fatalf("row %d above the group moved:\n%s", i, all)
		}
	}
	s.transcript.click(a, s.transcript.items[1])
	if got := drawTranscript(a, s, 100); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("closed again differs:\n%s", strings.Join(got, "\n"))
	}
}

func TestCallsGroupNarrow(t *testing.T) {
	a := &App{Theme: themeFor(term.Caps{}, ""), H: 30}
	s := callSession()
	rows := drawTranscript(a, s, 40)
	y := rowOf(rows, "▸ 3 tool calls")
	if y < 0 || !strings.HasSuffix(rows[y], "…") || term.Width(rows[y]) > 40-1 {
		t.Fatalf("group line not cut to width 40:\n%s", strings.Join(rows, "\n"))
	}
	s.show(callRec("c4", "git log"))
	s.show(callRec("c5", "git diff"))
	rows = drawTranscript(a, s, 40)
	if rowOf(rows, "▸ 2 tool calls · run on admin: git d…") < 0 {
		t.Fatalf("new calls do not update one line:\n%s", strings.Join(rows, "\n"))
	}
}
