// Command ultra-bench compares one coding task run three ways, with the
// task, model, starting revision, environment and permissions held equal:
//
//   - bare: the native agent CLI alone (claude -p, codex exec), told the task
//     and its acceptance checks;
//   - ultra: the same agent driven by PAI ULTRA, which builds the context,
//     records the run and checks acceptance itself;
//   - classic: PAI Classic, a session on a PAI server, when one is reachable
//     and a worker runs on this machine.
//
// Each run starts from a fresh copy of a fixed fixture repository at the same
// commit; success is the fixture's acceptance checks passing afterwards, run
// by this program, not anyone's claim. It also checks crash recovery without
// a model: a run dies mid-way, recovery must block on the uncertain
// operation, and the resumed task must finish without repeating the effect.
//
//	go run ./bench/ultra -modes bare-claude,ultra-claude -model haiku -n 1
//
// Writes bench/ultra/results.json and prints a table. Model runs cost tokens.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"pai/pkg/connector"
	"pai/pkg/harness"
	"pai/pkg/statefile"
	"pai/pkg/ultra/coord"
	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/task"
)

const objective = "The tests in package strutil fail for non-ASCII input. Fix Reverse so that `go test ./...` passes. Do not change the tests."

// checks are the fixture's acceptance criteria, as commands.
var checks = []string{"go test ./...", "git diff --exit-code -- strutil/strutil_test.go"}

var fixture = map[string]string{
	"go.mod":     "module fixture\n\ngo 1.22\n",
	".gitignore": ".gocache/\n",
	"strutil/strutil.go": `package strutil

// Reverse returns s reversed.
func Reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
`,
	"strutil/strutil_test.go": `package strutil

import "testing"

func TestReverse(t *testing.T) {
	for in, want := range map[string]string{"abc": "cba", "héllo": "olléh", "日本語": "語本日", "": ""} {
		if got := Reverse(in); got != want {
			t.Errorf("Reverse(%q) = %q, want %q", in, got, want)
		}
	}
}
`,
}

// Result is one run.
type Result struct {
	Mode          string   `json:"mode"`
	Run           int      `json:"run"`
	Model         string   `json:"model"`
	Success       bool     `json:"success"` // the acceptance checks pass afterwards
	Claimed       string   `json:"claimed,omitempty"`
	ElapsedS      float64  `json:"elapsed_s"`
	InputTokens   int      `json:"input_tokens"`
	OutputTokens  int      `json:"output_tokens"`
	CacheRead     int      `json:"cache_read_tokens"`
	TotalTokens   int      `json:"total_tokens"`
	UsageReported bool     `json:"usage_reported"`
	ContextEst    int      `json:"pai_context_tokens_est,omitempty"`
	Attempts      int      `json:"attempts,omitempty"`
	Background    int      `json:"background_model_calls"`
	FailedChecks  []string `json:"failed_checks,omitempty"`
	Error         string   `json:"error,omitempty"`
	Note          string   `json:"note,omitempty"`
}

// Report is the whole benchmark.
type Report struct {
	When        time.Time `json:"when"`
	Fixture     string    `json:"fixture"`
	Objective   string    `json:"objective"`
	Checks      []string  `json:"checks"`
	Permissions string    `json:"permissions"`
	Results     []Result  `json:"results"`
	Recovery    Recovery  `json:"recovery"`
	Unverified  []string  `json:"unverified,omitempty"`
}

// Recovery is the model-free crash scenario's outcome.
type Recovery struct {
	BlockedOnUnknown bool   `json:"blocked_on_unknown"`
	NoBlindReplay    bool   `json:"no_blind_replay"`
	ResumedAccepted  bool   `json:"resumed_accepted"`
	EffectsOnce      bool   `json:"effects_once"`
	Correct          bool   `json:"correct"`
	Error            string `json:"error,omitempty"`
}

