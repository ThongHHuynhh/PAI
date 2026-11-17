package coord

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/metrics"
	"pai/pkg/ultra/task"
)

const done = "```pai-result\n{\"status\":\"done\",\"summary\":\"wrote it\"}\n```"

// repo is a git repository with one commit, and an isolated ULTRA home.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("PAI_ULTRA_HOME", t.TempDir())
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}, {"config", "core.autocrlf", "false"}} {
		if out, err := gitRun(dir, args...); err != nil {
			t.Fatal(out, err)
		}
	}
	os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644)
	gitRun(dir, "add", ".")
	gitRun(dir, "commit", "-qm", "init")
	return dir
}

// script records each assignment it was given and runs step(n, a).
type script struct {
	mu   sync.Mutex
	seen []exec.Assignment
	step func(n int, a exec.Assignment) string
}

func (s *script) adapter(caps exec.Capabilities) exec.Scripted {
	return exec.Scripted{Name: "fake", Caps: caps, Do: func(ctx context.Context, a exec.Assignment) (string, exec.Usage, error) {
		s.mu.Lock()
		s.seen = append(s.seen, a)
		n := len(s.seen)
		s.mu.Unlock()
		return s.step(n, a), exec.Usage{Input: 100, Output: 10, Reported: true}, nil
	}}
}

