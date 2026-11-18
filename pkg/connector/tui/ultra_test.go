package tui

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"pai/pkg/connector/tui/term"
	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/mode"
	"pai/pkg/ultra/project"
)

// ultraFixture is a git project, an isolated ULTRA home with a library, and
// the ULTRA screen on a headless app. Every executor counts its starts, so a
// test can prove management views made no model call.
func ultraFixture(t *testing.T, w, h int) (*App, *ultra, *atomic.Int32) {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("PAI_ULTRA_HOME", home)
	lib := filepath.Join(home, "library")
	os.MkdirAll(filepath.Join(lib, "skills", "go-testing"), 0o755)
	os.WriteFile(filepath.Join(lib, "skills", "go-testing", "SKILL.md"), []byte("---\nname: go-testing\ndescription: Table-driven Go tests.\nmetadata:\n  version: \"1.0.0\"\n  tags: go,testing\n---\nUse t.Run.\n"), 0o644)
	os.MkdirAll(filepath.Join(lib, "skills", "web-ui"), 0o755)
	os.WriteFile(filepath.Join(lib, "skills", "web-ui", "SKILL.md"), []byte("---\nname: web-ui\ndescription: Web UI work.\nmetadata:\n  tags: web\n---\nBody.\n"), 0o644)
	os.MkdirAll(filepath.Join(lib, "rules"), 0o755)
	pkgRule := filepath.Join(lib, "rules", "no-push.md")
	os.WriteFile(pkgRule, []byte("---\ndescription: No push.\n---\nNever push without asking.\n"), 0o644)
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@e"}, {"config", "user.name", "t"}} {
		cmd := osexec.Command("git", args...)
		cmd.Dir = dir
		cmd.Run()
	}
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Run go vet first.\n"), 0o644)
	a := &App{Theme: themeFor(term.Caps{}, ""), W: w, H: h, Status: &Text{}, post: make(chan func(), 64)}
	u, err := openUltra(a, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	counting := exec.Scripted{Name: "claude", Caps: exec.Capabilities{Name: "claude", Tools: []string{"shell"}, AutoLoads: []string{"CLAUDE.md"}},
		Do: func(context.Context, exec.Assignment) (string, exec.Usage, error) {
			starts.Add(1)
			return "", exec.Usage{}, nil
		}}
	for name := range u.rt.Adapters {
		c := counting
		c.Name = name
		c.Caps.Name = name
		u.rt.Adapters[name] = c
	}
	u.executor = "claude"
	a.SetScreen(u)
	t.Cleanup(func() { u.rt.Close() })
	return a, u, &starts
}

// drawn draws the app and returns its text, one string per row.
func drawn(a *App) []string {
	a.screen.Size(a, a.W, a.H)
	root := a.screen.Root()
	root.Layout(Rect{0, 0, a.W, a.H})
	b := term.NewBuffer(a.W, a.H)
	root.Draw(a, &Canvas{b, Rect{0, 0, a.W, a.H}})
	for _, l := range a.layers {
		r := l.Place(a.W, a.H)
		l.Root.Layout(r)
		l.Root.Draw(a, &Canvas{b, Rect{0, 0, a.W, a.H}})
	}
	rows := make([]string, b.H)
	for y := range b.H {
		var s strings.Builder
		for x := range b.W {
			s.WriteString(b.At(x, y).G)
		}
		rows[y] = s.String()
	}
	return rows
}

func ucontains(rows []string, s string) bool {
	for _, r := range rows {
		if strings.Contains(r, s) {
			return true
		}
	}
	return false
}

func upress(a *App, keys ...string) {
	for _, k := range keys {
		a.key(uparseKey(k))
	}
}

