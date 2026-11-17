// Package coord is PAI ULTRA's task coordinator: mostly deterministic, it
// owns the task contract and workflow state, and leaves reasoning and tool
// use to the executor.
//
// One capable executor runs a task by default. The coordinator claims the
// task with a lease, builds the context by rule, starts the executor through
// its adapter, records the operation, then decides acceptance itself by the
// task's criteria: an executor saying "done" is a claim. A failed check is
// retried up to the task's attempts, with the failure in the next context;
// a review criterion waits for a person. Every attempt ends at a checkpoint.
package coord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pai/pkg/ultra/ctxsel"
	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/knowledge"
	"pai/pkg/ultra/metrics"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/task"
)

const (
	// knowledgeHits bounds how many knowledge items a context considers.
	knowledgeHits = 8
	// DefaultCheckTimeout bounds one command criterion.
	DefaultCheckTimeout = 10 * time.Minute
)

// ErrBlocked is returned when a task waits for a person.
var ErrBlocked = errors.New("the task waits for a person")

// Coordinator runs one project's tasks.
type Coordinator struct {
	Project   *project.Project
	Store     *task.Store
	Knowledge *knowledge.Store // nil disables retrieval
	Adapters  map[string]exec.Adapter
	Metrics   *metrics.Log
	Owner     task.Owner
	Lease     time.Duration
	// CheckTimeout bounds a command criterion without its own timeout.
	CheckTimeout time.Duration
}

// Open wires a coordinator for the project of dir. Its state lives under the
// project's machine-local directory, apart from Classic's.
func Open(dir string, adapters map[string]exec.Adapter) (*Coordinator, error) {
	p, err := project.Open(dir)
	if err != nil {
		return nil, err
	}
	k, err := knowledge.Open(p.State, p.Root)
	if err != nil {
		return nil, fmt.Errorf("knowledge: %w", err)
	}
	host, _ := os.Hostname()
	return &Coordinator{
		Project: p, Store: task.NewStore(filepath.Join(p.State, "tasks")), Knowledge: k, Adapters: adapters,
		Metrics: &metrics.Log{Path: filepath.Join(p.State, "runs.jsonl")},
		Owner:   task.Owner{ID: fmt.Sprintf("%s-%d-%s", host, os.Getpid(), task.NewID("o")[2:]), Host: host, PID: os.Getpid()},
		Lease:   task.DefaultLease, CheckTimeout: DefaultCheckTimeout,
	}, nil
}

// Close releases the knowledge store.
func (c *Coordinator) Close() error {
	if c.Knowledge != nil {
		return c.Knowledge.Close()
	}
	return nil
}

// NewTask describes a task to create.
type NewTask struct {
	ID          string // "" makes one
	Objective   string
	Criteria    []task.Criterion
	Constraints []string
	Paths       []string
	Skills      []string
	Executor    string
	Parent      string
	DependsOn   []string
	Worktree    string
	Links       []string
	NoDefaults  bool // leave out the manifest's default criteria
}

// Create stores a task with the project's default criteria and attempts.
func (c *Coordinator) Create(nt NewTask) (*task.Task, error) {
	if strings.TrimSpace(nt.Objective) == "" {
		return nil, errors.New("a task needs an objective")
	}
	p := c.current()
	m := p.Manifest
	crit := slices.Clone(nt.Criteria)
	if !nt.NoDefaults {
		for _, d := range m.Criteria {
			cr := task.Criterion{ID: d.ID, Text: d.Text, Command: d.Command, Kind: task.CriterionReview}
			if d.Command != "" {
				cr.Kind = task.CriterionCommand
			}
			if !slices.ContainsFunc(crit, func(x task.Criterion) bool { return x.ID == cr.ID }) {
				crit = append(crit, cr)
			}
		}
	}
	for i := range crit {
		if crit[i].ID == "" {
			crit[i].ID = fmt.Sprintf("c%d", i+1)
		}
		if crit[i].Kind == "" {
			crit[i].Kind = task.CriterionReview
			if crit[i].Command != "" {
				crit[i].Kind = task.CriterionCommand
			}
		}
	}
	executor := nt.Executor
	if executor == "" {
		executor = c.DefaultExecutor()
	}
	t := &task.Task{
		ID: nt.ID, Objective: nt.Objective, Criteria: crit, Constraints: nt.Constraints, Paths: nt.Paths, Skills: nt.Skills,
		Parent: nt.Parent, DependsOn: nt.DependsOn, Workspace: p.Root, Worktree: nt.Worktree, Links: nt.Links,
		Executor: executor, MaxAttempts: m.Attempts(), ConfigRev: p.Revision(), Mode: "ultra",
	}
	return t, c.Store.Create(t)
}