func open(t *testing.T, dir string, s *script, caps exec.Capabilities) *Coordinator {
	t.Helper()
	caps.Name = "fake"
	if caps.Tools == nil {
		caps.Tools = []string{"shell", "files"}
	}
	c, err := Open(dir, map[string]exec.Adapter{"fake": s.adapter(caps)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func fileCrit(path, contains string) task.Criterion {
	return task.Criterion{ID: "out", Kind: task.CriterionFile, Path: path, Contains: contains}
}

func TestDoneIsAClaimRetryThenAccept(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(n int, a exec.Assignment) string {
		if n == 2 {
			os.WriteFile(filepath.Join(a.Workdir, "out.txt"), []byte("hello"), 0o644)
		}
		return "claimed done\n" + done // says done both times
	}}
	c := open(t, dir, s, exec.Capabilities{Resume: true})
	tk, err := c.Create(NewTask{Objective: "write out.txt", Criteria: []task.Criterion{fileCrit("out.txt", "hello")}, Executor: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Run(context.Background(), tk.ID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Task.State != task.Accepted || out.Task.Attempts != 2 {
		t.Fatalf("state %s after %d attempts", out.Task.State, out.Task.Attempts)
	}
	// The retry's context told the executor what failed.
	if !strings.Contains(s.seen[1].Prompt, "out.txt is missing") {
		t.Fatalf("retry prompt lacks the failure:\n%s", s.seen[1].Prompt)
	}
	// Unchanged workspace between attempts? No: attempt 1 wrote nothing, so native resume was used.
	if s.seen[1].Resume == "" {
		t.Fatal("unchanged workspace did not reuse the native session")
	}
	ms, _ := c.Metrics.Read()
	sum := metrics.Summarize(ms)
	if sum.Attempts != 2 || sum.Accepted != 1 || sum.ValidationFailures != 1 || sum.BackgroundCalls != 0 || ms[0].ExecutorClaim != "done" || ms[0].Outcome != "retry" {
		t.Fatalf("metrics: %+v %+v", sum, ms)
	}
}

func TestValidationFailureDespiteSuccessFailsTask(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(int, exec.Assignment) string { return done }}
	c := open(t, dir, s, exec.Capabilities{})
	tk, _ := c.Create(NewTask{Objective: "x", Criteria: []task.Criterion{{ID: "cmd", Kind: task.CriterionCommand, Command: "exit 3"}}, Executor: "fake"})
	out, err := c.Run(context.Background(), tk.ID, "", nil)
	if err == nil || out.Task.State != task.Failed || out.Task.Attempts != 3 {
		t.Fatalf("state %s attempts %d err %v", out.Task.State, out.Task.Attempts, err)
	}
	if out.Validation.Results[0].Exit != 3 {
		t.Fatalf("exit not recorded: %+v", out.Validation.Results[0])
	}
}

func TestReviewWaitsForAPerson(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(int, exec.Assignment) string { return done }}
	c := open(t, dir, s, exec.Capabilities{})
	tk, _ := c.Create(NewTask{Objective: "x", Criteria: []task.Criterion{{ID: "ux", Kind: task.CriterionReview, Text: "looks right"}}, Executor: "fake"})
	_, err := c.Run(context.Background(), tk.ID, "", nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("review: %v", err)
	}
	if _, err := c.Approve(context.Background(), tk.ID, "ux", "executor:fake", "fine"); err == nil {
		t.Fatal("executor approved a review")
	}
	got, err := c.Approve(context.Background(), tk.ID, "ux", "person", "checked the screen")
	if err != nil || got.State != task.Accepted {
		t.Fatalf("approve: %v %v", got.State, err)
	}
}

func TestExecutorBlockedStops(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(int, exec.Assignment) string {
		return "```pai-result\n{\"status\":\"blocked\",\"summary\":\"need the API key\",\"open\":[\"which key?\"]}\n```"
	}}
	c := open(t, dir, s, exec.Capabilities{})
	tk, _ := c.Create(NewTask{Objective: "x", Executor: "fake"})
	out, err := c.Run(context.Background(), tk.ID, "", nil)
	if !errors.Is(err, ErrBlocked) || out.Task.State != task.Blocked || !strings.Contains(strings.Join(out.Task.Blockers, " "), "which key?") {
		t.Fatalf("blocked: %v %+v", err, out.Task)
	}
}

func TestCrashRecoveryAndResumeWithChangedWorkspace(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(n int, a exec.Assignment) string {
		os.WriteFile(filepath.Join(a.Workdir, "out.txt"), []byte("hello"), 0o644)
		return done
	}}
	c := open(t, dir, s, exec.Capabilities{Resume: true})
	tk, _ := c.Create(NewTask{Objective: "write", Criteria: []task.Criterion{fileCrit("out.txt", "hello")}, Executor: "fake"})
	// A previous process claimed, started an executor run, and died.
	dead := task.Owner{ID: "dead"}
	got, _ := c.Store.Claim(tk.ID, dead, 100*time.Millisecond)
	c.Store.StartAttempt(tk.ID, got.Epoch, "fake", 0)
	op, _ := c.Store.BeginOp(tk.ID, got.Epoch, task.Op{Kind: "executor_run", Desc: "fake attempt 1"})
	c.Store.SetOp(tk.ID, got.Epoch, op.ID, task.OpStarted, "native-1", "")
	c.Store.Update(tk.ID, task.Guard{Epoch: got.Epoch}, func(t *task.Task) (task.Event, error) {
		t.Native = map[string]string{"fake": "native-1"}
		return task.Event{Type: "test"}, nil
	})
	c.Store.Checkpoint(tk.ID)
	// It may have written part of its work.
	os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("half"), 0o644)
	time.Sleep(150 * time.Millisecond)
	// Recovery does not replay the uncertain run.
	if ids, _ := c.Recover(); len(ids) != 1 {
		t.Fatalf("recover: %v", ids)
	}
	if _, err := c.Run(context.Background(), tk.ID, "", nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("run over an unknown op: %v", err)
	}
	if len(s.seen) != 0 {
		t.Fatal("executor ran before reconciliation")
	}
	c.Store.ResolveOp(tk.ID, op.ID, task.OpFailed, "inspected: only partial.txt")
	out, err := c.Run(context.Background(), tk.ID, "", nil)
	if err != nil || out.Task.State != task.Accepted {
		t.Fatalf("resume: %v %v", err, out.Task.State)
	}
	// The workspace changed since the checkpoint: a fresh executor with a bounded handoff.
	a := s.seen[0]
	if a.Resume != "" || !strings.Contains(a.Prompt, "CHANGED") || !strings.Contains(a.Prompt, "partial.txt") {
		t.Fatalf("resume assignment: resume=%q\n%s", a.Resume, a.Prompt)
	}
}