func uparseKey(s string) term.Key {
	named := map[string]rune{"enter": term.KeyEnter, "tab": term.KeyTab, "esc": term.KeyEsc, "up": term.KeyUp, "down": term.KeyDown,
		"left": term.KeyLeft, "right": term.KeyRight, "backspace": term.KeyBackspace, "f1": term.KeyF1, "space": ' '}
	var mod term.Mod
	for {
		switch {
		case strings.HasPrefix(s, "alt+"):
			mod, s = mod|term.Alt, s[4:]
			continue
		case strings.HasPrefix(s, "ctrl+"):
			mod, s = mod|term.Ctrl, s[5:]
			continue
		case strings.HasPrefix(s, "shift+"):
			mod, s = mod|term.Shift, s[6:]
			continue
		}
		break
	}
	if r, ok := named[s]; ok {
		return term.Key{Code: r, Mod: mod}
	}
	return term.Key{Code: []rune(s)[0], Mod: mod}
}

func TestUltraShellWideAndNarrow(t *testing.T) {
	a, u, _ := ultraFixture(t, 140, 40)
	rows := drawn(a)
	// One bottom line: the mark, the views, the model; no header above.
	last := rows[len(rows)-1]
	if !strings.Contains(last, "◆ ULTRA") || !strings.Contains(last, "[Work]") || !strings.Contains(last, "Cont") || !strings.Contains(last, "claude") {
		t.Fatalf("bottom line: %q", last)
	}
	for _, gone := range []string{"config r0", "server", "PAI ULTRA ·"} {
		if ucontains(rows, gone) {
			t.Fatalf("%q is still drawn", gone)
		}
	}
	if !u.summary.Hidden {
		t.Fatal("task line shown with no task")
	}
	tk, _ := u.rt.Create(runtimeNewTask("Fix the parser", nil, "claude"))
	u.cur = tk.ID
	if rows = drawn(a); u.summary.Hidden || !ucontains(rows, "PENDING · attempt 0/3 · checks 0/0 passed") {
		t.Fatalf("task line:\n%s", strings.Join(rows, "\n"))
	}
	a.W, a.H = 50, 20
	rows = drawn(a)
	if !ucontains(rows, "[Work]") || !ucontains(rows, "◆ ULTRA") {
		t.Fatalf("narrow screen:\n%s", strings.Join(rows, "\n"))
	}
	for _, r := range rows {
		if term.Width(r) > a.W {
			t.Fatalf("row wider than the terminal: %q", r)
		}
	}
}

func TestUltraKeyboardViewsAndSearch(t *testing.T) {
	a, u, starts := ultraFixture(t, 140, 40)
	upress(a, "alt+2")
	if u.view != vSkills || !ucontains(drawn(a), "go-testing") {
		t.Fatal("alt+2 did not open Skills")
	}
	upress(a, "/", "w", "e", "b", "enter")
	l := u.lists[vSkills]
	if len(l.shown) != 1 || l.rows[l.shown[0]].Key != "web-ui" {
		t.Fatalf("search by tag: %+v", l.shown)
	}
	upress(a, "/", "esc") // clears the search
	upress(a, "j", "k", "enter")
	if !u.showDetail || !strings.Contains(strings.Join(u.detail.lines, "\n"), "~") {
		t.Fatal("enter did not inspect")
	}
	upress(a, "esc") // closes the detail
	if u.showDetail {
		t.Fatal("esc did not close the detail")
	}
	upress(a, "tab")
	if u.focus != fTabs {
		t.Fatalf("tab focus: %d", u.focus)
	}
	upress(a, "right")
	if u.view != vInstr {
		t.Fatal("right on the tabs did not move to Instructions")
	}
	upress(a, "]", "]", "]", "esc")
	if u.view != vWork {
		t.Fatalf("esc did not return to the Workspace: %d", u.view)
	}
	for i := range nViews {
		upress(a, "alt+"+string(rune('1'+i)))
		drawn(a)
	}
	if starts.Load() != 0 {
		t.Fatalf("management views started an executor %d times", starts.Load())
	}
}