// DefaultExecutor is the manifest's choice, else the first available of
// claude, codex, pai.
func (c *Coordinator) DefaultExecutor() string {
	if d := c.current().Manifest.Executor.Default; d != "" {
		return d
	}
	for _, n := range []string{"claude", "codex", "pai"} {
		if a, ok := c.Adapters[n]; ok && a.Available(context.Background()) == nil {
			return n
		}
	}
	return "claude"
}

// workdir is where a task's work happens.
func workdir(t *task.Task) string {
	if t.Worktree != "" {
		return t.Worktree
	}
	return t.Workspace
}

// Context selects the context for t on executor. Resume adds the newest
// checkpoint and the workspace drift since it.
func (c *Coordinator) Context(t *task.Task, executor string) (ctxsel.Context, *task.Drift, error) {
	p, err := project.Open(c.Project.Root)
	if err != nil {
		return ctxsel.Context{}, nil, err
	}
	ad, ok := c.Adapters[executor]
	if !ok {
		return ctxsel.Context{}, nil, fmt.Errorf("unknown executor %q", executor)
	}
	r := p.Resolve()
	var paths []string
	if len(t.Paths) > 0 {
		paths = t.Paths
	}
	sel := project.Apply(p.Inventory(r), executor, paths, t.Skills)
	boot, resumeBudget := p.Manifest.Budgets()
	in := ctxsel.Inputs{Task: t, Resolved: r, Selection: sel, Caps: ad.Capabilities(), Budget: boot, ConfigRev: p.Revision()}
	var drift *task.Drift
	if len(t.Checkpoints) > 0 {
		cp, err := c.Store.LoadCheckpoint(t.ID, "")
		if err == nil {
			d := task.Compare(cp, workdir(t))
			in.Checkpoint, in.Drift, in.Resume, in.Budget = cp, &d, true, resumeBudget
			drift = &d
		} else {
			// No readable checkpoint: nothing proves the workspace unchanged.
			drift = &task.Drift{Notes: []string{"newest checkpoint unreadable: " + err.Error()}}
		}
	}
	if c.Knowledge != nil {
		hits, err := c.Knowledge.Search(context.Background(), t.Objective, knowledge.Filter{Paths: paths, Scope: t.ID, Limit: knowledgeHits})
		if err == nil {
			in.Knowledge = hits
		}
	}
	return ctxsel.Build(in), drift, nil
}

// Outcome is what a Run ended with.
type Outcome struct {
	Task       *task.Task
	Result     exec.Result
	Validation *task.Validation
	Context    ctxsel.Context
	Warnings   []string
}

// permissions are the person's executor settings from the manifest. Skills,
// rules, memory and handoffs cannot set them.
func permissions(p *project.Project) exec.Permissions {
	s := p.Manifest.Executor.Settings
	return exec.Permissions{ClaudePermissionMode: s["claude.permission_mode"], CodexSandbox: s["codex.sandbox"],
		AllowedTools: strings.Fields(s["claude.allowed_tools"])}
}

// current is the project as it is on disk now. Each use opens it afresh, so
// an apply elsewhere (the TUI, the CLI) is seen, and no two goroutines share
// one Project. c.Project is only the project's identity and paths.
func (c *Coordinator) current() *project.Project {
	if p, err := project.Open(c.Project.Root); err == nil {
		return p
	}
	return c.Project
}

// Run works on task id with executor until it is accepted, blocked, failed
// or out of attempts. on, when set, sees the executor's progress.
func (c *Coordinator) Run(ctx context.Context, id, executor string, on func(exec.Event)) (Outcome, error) {
	var out Outcome
	if executor == "" {
		t, err := c.Store.Get(id)
		if err != nil {
			return out, err
		}
		executor = t.Executor
	}
	ad, ok := c.Adapters[executor]
	if !ok {
		return out, fmt.Errorf("unknown executor %q", executor)
	}
	if err := ad.Available(ctx); err != nil {
		return out, fmt.Errorf("%s unavailable: %w", executor, err)
	}
	for {
		t, err := c.Store.Get(id)
		if err != nil {
			return out, err
		}
		out.Task = t
		switch {
		case t.State == task.Accepted:
			return out, nil
		case t.State.Terminal() || t.State == task.Failed:
			return out, fmt.Errorf("task is %s", t.State)
		case t.State == task.Blocked:
			return out, fmt.Errorf("%w: %s", ErrBlocked, strings.Join(t.Blockers, "; "))
		}
		again, err := c.attempt(ctx, id, executor, ad, on, &out)
		if err != nil || !again {
			return out, err
		}
	}
}

