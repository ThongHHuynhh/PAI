package task

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newStore(t *testing.T) (*Store, *clock) {
	c := &clock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	s := NewStore(t.TempDir())
	s.now = c.now
	return s, c
}

func mk(t *testing.T, s *Store) *Task {
	t.Helper()
	tk := &Task{Objective: "Fix the parser\nso it accepts tabs", Criteria: []Criterion{{ID: "test", Kind: CriterionCommand, Command: "go test ./..."}}, Workspace: t.TempDir(), Mode: "ultra", MaxAttempts: 3}
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestTransitions(t *testing.T) {
	if !CanMove(Pending, Claimed) || CanMove(Accepted, Pending) || CanMove(Running, Accepted) {
		t.Fatal("transition table")
	}
	if !Accepted.Terminal() || Paused.Terminal() {
		t.Fatal("terminal states")
	}
	s, _ := newStore(t)
	tk := mk(t, s)
	if tk.Title != "Fix the parser" || tk.State != Pending || tk.Rev != 1 {
		t.Fatalf("created: %+v", tk)
	}
	if _, err := s.Move(tk.ID, Guard{}, Accepted, "x", ""); !errors.Is(err, ErrTransition) {
		t.Fatalf("pending -> accepted allowed: %v", err)
	}
}

func TestConcurrentClaimsOneWins(t *testing.T) {
	s, _ := newStore(t)
	tk := mk(t, s)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, held := 0, 0
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Claim(tk.ID, Owner{ID: string(rune('a' + i))}, DefaultLease)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrHeld) || errors.Is(err, ErrTransition):
				held++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || held != 7 {
		t.Fatalf("wins %d, refused %d", wins, held)
	}
}

func TestStaleOwnerIsFenced(t *testing.T) {
	s, c := newStore(t)
	tk := mk(t, s)
	a, err := s.Claim(tk.ID, Owner{ID: "a"}, DefaultLease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(tk.ID, Guard{Epoch: a.Epoch}, Running, "a", ""); err != nil {
		t.Fatal(err)
	}
	// a's process stalls past its lease; b takes over.
	c.add(2 * DefaultLease)
	b, err := s.Claim(tk.ID, Owner{ID: "b"}, DefaultLease)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if b.Epoch == a.Epoch || b.Owner.ID != "b" {
		t.Fatal("no new epoch")
	}
	// a wakes up and tries to report: refused.
	if _, err := s.Move(tk.ID, Guard{Epoch: a.Epoch}, Reported, "a", "done"); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale owner write: %v", err)
	}
	if err := s.Renew(tk.ID, a.Epoch, DefaultLease); !errors.Is(err, ErrStaleOwner) {
		t.Fatal("stale renew accepted")
	}
	if err := s.Renew(tk.ID, b.Epoch, DefaultLease); err != nil {
		t.Fatal(err)
	}
}

