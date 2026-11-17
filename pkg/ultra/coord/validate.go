package coord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pai/pkg/ultra/internal/proc"
	"pai/pkg/ultra/metrics"
	"pai/pkg/ultra/task"
)

// outputKeep is how much of a check's output a result keeps.
const outputKeep = 2000

// shell runs a criterion's command the way the platform's shell would.
func shell(ctx context.Context, dir, command string) *osexec.Cmd {
	// A timeout or a lost lease stops the command and everything it started.
	var cmd *osexec.Cmd
	if runtime.GOOS == "windows" {
		cmd = proc.Command(ctx, "cmd", "/C", command)
	} else {
		cmd = proc.Command(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	return cmd
}

// Validate checks every criterion of t in its workspace. Commands and files
// are checked here, deterministically; a review criterion passes only with a
// person's approval for the current attempt.
func (c *Coordinator) Validate(ctx context.Context, t *task.Task) task.Validation {
	v := task.Validation{Passed: true, CriteriaHash: t.CriteriaHash, Time: time.Now().UTC()}
	dir := workdir(t)
	for _, cr := range t.Criteria {
		r := task.CheckResult{Criterion: cr.ID, Time: time.Now().UTC()}
		switch cr.Kind {
		case task.CriterionCommand:
			r = c.runCheck(ctx, dir, cr)
		case task.CriterionFile:
			r = checkFile(dir, cr)
		case task.CriterionReview:
			if approved(t, cr.ID) {
				r.Status, r.Detail = "pass", "approved by a person for attempt "+fmt.Sprint(t.Attempts)
			} else {
				r.Status, r.Detail = "needs_review", cr.Text
				v.NeedsReview = true
			}
		default:
			r.Status, r.Detail = "error", "unknown criterion kind "+cr.Kind
		}
		if r.Status != "pass" && r.Status != "needs_review" {
			v.Passed = false
		}
		v.Results = append(v.Results, r)
	}
	return v
}

func (c *Coordinator) runCheck(ctx context.Context, dir string, cr task.Criterion) task.CheckResult {
	r := task.CheckResult{Criterion: cr.ID, Time: time.Now().UTC(), Env: task.EnvFingerprint(cr.Command)}
	timeout := c.CheckTimeout
	if cr.Timeout > 0 {
		timeout = time.Duration(cr.Timeout) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out bytes.Buffer
	cmd := shell(cctx, dir, cr.Command)
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	err := cmd.Run()
	r.Duration = time.Since(start)
	r.Output = tail(out.String(), outputKeep)
	var exit *osexec.ExitError
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		r.Status, r.Detail, r.Exit = "fail", fmt.Sprintf("`%s` timed out after %s", cr.Command, timeout), -1
	case errors.As(err, &exit):
		r.Status, r.Exit = "fail", exit.ExitCode()
		r.Detail = fmt.Sprintf("`%s` exited %d: %s", cr.Command, r.Exit, lastLine(out.String()))
	case err != nil:
		r.Status, r.Detail, r.Exit = "error", err.Error(), -1
	default:
		r.Status, r.Detail = "pass", fmt.Sprintf("`%s` exited 0", cr.Command)
	}
	return r
}

func checkFile(dir string, cr task.Criterion) task.CheckResult {
	r := task.CheckResult{Criterion: cr.ID, Time: time.Now().UTC()}
	if !filepath.IsLocal(filepath.FromSlash(cr.Path)) {
		r.Status, r.Detail = "error", "path is not inside the workspace: "+cr.Path
		return r
	}
	full := filepath.Join(dir, filepath.FromSlash(cr.Path))
	if !task.Inside(dir, full) {
		r.Status, r.Detail = "error", cr.Path+" leads outside the workspace"
		return r
	}
	b, err := os.ReadFile(full)
	switch {
	case err != nil:
		r.Status, r.Detail = "fail", cr.Path+" is missing"
	case cr.Contains != "" && !strings.Contains(string(b), cr.Contains):
		r.Status, r.Detail = "fail", fmt.Sprintf("%s does not contain %q", cr.Path, cr.Contains)
	default:
		r.Status, r.Detail = "pass", cr.Path+" ok"
	}
	return r
}

// approvalPrefix marks a person's approval of a review criterion: for one
// attempt and one version of the criteria, so an amendment (even one that
// keeps the criterion's id) needs a new approval.
func approvalPrefix(id string, attempt int, criteriaHash string) string {
	h := criteriaHash
	if len(h) > 12 {
		h = h[:12]
	}
	return fmt.Sprintf("review %s approved for attempt %d criteria %s", id, attempt, h)
}

func approved(t *task.Task, id string) bool {
	p := approvalPrefix(id, t.Attempts, t.CriteriaHash)
	for _, d := range t.Decisions {
		if strings.HasPrefix(d, p) {
			return true
		}
	}
	return false
}

// Approve records a person's approval of review criterion id for the task's
// current attempt and criteria, then re-checks. When every criterion now
// passes, the workspace is checkpointed and the task accepted on that
// checkpoint, in a write that refuses if the criteria changed meanwhile.
// Only a person approves; the executor has no path here.
func (c *Coordinator) Approve(ctx context.Context, id, criterion, actor, note string) (*task.Task, error) {
	if actor == "" || strings.HasPrefix(strings.ToLower(actor), "model") || strings.HasPrefix(strings.ToLower(actor), "executor") {
		return nil, errors.New("only a person approves a review")
	}
	t, err := c.Store.Update(id, task.Guard{}, func(t *task.Task) (task.Event, error) {
		found := false
		for _, cr := range t.Criteria {
			found = found || (cr.ID == criterion && cr.Kind == task.CriterionReview)
		}
		if !found {
			return task.Event{}, fmt.Errorf("no review criterion %q", criterion)
		}
		if t.State != task.Blocked {
			return task.Event{}, fmt.Errorf("the task is %s; reviews are approved while it waits", t.State)
		}
		t.Decisions = append(t.Decisions, fmt.Sprintf("%s by %s: %s", approvalPrefix(criterion, t.Attempts, t.CriteriaHash), actor, note))
		return task.Event{Type: "review_approved", Actor: actor, Note: criterion}, nil
	})
	if err != nil {
		return nil, err
	}
	v := c.Validate(ctx, t)
	got, err := c.Store.Update(id, task.Guard{Rev: t.Rev}, func(t *task.Task) (task.Event, error) {
		if t.CriteriaHash != v.CriteriaHash {
			return task.Event{}, errCriteriaChanged
		}
		t.Validation = &v
		return task.Event{Type: "validated", Actor: actor, Note: fmt.Sprintf("passed=%v review=%v", v.Passed, v.NeedsReview)}, nil
	})
	if err != nil || !v.Passed || v.NeedsReview || got.State != task.Blocked || len(got.Unknown()) > 0 {
		c.recordReview(got, actor, v, "blocked")
		return got, err
	}
	// Checkpoint exactly what was judged, then accept on it.
	cp, err := c.Store.Checkpoint(id)
	if err != nil {
		return got, fmt.Errorf("checkpoint: %w", err)
	}
	got, err = c.Store.Update(id, task.Guard{}, func(t *task.Task) (task.Event, error) {
		if t.CriteriaHash != v.CriteriaHash || t.State != task.Blocked {
			return task.Event{}, errCriteriaChanged
		}
		t.Blockers = nil
		t.State = task.Accepted
		t.AcceptedCheckpoint = cp.ID
		return task.Event{Type: "decided", Actor: actor, Note: "accepted after review, on " + cp.ID}, nil
	})
	if err == nil {
		c.recordReview(got, actor, v, "accepted")
	}
	return got, err
}

func (c *Coordinator) recordReview(t *task.Task, actor string, v task.Validation, outcome string) {
	if t == nil {
		return
	}
	now := time.Now().UTC()
	_ = c.Metrics.Append(metrics.Attempt{Task: t.ID, Attempt: t.Attempts, Mode: "ultra", Executor: "review:" + actor, ConfigRev: t.ConfigRev,
		Started: now, Ended: now, ValidationPassed: v.Passed, Outcome: outcome})
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + strings.ToValidUTF8(s[len(s)-n:], "")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