// attempt is one claim, execution, validation and decision. It reports
// whether to try again.
func (c *Coordinator) attempt(ctx context.Context, id, executor string, ad exec.Adapter, on func(exec.Event), out *Outcome) (bool, error) {
	began := time.Now()
	rec := metrics.Attempt{Task: id, Mode: "ultra", Executor: executor, Started: began.UTC()}
	t, err := c.Store.Claim(id, c.Owner, c.Lease)
	if err != nil {
		return false, err
	}
	epoch := t.Epoch
	// runCtx ends when the attempt ends or the lease is lost: everything the
	// attempt starts (the executor, the checks) runs under it.
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	lost := make(chan struct{})
	stopBeat := c.heartbeat(runCtx, id, epoch, func() { close(lost); stopRun() })
	defer stopBeat()
	isLost := func() bool {
		select {
		case <-lost:
			return true
		default:
			return false
		}
	}
	release := func(to task.State, note string) error {
		got, err := c.Store.Release(id, epoch, to, note)
		if err == nil {
			out.Task = got
		}
		return err
	}

	ctxStart := time.Now()
	cx, drift, err := c.Context(t, executor)
	rec.ContextMS = time.Since(ctxStart).Milliseconds()
	if err != nil {
		return false, errors.Join(err, release(task.Paused, "context: "+err.Error()))
	}
	out.Context = cx
	rec.ContextTokensEst, rec.ContextOverrun, rec.ConfigRev = cx.Tokens, cx.Overrun, cx.ConfigRev
	native := ""
	// A native session is reused only when the newest checkpoint loaded and
	// the workspace is exactly as it left it.
	if ad.Capabilities().Resume && t.Native[executor] != "" && drift != nil && drift.Same {
		native = t.Native[executor]
	}
	rec.Resumed, rec.NativeResume = len(t.Checkpoints) > 0, native != ""
	t, err = c.Store.StartAttempt(id, epoch, executor, cx.ConfigRev)
	if err != nil {
		return false, errors.Join(err, release(task.Paused, err.Error()))
	}
	rec.Attempt = t.Attempts
	op, err := c.Store.BeginOp(id, epoch, task.Op{Kind: "executor_run", Desc: fmt.Sprintf("%s attempt %d", executor, t.Attempts)})
	if err != nil {
		return false, err
	}
	p := c.current()
	model := p.Manifest.Executor.Model
	rec.Model = model
	exStart := time.Now()
	run, err := ad.Start(runCtx, exec.Assignment{TaskID: id, Workdir: workdir(t), Prompt: cx.Prompt, Model: model, Resume: native, Permissions: permissions(p)})
	if err != nil {
		var unc *exec.UncertainError
		if errors.As(err, &unc) {
			// It may have started: record it as unknown, which blocks the
			// task until a person checks, instead of letting a retry repeat it.
			serr := c.Store.SetOp(id, epoch, op.ID, task.OpUnknown, unc.Ref, err.Error())
			rerr := release(task.Paused, "uncertain start")
			rec.Outcome, rec.Error = "error", err.Error()
			c.finish(&rec, out, began, exStart, time.Now(), 0)
			return false, errors.Join(err, serr, rerr)
		}
		serr := c.Store.SetOp(id, epoch, op.ID, task.OpFailed, "", "did not start: "+err.Error())
		rerr := release(task.Paused, "executor did not start")
		rec.Outcome, rec.Error = "error", err.Error()
		c.finish(&rec, out, began, exStart, time.Now(), 0)
		return false, errors.Join(err, serr, rerr)
	}
	if err := c.Store.SetOp(id, epoch, op.ID, task.OpStarted, run.ID(), ""); err != nil {
		run.Cancel()
		run.Wait()
		return false, err
	}
	for e := range run.Events() {
		if on != nil {
			on(e)
		}
	}
	res := run.Wait()
	exEnd := time.Now()
	out.Result = res
	rec.ExecutorClaim = res.Status
	rec.InputTokens, rec.OutputTokens, rec.CacheReadTokens, rec.UsageReported = res.Usage.Input, res.Usage.Output, res.Usage.CacheRead, res.Usage.Reported
	if isLost() {
		// Another owner took over: this attempt writes nothing more. Its op
		// was marked unknown by the takeover.
		rec.Outcome, rec.Error = "error", "lease lost"
		c.finish(&rec, out, began, exStart, exEnd, 0)
		return false, task.ErrStaleOwner
	}
	opState, note := task.OpDone, ""
	if res.Status == "cancelled" || res.Status == "failed" && res.Err != "" {
		opState, note = task.OpFailed, res.Err+"; partial changes may remain in the workspace"
	}
	if err := c.Store.SetOp(id, epoch, op.ID, opState, res.NativeSession, note); err != nil {
		return false, err
	}
	report := &task.Report{Status: res.Status, Summary: res.Summary, Changes: res.Changes, Evidence: res.Evidence, Open: res.Open, Time: time.Now().UTC(), Native: res.NativeSession}
	if res.Status == "cancelled" {
		_, uerr := c.Store.Update(id, task.Guard{Epoch: epoch}, func(t *task.Task) (task.Event, error) {
			t.Report = report
			t.FailedAttempts = append(t.FailedAttempts, fmt.Sprintf("attempt %d with %s was cancelled; changes it made may remain", t.Attempts, executor))
			return task.Event{Type: "report", Actor: executor, Note: "cancelled"}, nil
		})
		rerr := release(task.Paused, "cancelled")
		_, cerr := c.Store.Checkpoint(id)
		rec.Outcome = "cancelled"
		c.finish(&rec, out, began, exStart, exEnd, 0)
		return false, errors.Join(ctx.Err(), uerr, rerr, cerr)
	}
	// Every claim, "blocked" included, is checked: an executor that could not
	// verify its own work may still have done it.
	nextState := task.Reported
	t, err = c.Store.Update(id, task.Guard{Epoch: epoch}, func(t *task.Task) (task.Event, error) {
		t.Report = report
		if t.Native == nil {
			t.Native = map[string]string{}
		}
		if res.NativeSession != "" {
			t.Native[executor] = res.NativeSession
		}
		if res.Summary != "" {
			t.Completed = append(t.Completed, fmt.Sprintf("attempt %d (%s, claimed %s): %s", t.Attempts, executor, res.Status, res.Summary))
		}
		t.State = nextState
		return task.Event{Type: "report", Actor: executor, Note: "claimed " + res.Status}, nil
	})
	if err != nil {
		return false, err
	}
	// The coordinator, not the executor, decides. Checks run under the
	// lease; if the criteria change while they run, they run again.
	vStart := time.Now()
	vop, err := c.Store.BeginOp(id, epoch, task.Op{Kind: "validate", Desc: "check acceptance criteria", Idempotent: true})
	if err != nil {
		return false, err
	}
	var val task.Validation
	for range validateTries {
		val = c.Validate(runCtx, t)
		if isLost() {
			rec.Outcome, rec.Error = "error", "lease lost during validation"
			c.finish(&rec, out, began, exStart, exEnd, time.Since(vStart))
			return false, task.ErrStaleOwner
		}
		cur, err := c.Store.Get(id)
		if err != nil {
			return false, err
		}
		if cur.CriteriaHash == val.CriteriaHash {
			break
		}
		t = cur
	}
	if err := c.Store.SetOp(id, epoch, vop.ID, task.OpDone, "", ""); err != nil {
		return false, err
	}
	vDur := time.Since(vStart)
	out.Validation = &val
	rec.ValidationPassed = val.Passed
	failing := failures(val)
	rec.ValidationFailures = len(failing)
	// Record the result and checkpoint the exact state that was judged,
	// before deciding, so the decision always has its evidence on disk.
	t, err = c.Store.Update(id, task.Guard{Epoch: epoch}, func(t *task.Task) (task.Event, error) {
		t.Validation = &val
		if len(failing) > 0 {
			t.FailedAttempts = append(t.FailedAttempts, fmt.Sprintf("attempt %d (%s claimed %s) failed checks: %s", t.Attempts, executor, res.Status, strings.Join(failing, "; ")))
			t.NextAction = "make these checks pass: " + strings.Join(failing, "; ")
		}
		return task.Event{Type: "validated", Note: fmt.Sprintf("passed=%v review=%v", val.Passed, val.NeedsReview)}, nil
	})
	if err != nil {
		return false, err
	}
	judged, err := c.Store.Checkpoint(id)
	if err != nil {
		return false, errors.Join(fmt.Errorf("checkpoint: %w", err), release(task.Paused, "checkpoint failed"))
	}
	again := false
	var decision error
	// With no criteria the checks pass vacuously: the executor's claim is all
	// there is, so only "done" is accepted, and it stays unverified.
	vacuous := len(t.Criteria) == 0 && res.Status != "done"
	switch {
	case val.Passed && !val.NeedsReview && !vacuous:
		decision = c.decide(id, epoch, val, task.Accepted, "all criteria passed", out, judged.ID)
		rec.Outcome = "accepted"
	case len(failing) == 0 && val.NeedsReview:
		decision = c.decide(id, epoch, val, task.Blocked, "review needed: "+strings.Join(reviewIDs(val), ", "), out, "")
		rec.Outcome = "blocked"
		if decision == nil {
			decision = fmt.Errorf("%w: review needed", ErrBlocked)
		}
	case res.Status == "blocked":
		// The executor needs a person and the checks do not pass: no retry.
		decision = c.decide(id, epoch, val, task.Blocked, "executor reported blocked: "+strings.Join(append([]string{res.Summary}, res.Open...), "; "), out, "")
		rec.Outcome = "blocked"
		if decision == nil {
			decision = fmt.Errorf("%w: the executor reported it is blocked", ErrBlocked)
		}
	case t.Attempts < t.MaxAttempts:
		decision = c.decide(id, epoch, val, task.Pending, "retry after failed checks", out, "")
		rec.Outcome, again = "retry", decision == nil
	default:
		decision = c.decide(id, epoch, val, task.Failed, "attempts exhausted", out, "")
		rec.Outcome = "failed"
		if decision == nil {
			decision = fmt.Errorf("task failed after %d attempts: %s", t.Attempts, strings.Join(failing, "; "))
		}
	}
	c.finish(&rec, out, began, exStart, exEnd, vDur)
	return again, decision
}