func TestMissingCapabilityIsWarned(t *testing.T) {
	dir := repo(t)
	home := os.Getenv("PAI_ULTRA_HOME")
	os.MkdirAll(filepath.Join(home, "library", "skills", "browse"), 0o755)
	os.WriteFile(filepath.Join(home, "library", "skills", "browse", "SKILL.md"), []byte("---\nname: browse\ndescription: Use a browser.\nmetadata:\n  requires: browser\n---\nbody\n"), 0o644)
	s := &script{step: func(int, exec.Assignment) string { return done }}
	c := open(t, dir, s, exec.Capabilities{Tools: []string{"shell"}})
	st := c.Project.Begin()
	st.Enable("skill", "browse", "")
	if _, err := st.Apply(); err != nil {
		t.Fatal(err)
	}
	tk, _ := c.Create(NewTask{Objective: "x", Executor: "fake"})
	cx, _, err := c.Context(tk, "fake")
	if err != nil || len(cx.Warnings) == 0 || !strings.Contains(strings.Join(cx.Warnings, " "), "browser") {
		t.Fatalf("warnings: %v %v", cx.Warnings, err)
	}
	if cx.ConfigRev != 1 {
		t.Fatalf("config revision not pinned: %d", cx.ConfigRev)
	}
}

func TestDelegateAndIntegrate(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(n int, a exec.Assignment) string {
		os.WriteFile(filepath.Join(a.Workdir, "README"), []byte("x\nchild\n"), 0o644)
		os.WriteFile(filepath.Join(a.Workdir, "new.txt"), []byte("new"), 0o644)
		return done
	}}
	c := open(t, dir, s, exec.Capabilities{})
	parent, _ := c.Create(NewTask{Objective: "parent", Executor: "fake"})
	child, err := c.Delegate(parent.ID, NewTask{Objective: "child", Criteria: []task.Criterion{fileCrit("new.txt", "new")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Integrate(context.Background(), child.ID); err == nil {
		t.Fatal("integrated unaccepted work")
	}
	if _, err := c.Run(context.Background(), child.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Fatal("child wrote into the parent workspace")
	}
	files, err := c.Integrate(context.Background(), child.ID)
	if err != nil || len(files) != 2 {
		t.Fatalf("integrate: %v %v", files, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "README"))
	if !strings.Contains(string(b), "child") {
		t.Fatal("patch not applied")
	}
	if _, err := c.Delegate(child.ID, NewTask{Objective: "grandchild"}); err == nil {
		t.Fatal("child delegated again")
	}
}

func TestIntegrateTakesTheAcceptedSnapshotWithCommits(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(n int, a exec.Assignment) string {
		os.WriteFile(filepath.Join(a.Workdir, "lib.txt"), []byte("committed\n"), 0o644)
		gitRun(a.Workdir, "add", "lib.txt")
		gitRun(a.Workdir, "commit", "-qm", "child commit")
		os.WriteFile(filepath.Join(a.Workdir, "README"), []byte("x\nuncommitted\n"), 0o644)
		return done
	}}
	c := open(t, dir, s, exec.Capabilities{})
	parent, _ := c.Create(NewTask{Objective: "parent", Executor: "fake"})
	child, err := c.Delegate(parent.ID, NewTask{Objective: "child", Criteria: []task.Criterion{fileCrit("lib.txt", "committed")}})
	if err != nil || child.Base == "" {
		t.Fatalf("delegate: %v base %q", err, child.Base)
	}
	if _, err := c.Run(context.Background(), child.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	// An edit after acceptance must not travel.
	os.WriteFile(filepath.Join(child.Worktree, "late.txt"), []byte("late"), 0o644)
	if _, err := c.Integrate(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "lib.txt")); string(b) != "committed\n" {
		t.Fatal("child's commit not integrated")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README")); !strings.Contains(string(b), "uncommitted") {
		t.Fatal("child's uncommitted change not integrated")
	}
	if _, err := os.Stat(filepath.Join(dir, "late.txt")); err == nil {
		t.Fatal("post-acceptance edit integrated")
	}
}

type uncertain struct{ exec.Scripted }

func (uncertain) Start(context.Context, exec.Assignment) (exec.Run, error) {
	return nil, &exec.UncertainError{Ref: "pai session s1", Err: errors.New("connection reset")}
}

func TestUncertainStartBlocksInsteadOfRetrying(t *testing.T) {
	dir := repo(t)
	c, err := Open(dir, map[string]exec.Adapter{"u": uncertain{exec.Scripted{Name: "u"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tk, _ := c.Create(NewTask{Objective: "x", Executor: "u"})
	if _, err := c.Run(context.Background(), tk.ID, "", nil); err == nil {
		t.Fatal("uncertain start reported success")
	}
	got, _ := c.Store.Get(tk.ID)
	if got.State != task.Blocked || len(got.Unknown()) != 1 || got.Unknown()[0].Ref != "pai session s1" {
		t.Fatalf("after uncertain start: %s %+v", got.State, got.Ops)
	}
}

func TestUnreadableCheckpointPreventsNativeResume(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(n int, a exec.Assignment) string { return done }}
	c := open(t, dir, s, exec.Capabilities{Resume: true})
	tk, _ := c.Create(NewTask{Objective: "x", Criteria: []task.Criterion{fileCrit("never.txt", "")}, Executor: "fake"})
	c.Store.Update(tk.ID, task.Guard{}, func(t *task.Task) (task.Event, error) {
		t.Native = map[string]string{"fake": "old"}
		t.Checkpoints = []string{"cp-missing"}
		return task.Event{Type: "test"}, nil
	})
	c.Run(context.Background(), tk.ID, "", nil)
	if s.seen[0].Resume != "" {
		t.Fatal("resumed a native session without a readable checkpoint")
	}
}

func TestBlockedClaimIsStillChecked(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(n int, a exec.Assignment) string {
		os.WriteFile(filepath.Join(a.Workdir, "out.txt"), []byte("hello"), 0o644)
		return "```pai-result\n{\"status\":\"blocked\",\"summary\":\"could not run the tests\"}\n```"
	}}
	c := open(t, dir, s, exec.Capabilities{})
	tk, _ := c.Create(NewTask{Objective: "x", Criteria: []task.Criterion{fileCrit("out.txt", "hello")}, Executor: "fake"})
	out, err := c.Run(context.Background(), tk.ID, "", nil)
	if err != nil || out.Task.State != task.Accepted {
		t.Fatalf("work done but claimed blocked: %v %s", err, out.Task.State)
	}
}

func TestIntegrateUsesTheAcceptedCheckpointNotANewerOne(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(n int, a exec.Assignment) string {
		os.WriteFile(filepath.Join(a.Workdir, "lib.txt"), []byte("v1\n"), 0o644)
		gitRun(a.Workdir, "add", "lib.txt")
		gitRun(a.Workdir, "commit", "-qm", "c1")
		os.WriteFile(filepath.Join(a.Workdir, "lib.txt"), []byte("v2\n"), 0o644) // uncommitted, on top of the commit
		return done
	}}
	c := open(t, dir, s, exec.Capabilities{})
	parent, _ := c.Create(NewTask{Objective: "parent", Executor: "fake"})
	child, _ := c.Delegate(parent.ID, NewTask{Objective: "child", Criteria: []task.Criterion{fileCrit("lib.txt", "v2")}})
	if _, err := c.Run(context.Background(), child.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	// After acceptance the child's tree changes and a newer checkpoint exists.
	os.WriteFile(filepath.Join(child.Worktree, "lib.txt"), []byte("tampered\n"), 0o644)
	if _, err := c.Store.Checkpoint(child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Integrate(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "lib.txt")); string(b) != "v2\n" {
		t.Fatalf("integrated %q, want the accepted v2 (commit then uncommitted change on top)", b)
	}
}

func TestAmendedReviewNeedsNewApproval(t *testing.T) {
	dir := repo(t)
	s := &script{step: func(int, exec.Assignment) string { return done }}
	c := open(t, dir, s, exec.Capabilities{})
	tk, _ := c.Create(NewTask{Objective: "x", Criteria: []task.Criterion{{ID: "ux", Kind: task.CriterionReview, Text: "looks right"}}, Executor: "fake"})
	c.Run(context.Background(), tk.ID, "", nil)
	got, _ := c.Store.Get(tk.ID)
	if !approvedFor(got) {
		// Approve, then amend the same criterion id: the old approval must not count.
		c.Store.Update(tk.ID, task.Guard{}, func(t *task.Task) (task.Event, error) {
			t.Decisions = append(t.Decisions, approvalPrefix("ux", t.Attempts, t.CriteriaHash)+" by person: ok")
			return task.Event{Type: "test"}, nil
		})
	}
	c.Store.AmendCriteria(tk.ID, task.Guard{}, []task.Criterion{{ID: "ux", Kind: task.CriterionReview, Text: "now also accessible"}}, "person", "stricter")
	got, _ = c.Store.Get(tk.ID)
	if approved(got, "ux") {
		t.Fatal("an approval survived an amendment of the criterion")
	}
}

func approvedFor(t *task.Task) bool { return approved(t, "ux") }