func main() {
	modes := flag.String("modes", "bare-claude,ultra-claude", "comma list of bare-claude, ultra-claude, bare-codex, ultra-codex, classic")
	claudeModel := flag.String("claude-model", "haiku", "model for the claude modes, bare and ULTRA alike")
	codexModel := flag.String("codex-model", "", "model for the codex modes; empty is Codex's default")
	n := flag.Int("n", 1, "runs per mode")
	perm := flag.String("claude-permission-mode", "acceptEdits", "Claude Code permission mode, the same for bare and ULTRA")
	tools := flag.String("claude-allowed-tools", "Bash(go test:*) Bash(git diff:*)", "Claude Code tools allowed without asking, the same for bare and ULTRA")
	sandbox := flag.String("codex-sandbox", "workspace-write", "Codex sandbox, the same for bare and ULTRA")
	out := flag.String("out", "bench/ultra/results.json", "where to write results")
	flag.Parse()
	rep := Report{When: time.Now().UTC(), Fixture: "strutil Reverse, 4 cases (bench/ultra/main.go)", Objective: objective, Checks: checks,
		Permissions: fmt.Sprintf("claude --permission-mode %s --allowedTools %s; codex --sandbox %s", *perm, *tools, *sandbox)}
	rep.Recovery = recovery()
	for _, m := range strings.Split(*modes, ",") {
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		model := *claudeModel
		if strings.HasSuffix(m, "codex") {
			model = *codexModel
		}
		for i := 1; i <= *n; i++ {
			r := runMode(m, model, *perm, *tools, *sandbox)
			r.Run, r.Model = i, model
			r.TotalTokens = r.InputTokens + r.OutputTokens + r.CacheRead
			rep.Results = append(rep.Results, r)
			fmt.Printf("%-13s run %d: success=%v %.0fs tokens=%d (in %d, out %d, cache %d, reported %v) %s\n", r.Mode, i, r.Success, r.ElapsedS, r.TotalTokens, r.InputTokens, r.OutputTokens, r.CacheRead, r.UsageReported, r.Error)
		}
	}
	fmt.Printf("recovery: %+v\n", rep.Recovery)
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newFixture writes the fixture into a fresh git repository at one commit.
func newFixture() (string, error) {
	dir, err := os.MkdirTemp("", "ultra-bench-")
	if err != nil {
		return "", err
	}
	for p, s := range fixture {
		f := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(s), 0o644); err != nil {
			return "", err
		}
	}
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "bench@example.com"}, {"config", "user.name", "bench"},
		{"config", "core.autocrlf", "false"}, {"add", "."}, {"commit", "-qm", "fixture"}} {
		if out, err := run(dir, "git", a...); err != nil {
			return "", fmt.Errorf("git %v: %v %s", a, err, out)
		}
	}
	return dir, nil
}