func TestUltraStagedSkillApplyAndDiscard(t *testing.T) {
	a, u, starts := ultraFixture(t, 140, 40)
	upress(a, "alt+2")
	upress(a, "space") // stage enabling the selected skill
	if u.staged() != 1 || !ucontains(drawn(a), "1 staged") {
		t.Fatal("space did not stage")
	}
	if _, err := os.Stat(u.proj.Path(project.ManifestFile)); err == nil {
		t.Fatal("staging wrote the manifest")
	}
	upress(a, "d")
	if u.staged() != 0 {
		t.Fatal("d did not discard")
	}
	upress(a, "space", "a")
	if u.proj.Revision() != 1 || len(u.proj.Lock.Packages) != 1 {
		t.Fatalf("apply: revision %d, %d packages", u.proj.Revision(), len(u.proj.Lock.Packages))
	}
	if !strings.Contains(u.lists[vSkills].rows[0].State, "enabled") {
		t.Fatalf("row after apply: %+v", u.lists[vSkills].rows[0])
	}
	if starts.Load() != 0 {
		t.Fatal("apply started an executor")
	}
}

func TestUltraInvalidConfigWritesNothing(t *testing.T) {
	a, u, _ := ultraFixture(t, 140, 40)
	u.stageOrNew().Enable(project.KindSkill, "not-installed", "")
	upress(a, "alt+2", "a")
	if _, err := os.Stat(u.proj.Path(project.ManifestFile)); err == nil {
		t.Fatal("invalid configuration was written")
	}
	if !strings.Contains(u.lists[vSkills].Err, "invalid") || len(a.layers) == 0 {
		t.Fatalf("validation error not shown: %q", u.lists[vSkills].Err)
	}
}

func TestUltraExternalInstructionEditMakesOverride(t *testing.T) {
	a, u, _ := ultraFixture(t, 140, 40)
	st := u.stageOrNew()
	st.Enable(project.KindRule, "no-push", "")
	if _, err := st.Apply(); err != nil {
		t.Fatal(err)
	}
	u.stage = nil
	upress(a, "alt+3")
	l := u.lists[vInstr]
	for i, j := range l.shown {
		if strings.Contains(l.rows[j].Key, "no-push") {
			l.sel = i
		}
	}
	upress(a, "e") // read-only: refused
	if !strings.Contains(a.Status.S, "read-only") {
		t.Fatalf("edit of a package rule: %q", a.Status.S)
	}
	upress(a, "o") // override: opens the editor on the package text
	if len(a.layers) != 1 {
		t.Fatal("override did not open the editor")
	}
	upress(a, "X", "ctrl+s")
	upress(a, "a")
	ov := u.proj.OverridePath(project.KindRule, "no-push")
	b, err := os.ReadFile(ov)
	if err != nil || !strings.Contains(string(b), "X") {
		t.Fatalf("override not written: %v %q", err, b)
	}
	pkg, _ := os.ReadFile(filepath.Join(os.Getenv("PAI_ULTRA_HOME"), "library", "rules", "no-push.md"))
	if strings.Contains(string(pkg), "X") {
		t.Fatal("the package file was edited")
	}
}

func TestUltraContextShowsUnknownNativeLoad(t *testing.T) {
	a, u, _ := ultraFixture(t, 140, 40)
	os.WriteFile(filepath.Join(u.proj.Root, "CLAUDE.md"), []byte("claude rules\n"), 0o644)
	upress(a, "alt+6")
	var found bool
	for _, r := range u.lists[vContext].rows {
		if r.Key == "project:CLAUDE.md" {
			found = r.Mark == "n" && strings.Contains(r.Hint, "unknown")
		}
	}
	if !found || !ucontains(drawn(a), "tok of") {
		t.Fatalf("native load status: %+v", u.lists[vContext].rows)
	}
}