// validateTries bounds re-validation when criteria change during checks.
const validateTries = 3

// errCriteriaChanged stops a decision made on criteria that are no longer the task's.
var errCriteriaChanged = errors.New("the criteria changed after they were checked; run again to check the new ones")

// decide releases the task to its decided state in one guarded write: only
// if the validation was of the task's current criteria.
func (c *Coordinator) decide(id string, epoch int, val task.Validation, to task.State, note string, out *Outcome, judged string) error {
	got, err := c.Store.Update(id, task.Guard{Epoch: epoch}, func(t *task.Task) (task.Event, error) {
		if t.CriteriaHash != val.CriteriaHash {
			return task.Event{}, errCriteriaChanged
		}
		if to == task.Blocked {
			t.Blockers = append(t.Blockers, note)
		}
		if to == task.Accepted {
			t.AcceptedCheckpoint = judged
		}
		actor := t.Owner.ID
		t.Owner = nil
		t.State = to
		return task.Event{Type: "decided", Actor: actor, Note: note}, nil
	})
	if errors.Is(err, errCriteriaChanged) {
		rel, rerr := c.Store.Release(id, epoch, task.Pending, "criteria changed during validation")
		if rerr == nil {
			out.Task = rel
		}
		return errors.Join(err, rerr)
	}
	if err == nil {
		out.Task = got
	}
	return err
}