func TestCrashRecoveryMarksUnknownAndBlocks(t *testing.T) {
	s, c := newStore(t)
	tk := mk(t, s)
	a, _ := s.Claim(tk.ID, Owner{ID: "a"}, DefaultLease)
	s.Move(tk.ID, Guard{Epoch: a.Epoch}, Running, "a", "")
	op, err := s.BeginOp(tk.ID, a.Epoch, Op{Kind: "executor_run", Desc: "claude run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOp(tk.ID, a.Epoch, op.ID, OpStarted, "sess-1", ""); err != nil {
		t.Fatal(err)
	}
	// The process dies. Nothing renews.
	c.add(2 * DefaultLease)
	ids, err := s.Recover()
	if err != nil || len(ids) != 1 {
		t.Fatalf("recover: %v %v", ids, err)
	}
	got, _ := s.Get(tk.ID)
	if got.State != Blocked || got.Owner != nil || got.Op(op.ID).State != OpUnknown || got.Op(op.ID).Ref != "sess-1" {
		t.Fatalf("after recovery: %+v", got)
	}
	// It cannot be claimed and blindly replayed.
	if _, err := s.Claim(tk.ID, Owner{ID: "b"}, DefaultLease); err == nil {
		t.Fatal("blocked task with an unknown op was claimed")
	}
	// A person reconciles it; then it resumes.
	if _, err := s.ResolveOp(tk.ID, op.ID, OpDone, "patch is in the tree"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(tk.ID)
	if got.State != Paused || len(got.Blockers) != 0 {
		t.Fatalf("after reconcile: %s %v", got.State, got.Blockers)
	}
	if _, err := s.Claim(tk.ID, Owner{ID: "b"}, DefaultLease); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotentOpPausesOnly(t *testing.T) {
	s, c := newStore(t)
	tk := mk(t, s)
	a, _ := s.Claim(tk.ID, Owner{ID: "a"}, DefaultLease)
	s.BeginOp(tk.ID, a.Epoch, Op{Kind: "validate", Desc: "go test", Idempotent: true})
	c.add(2 * DefaultLease)
	s.Recover()
	got, _ := s.Get(tk.ID)
	if got.State != Paused {
		t.Fatalf("idempotent op blocked the task: %s", got.State)
	}
}

func TestLogWinsOverStaleSnapshot(t *testing.T) {
	s, _ := newStore(t)
	tk := mk(t, s)
	snap, _ := os.ReadFile(filepath.Join(s.dir(tk.ID), taskFile))
	if _, err := s.Move(tk.ID, Guard{}, Blocked, "x", "waiting"); err != nil {
		t.Fatal(err)
	}
	// Crash after the event was appended but before the snapshot was replaced.
	os.WriteFile(filepath.Join(s.dir(tk.ID), taskFile), snap, 0o644)
	// And a torn half line from a later crash.
	f, _ := os.OpenFile(filepath.Join(s.dir(tk.ID), eventsFile), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"rev":9,"task":{"st`)
	f.Close()
	got, err := s.Get(tk.ID)
	if err != nil || got.State != Blocked || got.Rev != 2 {
		t.Fatalf("recovered %+v %v", got, err)
	}
}

func TestRevisionGuard(t *testing.T) {
	s, _ := newStore(t)
	tk := mk(t, s)
	if _, err := s.Move(tk.ID, Guard{Rev: tk.Rev}, Blocked, "ui", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(tk.ID, Guard{Rev: tk.Rev}, Cancelled, "ui", ""); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale view applied: %v", err)
	}
}

func TestCriteriaCannotChangeSilently(t *testing.T) {
	s, _ := newStore(t)
	tk := mk(t, s)
	_, err := s.Update(tk.ID, Guard{}, func(t *Task) (Event, error) {
		t.Criteria = nil // an executor's result trying to weaken the bar
		return Event{Type: "report"}, nil
	})
	if err == nil {
		t.Fatal("criteria dropped without an amendment")
	}
	if _, err := s.AmendCriteria(tk.ID, Guard{}, nil, "person", ""); err == nil {
		t.Fatal("amendment without a reason")
	}
	got, err := s.AmendCriteria(tk.ID, Guard{}, []Criterion{{ID: "lint", Kind: CriterionCommand, Command: "go vet ./..."}}, "person", "tests are flaky upstream")
	if err != nil || len(got.Decisions) != 1 {
		t.Fatalf("amend: %v %+v", err, got)
	}
	evs, _ := s.Events(tk.ID)
	if evs[len(evs)-1].Type != "criteria_amended" || !strings.Contains(string(evs[len(evs)-1].Data), "go test") {
		t.Fatal("old criteria not kept in the log")
	}
}

func TestDependencies(t *testing.T) {
	s, _ := newStore(t)
	a := mk(t, s)
	b := &Task{Objective: "after a", DependsOn: []string{a.ID}, Workspace: t.TempDir()}
	s.Create(b)
	if _, err := s.Claim(b.ID, Owner{ID: "x"}, DefaultLease); !errors.Is(err, ErrDeps) {
		t.Fatalf("claimed before its dependency: %v", err)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	run("config", "core.autocrlf", "false")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "init")
	return dir
}

func TestCheckpointRestoreAndDrift(t *testing.T) {
	repo := gitRepo(t)
	s, _ := newStore(t)
	tk := &Task{Objective: "edit a", Workspace: repo, Criteria: []Criterion{{ID: "c", Kind: CriterionFile, Path: "b.txt"}}, NextAction: "run the tests"}
	s.Create(tk)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("new file\n"), 0o644)
	cp, err := s.Checkpoint(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cp.Workspace.Git || cp.Workspace.Patch == "" || len(cp.Workspace.Untracked) != 1 || cp.Workspace.Untracked[0].Artifact == "" {
		t.Fatalf("capture: %+v", cp.Workspace)
	}
	if d := Compare(cp, repo); !d.Same {
		t.Fatalf("unchanged workspace drifted: %+v", d)
	}
	// The workspace moves on after the checkpoint.
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("three\n"), 0o644)
	d := Compare(cp, repo)
	if d.Same || !slices.Contains(d.Changed, "a.txt") {
		t.Fatalf("changed workspace not detected: %+v", d)
	}
	// Restore the checkpoint's exact state somewhere else.
	cp2, err := s.LoadCheckpoint(tk.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err := Restore(cp2, repo, dst, false); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(dst, "a.txt"))
	b, _ := os.ReadFile(filepath.Join(dst, "b.txt"))
	if string(a) != "two\n" || string(b) != "new file\n" {
		t.Fatalf("restored %q %q", a, b)
	}
	if d := Compare(cp2, dst); !d.Same {
		t.Fatalf("restored workspace differs: %+v", d)
	}
	h := cp2.Render(2000, &d)
	if !strings.Contains(h.Text, "edit a") || !strings.Contains(h.Text, "CHANGED") || !strings.Contains(h.Text, "run the tests") || h.Tokens == 0 {
		t.Fatalf("handoff: %s", h.Text)
	}
}

func TestRenderKeepsMandatoryOverBudget(t *testing.T) {
	cp := &Checkpoint{TaskID: "t", Objective: strings.Repeat("must keep ", 200), NextAction: "next", Completed: []string{"a", "b"}}
	h := cp.Render(10, nil)
	if !h.Overrun || !strings.Contains(h.Text, "must keep") || !strings.Contains(h.Text, "next") || h.Truncated != 2 {
		t.Fatalf("render: overrun=%v truncated=%d", h.Overrun, h.Truncated)
	}
}

func TestNonGitWorkspaceIsNotedNotRecoverable(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o644)
	w, err := Capture(dir, t.TempDir())
	if err != nil || w.Git || len(w.Notes) == 0 || w.Fingerprint == "" {
		t.Fatalf("%+v %v", w, err)
	}
}

func TestTornTailDoesNotSwallowNextEvent(t *testing.T) {
	s, _ := newStore(t)
	tk := mk(t, s)
	f, _ := os.OpenFile(filepath.Join(s.dir(tk.ID), eventsFile), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"rev":2,"task":{"st`)
	f.Close()
	if _, err := s.Move(tk.ID, Guard{}, Blocked, "x", ""); err != nil {
		t.Fatal(err)
	}
	// Snapshot lost: the log alone must hold the new event.
	os.Remove(filepath.Join(s.dir(tk.ID), taskFile))
	got, err := s.Get(tk.ID)
	if err != nil || got.State != Blocked {
		t.Fatalf("event after a torn tail lost: %+v %v", got, err)
	}
	if _, err := s.Events(tk.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("interior bad line not reported: %v", err)
	}
}

func TestUnresolvedOpCannotBeBypassed(t *testing.T) {
	s, c := newStore(t)
	tk := mk(t, s)
	a, _ := s.Claim(tk.ID, Owner{ID: "a"}, DefaultLease)
	s.BeginOp(tk.ID, a.Epoch, Op{Kind: "executor_run", Desc: "run"})
	c.add(2 * DefaultLease)
	s.Recover()
	if _, err := s.Move(tk.ID, Guard{}, Pending, "x", ""); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("blocked -> pending over an unknown op: %v", err)
	}
	// Releasing a run with an op in progress blocks too.
	s2, _ := newStore(t)
	tk2 := mk(t, s2)
	b, _ := s2.Claim(tk2.ID, Owner{ID: "b"}, DefaultLease)
	s2.BeginOp(tk2.ID, b.Epoch, Op{Kind: "executor_run", Desc: "run"})
	got, err := s2.Release(tk2.ID, b.Epoch, Paused, "")
	if err != nil || got.State != Blocked {
		t.Fatalf("release with op in flight: %v %v", got, err)
	}
}

func TestAttemptsAreBounded(t *testing.T) {
	s, _ := newStore(t)
	tk := &Task{Objective: "x", Workspace: t.TempDir(), MaxAttempts: 1}
	s.Create(tk)
	a, _ := s.Claim(tk.ID, Owner{ID: "a"}, DefaultLease)
	if _, err := s.StartAttempt(tk.ID, a.Epoch, "fake", 1); err != nil {
		t.Fatal(err)
	}
	s.Release(tk.ID, a.Epoch, Paused, "")
	b, _ := s.Claim(tk.ID, Owner{ID: "b"}, DefaultLease)
	if _, err := s.StartAttempt(tk.ID, b.Epoch, "fake", 1); !errors.Is(err, ErrAttempts) {
		t.Fatalf("second attempt over the limit: %v", err)
	}
}

func TestCriteriaHashBypassRefused(t *testing.T) {
	s, _ := newStore(t)
	tk := mk(t, s)
	_, err := s.Update(tk.ID, Guard{}, func(t *Task) (Event, error) {
		t.Criteria = nil
		t.CriteriaHash = HashCriteria(nil)
		return Event{Type: "criteria_amended"}, nil
	})
	if err == nil {
		t.Fatal("criteria and hash replaced together through a plain update")
	}
}

func TestCreateIsExclusive(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Create(&Task{ID: "same", Objective: "x"}) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d creators won", ok)
	}
}

func TestRestoreRefusesUnsafePaths(t *testing.T) {
	repo := gitRepo(t)
	head, _ := git(repo, "rev-parse", "HEAD")
	cp := &Checkpoint{Dir: t.TempDir(), Workspace: Workspace{Git: true, Head: strings.TrimSpace(string(head)),
		Untracked: []FileState{{Path: "../victim", Artifact: "untracked/../victim"}}}}
	if err := Restore(cp, repo, filepath.Join(t.TempDir(), "w"), true); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe path: %v", err)
	}
	cp.Workspace.Untracked = []FileState{{Path: "big.bin", Omitted: "larger than limit"}}
	if err := Restore(cp, repo, filepath.Join(t.TempDir(), "w"), false); err == nil {
		t.Fatal("partial restore succeeded silently")
	}
}

func TestReportedWithDeadOwnerIsRecovered(t *testing.T) {
	s, c := newStore(t)
	tk := mk(t, s)
	a, _ := s.Claim(tk.ID, Owner{ID: "a"}, DefaultLease)
	s.StartAttempt(tk.ID, a.Epoch, "x", 0)
	s.Move(tk.ID, Guard{Epoch: a.Epoch}, Reported, "a", "done")
	c.add(2 * DefaultLease)
	if ids, _ := s.Recover(); len(ids) != 1 {
		t.Fatal("reported task with a dead owner not recovered")
	}
	got, _ := s.Get(tk.ID)
	if got.State != Pending || got.Owner != nil {
		t.Fatalf("after recovery: %s", got.State)
	}
}
