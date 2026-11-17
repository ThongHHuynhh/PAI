package handoff

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pai/pkg/harness"
	"pai/pkg/statefile"
	"pai/pkg/ultra/coord"
	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/task"
)

// fakeClassic is a Classic server with one session, recording every create
// and counting posts (there must be none: a handoff makes no model call).
type fakeClassic struct {
	info    harness.Info
	recs    []statefile.Record
	created []harness.Create
	posts   int
}

func (f *fakeClassic) Info(context.Context, string) (harness.Info, error) { return f.info, nil }
func (f *fakeClassic) Records(context.Context, string, int, string) ([]statefile.Record, error) {
	return f.recs, nil
}
func (f *fakeClassic) Sessions(context.Context, string, int) ([]harness.Info, error) {
	return []harness.Info{f.info}, nil
}
func (f *fakeClassic) Create(_ context.Context, cr harness.Create, _ string) (string, error) {
	f.created = append(f.created, cr)
	return "classic-new", nil
}

func rec(k statefile.Kind, body string) statefile.Record {
	return statefile.Record{Header: statefile.Header{Kind: k, Type: "text/plain"}, Body: []byte(body)}
}

func setup(t *testing.T) (*coord.Coordinator, *fakeClassic) {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("PAI_ULTRA_HOME", t.TempDir())
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@e"}, {"config", "user.name", "t"}} {
		cmd := osexec.Command("git", a...)
		cmd.Dir = dir
		cmd.Run()
	}
	os.WriteFile(filepath.Join(dir, "a"), []byte("1"), 0o644)
	for _, a := range [][]string{{"add", "."}, {"commit", "-qm", "i"}} {
		cmd := osexec.Command("git", a...)
		cmd.Dir = dir
		cmd.Run()
	}
	c, err := coord.Open(dir, map[string]exec.Adapter{"fake": exec.Scripted{Name: "fake"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := &fakeClassic{info: harness.Info{ID: "classic-1", Status: statefile.StatusClosed, Config: statefile.Conf{Model: "m/x", Tools: "default"}},
		recs: []statefile.Record{rec(statefile.Message, "Add a dark theme"), rec(statefile.Text, "I added tokens in theme.css"), rec(statefile.Message, "Also the sidebar")}}
	return c, f
}

func TestClassicToUltraKeepsTheSessionAndCallsNoModel(t *testing.T) {
	c, f := setup(t)
	p, err := PlanFromClassic(context.Background(), f, "classic-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Objective, "Add a dark theme") || !strings.Contains(p.Objective, "Follow-up 1: Also the sidebar") || !strings.Contains(p.Objective, "unverified") || len(p.Unsupported) == 0 {
		t.Fatalf("plan: %+v", p)
	}
	tk, err := ApplyFromClassic(c, p)
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != task.Pending || len(tk.Checkpoints) != 1 || tk.Links[0] != "classic:classic-1" {
		t.Fatalf("task: %+v", tk)
	}
	pr, _ := c.Project.LoadPrefs()
	if len(pr.LinksOf("classic:classic-1")) != 1 || len(f.created) != 0 || f.posts != 0 {
		t.Fatal("link missing, or the Classic side was changed")
	}
}

func TestUltraSessionIsNotPlannedAsClassic(t *testing.T) {
	_, f := setup(t)
	f.info.Config.Mode = "ultra"
	if _, err := PlanFromClassic(context.Background(), f, "x"); err == nil {
		t.Fatal("an ULTRA-pinned session was treated as Classic")
	}
}

func TestUltraToClassicAtASafeBoundary(t *testing.T) {
	c, f := setup(t)
	tk, _ := c.Create(coord.NewTask{Objective: "Finish the parser", Executor: "fake", Criteria: []task.Criterion{{ID: "t", Kind: task.CriterionCommand, Command: "go test ./..."}}})
	// While held by an owner, no handoff.
	held, _ := c.Store.Claim(tk.ID, task.Owner{ID: "o"}, task.DefaultLease)
	if _, err := TaskToClassic(context.Background(), f, c, tk.ID); err == nil {
		t.Fatal("handed off a held task")
	}
	c.Store.Release(tk.ID, held.Epoch, task.Paused, "")
	out, err := TaskToClassic(context.Background(), f, c, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Session != "classic-new" || len(f.created) != 1 || f.created[0].Mode != "" || f.created[0].Model != "m/x" {
		t.Fatalf("created: %+v", f.created)
	}
	if !strings.Contains(out.Text, "Finish the parser") || !strings.Contains(out.Text, "go test ./...") || f.posts != 0 {
		t.Fatalf("handoff text or a post: %s", out.Text)
	}
	got, _ := c.Store.Get(tk.ID)
	if got.State != task.HandedOff || got.Links[0] != "classic:classic-new" {
		t.Fatalf("task after handoff: %s %v", got.State, got.Links)
	}
	text, offer, ok := PendingFor(c.Project.Project, "classic-new")
	if !ok || text != out.Text {
		t.Fatal("pending handoff not found for the new session")
	}
	offer()
	if _, _, again := PendingFor(c.Project.Project, "classic-new"); again {
		t.Fatal("handoff offered twice")
	}
}