func run(dir, name string, args ...string) (string, error) {
	cmd := osexec.Command(name, args...)
	cmd.Dir = dir
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

// accepted runs the checks the way the person would, and says which failed.
func accepted(dir string) (bool, []string) {
	var failed []string
	for _, c := range checks {
		f := strings.Fields(c)
		if out, err := run(dir, f[0], f[1:]...); err != nil {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			failed = append(failed, c+": "+strings.Join(lines[max(0, len(lines)-6):], " | "))
		}
	}
	return len(failed) == 0, failed
}

func bareText() string {
	return objective + "\nAcceptance checks: " + strings.Join(checks, "; ") + "."
}

func runMode(m, model, perm, tools, sandbox string) Result {
	r := Result{Mode: m}
	dir, err := newFixture()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer os.RemoveAll(dir)
	// Go's build cache lives in the workspace, so a sandbox that allows only
	// workspace writes can still run the tests. Every mode gets the same.
	os.Setenv("GOCACHE", filepath.Join(dir, ".gocache"))
	start := time.Now()
	ctx := context.Background()
	switch m {
	case "bare-claude", "bare-codex":
		var ad exec.Adapter = exec.Claude{}
		ps := exec.Permissions{ClaudePermissionMode: perm, AllowedTools: strings.Fields(tools)}
		if m == "bare-codex" {
			ad, ps = exec.Codex{}, exec.Permissions{CodexSandbox: sandbox}
		}
		if err := ad.Available(ctx); err != nil {
			r.Error = err.Error()
			return r
		}
		// The adapter only shells out to the CLI with the prompt: no PAI
		// context, no checks, no retries. The same process code as ULTRA's.
		runr, err := ad.Start(ctx, exec.Assignment{TaskID: "bare", Workdir: dir, Prompt: bareText(), Model: model, Permissions: ps, Timeout: 20 * time.Minute})
		if err != nil {
			r.Error = err.Error()
			return r
		}
		for range runr.Events() {
		}
		res := runr.Wait()
		r.Claimed, r.Error = res.Status, res.Err
		r.InputTokens, r.OutputTokens, r.CacheRead, r.UsageReported = res.Usage.Input, res.Usage.Output, res.Usage.CacheRead, res.Usage.Reported
		r.Note = "bare: the agent's own judgment of done; no PAI checks"
	case "ultra-claude", "ultra-codex":
		home, _ := os.MkdirTemp("", "ultra-bench-home-")
		defer os.RemoveAll(home)
		os.Setenv("PAI_ULTRA_HOME", home)
		executor := strings.TrimPrefix(m, "ultra-")
		c, err := coord.Open(dir, map[string]exec.Adapter{"claude": exec.Claude{}, "codex": exec.Codex{}})
		if err != nil {
			r.Error = err.Error()
			return r
		}
		defer c.Close()
		st := c.Project.Begin()
		st.SetExecutor(executor)
		st.Manifest.Executor.Model = model
		st.Manifest.Executor.Settings = map[string]string{"claude.permission_mode": perm, "claude.allowed_tools": tools, "codex.sandbox": sandbox}
		if _, err := st.Apply(); err != nil {
			r.Error = err.Error()
			return r
		}
		var crit []task.Criterion
		for i, cmd := range checks {
			crit = append(crit, task.Criterion{ID: fmt.Sprintf("check%d", i+1), Kind: task.CriterionCommand, Command: cmd})
		}
		// The manifest was applied under the program: reopen to read it.
		c.Project, _ = project.Open(dir)
		t, err := c.Create(coord.NewTask{Objective: objective, Criteria: crit, Executor: executor})
		if err != nil {
			r.Error = err.Error()
			return r
		}
		out, err := c.Run(ctx, t.ID, executor, nil)
		if err != nil {
			r.Error = err.Error()
		}
		ms, _ := c.Metrics.Read()
		for _, a := range ms {
			r.InputTokens += a.InputTokens
			r.OutputTokens += a.OutputTokens
			r.CacheRead += a.CacheReadTokens
			r.UsageReported = a.UsageReported
			r.Background += a.BackgroundCalls
			r.ContextEst += a.ContextTokensEst
		}
		if out.Task != nil {
			r.Attempts = out.Task.Attempts
		}
		r.Claimed = out.Result.Status
		r.Note = "ultra: accepted only when PAI's own checks pass; tokens summed over attempts"
	case "classic":
		r = classic(ctx, dir, model)
	default:
		r.Error = "unknown mode"
		return r
	}
	r.ElapsedS = time.Since(start).Seconds()
	r.Success, r.FailedChecks = accepted(dir)
	return r
}

// classic runs the task in a PAI Classic session on the server this
// machine is signed in to. The server's worker must reach dir.
func classic(ctx context.Context, dir, model string) Result {
	r := Result{Mode: "classic"}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, err := connector.Load(cctx)
	if err != nil {
		r.Error = "no PAI server: " + err.Error()
		return r
	}
	infos, err := c.Sessions(cctx, "", 20)
	if err != nil || len(infos) == 0 {
		r.Error = "no Classic session to copy workers from"
		return r
	}
	var base statefile.Conf
	for _, in := range infos {
		if in.Config.Mode == "" {
			base = in.Config
			break
		}
	}
	id, err := c.Create(cctx, harness.Create{Model: base.Model, Tools: base.Tools, Workers: base.Workers, ContextLimit: base.ContextLimit, Name: "ultra-bench classic"}, "bench")
	if err != nil {
		r.Error = err.Error()
		return r
	}
	before, _ := c.Info(cctx, id)
	if _, err := c.Post(cctx, id, "bench", "Working directory on this machine: "+dir+"\n\n"+bareText(), nil); err != nil {
		r.Error = err.Error()
		return r
	}
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		info, err := c.Info(ctx, id)
		if err != nil {
			continue
		}
		if info.Status == statefile.StatusClosed || info.Status == statefile.StatusFailed {
			u := info.Usage
			r.InputTokens = u.Input - before.Usage.Input + u.CacheWrite - before.Usage.CacheWrite
			r.OutputTokens = u.Output - before.Usage.Output
			r.CacheRead = u.CacheRead - before.Usage.CacheRead
			r.UsageReported = true
			r.Note = "classic: model " + info.Config.Model + " (the server session's; -model does not apply); Classic memory work may add background calls not counted here"
			return r
		}
	}
	r.Error = "timed out"
	return r
}