func TestUltraModeSwitchPersistsAndKeepsTasks(t *testing.T) {
	a, u, _ := ultraFixture(t, 140, 40)
	p, _ := mode.Find(u.proj.Root)
	p.SetDefault(mode.Ultra)
	tk, err := u.rt.Create(runtimeNewTask("keep me", nil, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	root := u.proj.Root
	u.input.insert(textElems("/mode classic")...)
	upress(a, "enter")
	if m, _ := mode.Default(root); m != mode.Classic {
		t.Fatal("/mode classic did not persist")
	}
	if _, ok := a.screen.(*login); !ok {
		t.Fatalf("offline switch to Classic should land on login, got %T", a.screen)
	}
	// The ULTRA task is untouched by the switch.
	u2, err := openUltra(a, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	defer u2.rt.Close()
	got, err := u2.rt.Store.Get(tk.ID)
	if err != nil || got.Mode != "ultra" || got.State != "pending" {
		t.Fatalf("task after switch: %+v %v", got, err)
	}
}

func TestUltraHelpIsContextSensitive(t *testing.T) {
	a, _, _ := ultraFixture(t, 140, 40)
	upress(a, "alt+4", "?")
	if !ucontains(drawn(a), "verify (you, as a person)") {
		t.Fatal("Memory help missing")
	}
}

func TestSlashExactNameWinsOverLongerName(t *testing.T) {
	in := NewInput()
	in.insert(textElems("/mode")...)
	l := &slash{in: in, all: commands()}
	l.update()
	if len(l.cmds) < 2 || l.cmds[l.sel].name != "mode" {
		t.Fatalf("/mode selects %q", l.cmds[l.sel].name)
	}
	in.Clear()
	in.insert(textElems("/mod")...)
	l.update()
	if l.cmds[l.sel].name != "model" {
		t.Fatal("a partial name no longer picks the first match")
	}
}

func TestUltraInputShowsWhatIsTyped(t *testing.T) {
	a, _, _ := ultraFixture(t, 140, 40)
	upress(a, "h", "e", "l", "l", "o", "space", "w", "o", "r", "l", "d")
	rows := drawn(a)
	if !ucontains(rows, "hello world") {
		t.Fatalf("typed text not drawn:\n%s", strings.Join(rows, "\n"))
	}
	for _, name := range viewShort {
		if !ucontains(rows, name) {
			t.Fatalf("tab %q not drawn", name)
		}
	}
	a.W = 60
	rows = drawn(a)
	if !ucontains(rows, "hello world") || !ucontains(rows, "Cont") {
		t.Fatalf("narrow screen:\n%s", strings.Join(rows, "\n"))
	}
}

func TestUltraWarnsOutsideAGitProject(t *testing.T) {
	if w := workspaceWarning(t.TempDir()); !strings.Contains(w, "Not a git repository") {
		t.Fatalf("warning: %q", w)
	}
	if h, err := os.UserHomeDir(); err == nil && !strings.Contains(workspaceWarning(h), "home folder") {
		t.Fatal("no home-folder warning")
	}
}

func TestUltraReplyIsMarkdownWithoutTheResultBlock(t *testing.T) {
	a, u, _ := ultraFixture(t, 100, 30)
	u.event(exec.Event{Kind: "text", Text: "I'm running **locally** in `C:\try`, a long sentence that must wrap between words and never inside one.\n\n```pai-result\n{\"status\":\"done\"}\n```"})
	rows := drawn(a)
	text := strings.Join(rows, "\n")
	if strings.Contains(text, "**") || strings.Contains(text, "pai-result") || strings.Contains(text, `"status"`) {
		t.Fatalf("raw markdown or the result block shown:\n%s", text)
	}
	if !ucontains(rows, "locally") {
		t.Fatalf("reply not shown:\n%s", text)
	}
	// Word wrapping: no row ends inside a word of the sentence.
	for _, r := range rows {
		if strings.HasSuffix(strings.TrimRight(r, " "), "betwe") || strings.HasSuffix(strings.TrimRight(r, " "), "insi") {
			t.Fatalf("wrapped inside a word: %q", r)
		}
	}
}