func reviewIDs(v task.Validation) []string {
	var out []string
	for _, r := range v.Results {
		if r.Status == "needs_review" {
			out = append(out, r.Criterion)
		}
	}
	return out
}

func (c *Coordinator) finish(rec *metrics.Attempt, out *Outcome, began, exStart, exEnd time.Time, validation time.Duration) {
	end := time.Now()
	rec.Ended = end.UTC()
	rec.TotalMS = end.Sub(began).Milliseconds()
	rec.ExecutorMS = exEnd.Sub(exStart).Milliseconds()
	rec.ValidationMS = validation.Milliseconds()
	rec.CoordinationMS = max(0, rec.TotalMS-rec.ExecutorMS-rec.ValidationMS)
	if err := c.Metrics.Append(*rec); err != nil {
		out.Warnings = append(out.Warnings, "metrics not recorded: "+err.Error())
	}
}

// heartbeat renews the lease at a third of its length. When renewal says the
// lease is gone, lost runs once: the attempt must stop writing.
func (c *Coordinator) heartbeat(ctx context.Context, id string, epoch int, lost func()) func() {
	every := c.Lease / 3
	if every <= 0 {
		every = time.Second
	}
	done := make(chan struct{})
	go func() {
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tk.C:
				if err := c.Store.Renew(id, epoch, c.Lease); errors.Is(err, task.ErrStaleOwner) {
					lost()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

func failures(v task.Validation) []string {
	var out []string
	for _, r := range v.Results {
		if r.Status == "fail" || r.Status == "error" {
			out = append(out, fmt.Sprintf("%s: %s", r.Criterion, firstLine(r.Detail)))
		}
	}
	return out
}

func firstLine(s string) string { return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0]) }

// Recover pauses or blocks tasks whose owners died.
func (c *Coordinator) Recover() ([]string, error) { return c.Store.Recover() }