// recovery runs the model-free crash scenario.
func recovery() Recovery {
	var rc Recovery
	dir, err := newFixture()
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	defer os.RemoveAll(dir)
	home, _ := os.MkdirTemp("", "ultra-bench-home-")
	defer os.RemoveAll(home)
	os.Setenv("PAI_ULTRA_HOME", home)
	effects := filepath.Join(dir, "effects.log")
	fix := exec.Scripted{Name: "fake", Do: func(ctx context.Context, a exec.Assignment) (string, exec.Usage, error) {
		f, _ := os.OpenFile(effects, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString("effect\n")
		f.Close()
		os.WriteFile(filepath.Join(a.Workdir, "strutil", "strutil.go"), []byte("package strutil\n\nfunc Reverse(s string) string {\n\tr := []rune(s)\n\tfor i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {\n\t\tr[i], r[j] = r[j], r[i]\n\t}\n\treturn string(r)\n}\n"), 0o644)
		return "```pai-result\n{\"status\":\"done\"}\n```", exec.Usage{}, nil
	}}
	c, err := coord.Open(dir, map[string]exec.Adapter{"fake": fix})
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	defer c.Close()
	t, _ := c.Create(coord.NewTask{Objective: objective, Executor: "fake", Criteria: []task.Criterion{{ID: "test", Kind: task.CriterionCommand, Command: "go test ./..."}}})
	// A previous process claimed it, started the executor (whose effect
	// landed), and died before recording the outcome.
	// Its lease is long enough for these writes on a slow disk; the scenario
	// then waits it out, as a crashed owner's lease runs out.
	const deadLease = 2 * time.Second
	dead, err := c.Store.Claim(t.ID, task.Owner{ID: "dead"}, deadLease)
	if err != nil {
		rc.Error = "setup: " + err.Error()
		return rc
	}
	_, serr := c.Store.StartAttempt(t.ID, dead.Epoch, "fake", 0)
	op, berr := c.Store.BeginOp(t.ID, dead.Epoch, task.Op{Kind: "executor_run", Desc: "fake attempt 1"})
	if err := errors.Join(serr, berr, c.Store.SetOp(t.ID, dead.Epoch, op.ID, task.OpStarted, "native-x", "")); err != nil {
		rc.Error = "setup: " + err.Error()
		return rc
	}
	os.WriteFile(effects, []byte("effect\n"), 0o644)
	time.Sleep(deadLease + 200*time.Millisecond)
	c.Recover()
	_, err = c.Run(context.Background(), t.ID, "", nil)
	got, _ := c.Store.Get(t.ID)
	rc.BlockedOnUnknown = errors.Is(err, coord.ErrBlocked) && got.State == task.Blocked
	b, _ := os.ReadFile(effects)
	rc.NoBlindReplay = strings.Count(string(b), "effect") == 1
	// A person inspects and records that the run did not finish its work.
	c.Store.ResolveOp(t.ID, op.ID, task.OpFailed, "inspected: no fix in the tree")
	out, err := c.Run(context.Background(), t.ID, "", nil)
	rc.ResumedAccepted = err == nil && out.Task != nil && out.Task.State == task.Accepted
	b, _ = os.ReadFile(effects)
	rc.EffectsOnce = strings.Count(string(b), "effect") == 2 // the dead run's, then exactly one resume
	rc.Correct = rc.BlockedOnUnknown && rc.NoBlindReplay && rc.ResumedAccepted && rc.EffectsOnce
	if err != nil {
		rc.Error = err.Error()
	}
	return rc
}
