package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"pai/pkg/connector"
	"pai/pkg/connector/tui"
	"pai/pkg/ultra/coord"
	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/handoff"
	"pai/pkg/ultra/knowledge"
	"pai/pkg/ultra/metrics"
	"pai/pkg/ultra/mode"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/runtime"
	"pai/pkg/ultra/task"
	"pai/pkg/version"
)

const ultraUsage = `pai mode [classic|ultra]           show or set this project's default mode for new sessions
pai ultra <command>                 PAI ULTRA (see docs/ultra.md)

  status                            project, configuration revision, executors, tasks
  init [--profile P] [--executor E] [--check CMD]... [--dry-run]
  skills [list]                     installed skills and their state for this project
  skills show NAME
  skills enable|disable|update NAME [--dry-run]
  skills pin NAME VERSION | skills unpin NAME
  skills activation NAME auto|always|manual
  skills paths NAME [GLOB...]
  rules [list] | rules show NAME | rules enable|disable NAME
  rules new NAME                    create .pai/rules/NAME.md (opens $EDITOR when set)
  rules override NAME               copy a package rule into a project override
  profile apply|remove NAME
  instructions [--path P]... [--executor E]
  context TASK | context --objective TEXT [--path P]... [--executor E]
  task new OBJECTIVE [--check CMD]... [--file PATH[:TEXT]]... [--review TEXT]... [--path P]... [--skill S]... [--executor E] [--no-defaults]
  run TASK|OBJECTIVE [--executor E] [same options as task new]
  resume TASK [--executor E]
  tasks                             all tasks
  show TASK                         contract, state, checkpoints, events
  checkpoint TASK
  restore TASK DIR [--partial]      rebuild the newest checkpoint's workspace in a new worktree
  approve TASK CRITERION [NOTE]     a person approves a review criterion
  reconcile TASK OP done|failed NOTE
  reopen TASK ATTEMPTS REASON
  amend TASK REASON --check CMD...  replace the criteria explicitly
  recover                           pause or block tasks whose owner died
  delegate PARENT OBJECTIVE [task options]
  integrate CHILD
  handoff TASK --to classic [--apply]   preview, then continue a task in a new Classic session (no model call)
  handoff --from-classic SESSION [--apply]   preview, then create an ULTRA task from a Classic session
  memory [list] [--all] | memory search QUERY
  memory add preference|decision|fact|hypothesis TEXT [--dep PATH]...
  memory verify ID [--evidence TEXT] | memory invalidate ID REASON
  memory import [--apply] [--limit N]   preview (default) or import Classic memory as candidates
  metrics                           attempt totals: tokens, time, retries, outcomes
`

// opts is the parsed flags of a ULTRA command: repeated flags collect.
type opts struct {
	pos   []string
	multi map[string][]string
	bools map[string]bool
}

var (
	boolFlags  = []string{"--dry-run", "--no-defaults", "--apply", "--all", "--partial", "--json"}
	valueFlags = []string{"--profile", "--executor", "--check", "--source", "--path", "--objective", "--file", "--review", "--skill",
		"--constraint", "--dep", "--evidence", "--limit", "--to", "--from-classic"}
)

// parseOpts reads known flags anywhere; "--" ends flags, so a positional
// may start with "--"; an unknown flag is an error, never a positional.
func parseOpts(args []string) opts {
	o := opts{multi: map[string][]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--":
			o.pos = append(o.pos, args[i+1:]...)
			return o
		case slices.Contains(boolFlags, a):
			o.bools[a] = true
		case slices.Contains(valueFlags, name) && hasVal:
			o.multi[name] = append(o.multi[name], val)
		case slices.Contains(valueFlags, a):
			if i+1 >= len(args) || slices.Contains(boolFlags, args[i+1]) || slices.Contains(valueFlags, args[i+1]) {
				die(fmt.Errorf("%s needs a value", a))
			}
			o.multi[a] = append(o.multi[a], args[i+1])
			i++
		case strings.HasPrefix(a, "--"):
			die(fmt.Errorf("unknown flag %s (use -- before text that starts with --)", a))
		default:
			o.pos = append(o.pos, a)
		}
	}
	return o
}

func (o opts) one(k string) string {
	if v := o.multi[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (o opts) arg(i int, what string) string {
	if i >= len(o.pos) {
		die(fmt.Errorf("missing %s\n\n%s", what, ultraUsage))
	}
	return o.pos[i]
}

func cwd() string {
	d, err := os.Getwd()
	if err != nil {
		die(err)
	}
	return d
}

// modeCLI shows or sets the project's default mode.
func modeCLI(args []string) {
	p, err := mode.Find(cwd())
	if err != nil {
		die(err)
	}
	if len(args) == 0 {
		m, err := mode.Default(cwd())
		fmt.Printf("%s is this project's default for new sessions (%s)\n", m.Label(), p.Root)
		if err != nil {
			fmt.Printf("note: %v; Classic is used\n", err)
		}
		return
	}
	m, err := mode.Parse(args[0])
	if err != nil {
		die(err)
	}
	if err := p.SetDefault(m); err != nil {
		die(err)
	}
	fmt.Printf("new sessions in %s start in %s; running sessions keep their mode\n", p.Root, m.Label())
}

func ultraCLI(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(ultraUsage)
		return
	}
	rt, err := runtime.Open(cwd())
	if err != nil {
		die(fmt.Errorf("PAI ULTRA could not open this project (Classic is unaffected): %w", err))
	}
	defer rt.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	o := parseOpts(args[1:])
	switch args[0] {
	case "status":
		ultraStatus(ctx, rt)
	case "init":
		ultraInit(rt, o)
	case "skills":
		ultraPackages(rt, project.KindSkill, o)
	case "rules":
		ultraPackages(rt, project.KindRule, o)
	case "profile":
		s := rt.Project.Begin()
		switch o.arg(0, "apply or remove") {
		case "apply":
			s.ApplyProfile(o.arg(1, "profile"))
		case "remove":
			s.RemoveProfile(o.arg(1, "profile"))
		default:
			die(errors.New("profile apply|remove NAME"))
		}
		applyStage(s, o.bools["--dry-run"])
	case "instructions":
		ultraInstructions(rt, o)
	case "context":
		ultraContext(rt, o)
	case "task":
		if o.arg(0, "new") != "new" {
			die(errors.New("task new OBJECTIVE"))
		}
		t := newTask(rt, o, 1)
		fmt.Println(t.ID)
	case "run":
		id := o.arg(0, "task or objective")
		if _, err := rt.Store.Get(id); err != nil {
			if !errors.Is(err, task.ErrNotFound) || strings.HasPrefix(id, "t-") && len(o.pos) == 1 {
				die(err) // a mistyped task id must not become an objective
			}
			id = newTask(rt, o, 0).ID
			fmt.Println("task", id)
		}
		runTask(ctx, rt, id, o.one("--executor"))
	case "resume":
		runTask(ctx, rt, o.arg(0, "task"), o.one("--executor"))
	case "tasks", "runs":
		ultraTasks(rt)
	case "show":
		ultraShow(rt, o.arg(0, "task"))
	case "checkpoint":
		cp, err := rt.Store.Checkpoint(o.arg(0, "task"))
		if err != nil {
			die(err)
		}
		fmt.Printf("checkpoint %s at %s\n", cp.ID, cp.Dir)
		for _, n := range cp.Workspace.Notes {
			fmt.Println("note:", n)
		}
	case "restore":
		cp, err := rt.Store.LoadCheckpoint(o.arg(0, "task"), "")
		if err != nil {
			die(err)
		}
		dir, _ := filepath.Abs(o.arg(1, "directory"))
		if err := task.Restore(cp, rt.Project.Root, dir, o.bools["--partial"]); err != nil {
			die(err)
		}
		fmt.Println("restored checkpoint", cp.ID, "in", dir)
	case "approve":
		t, err := rt.Approve(ctx, o.arg(0, "task"), o.arg(1, "criterion"), "person", strings.Join(o.pos[2:], " "))
		if err != nil {
			die(err)
		}
		fmt.Println("task", t.ID, "is", t.State)
	case "reconcile":
		st := o.arg(2, "done or failed")
		t, err := rt.Store.ResolveOp(o.arg(0, "task"), o.arg(1, "op"), st, strings.Join(o.pos[3:], " "))
		if err != nil {
			die(err)
		}
		fmt.Println("task", t.ID, "is", t.State)
	case "reopen":
		n, err := strconv.Atoi(o.arg(1, "attempts"))
		if err != nil {
			die(err)
		}
		t, err := rt.Store.Reopen(o.arg(0, "task"), n, "person", strings.Join(o.pos[2:], " "))
		if err != nil {
			die(err)
		}
		fmt.Println("task", t.ID, "is", t.State, "with", t.MaxAttempts-t.Attempts, "attempts left")
	case "amend":
		t, err := rt.Store.AmendCriteria(o.arg(0, "task"), task.Guard{}, criteria(o), "person", strings.Join(o.pos[1:], " "))
		if err != nil {
			die(err)
		}
		fmt.Println("criteria amended; recorded in decisions:", t.Decisions[len(t.Decisions)-1])
	case "recover":
		ids, err := rt.Recover()
		if err != nil {
			die(err)
		}
		fmt.Printf("%d task(s) recovered %v\n", len(ids), ids)
	case "delegate":
		nt := taskOpts(rt, o, 1)
		t, err := rt.Delegate(o.arg(0, "parent"), nt)
		if err != nil {
			die(err)
		}
		fmt.Println(t.ID, "in", t.Worktree)
	case "integrate":
		files, err := rt.Integrate(ctx, o.arg(0, "child"))
		if err != nil {
			die(err)
		}
		fmt.Println("integrated:", strings.Join(files, ", "))
	case "handoff":
		ultraHandoff(ctx, rt, o)
	case "memory":
		ultraMemory(ctx, rt, o)
	case "metrics":
		ms, err := rt.Metrics.Read()
		if err != nil {
			die(err)
		}
		fmt.Println(runtime.MarshalIndent(metrics.Summarize(ms)))
	default:
		fmt.Print(ultraUsage)
		os.Exit(2)
	}
}

func ultraStatus(ctx context.Context, rt *runtime.Runtime) {
	p := rt.Project
	m, merr := mode.Default(p.Root)
	fmt.Printf("project   %s\nmode      %s (default for new sessions)\n", p.Root, m.Label())
	if merr != nil {
		fmt.Println("note     ", merr)
	}
	fmt.Printf("config    %s", project.RevisionLabel(p.Revision()))
	switch {
	case !p.HasManifest:
		fmt.Print(" (no .pai/project.yaml yet: pai ultra init)")
	case p.ManifestEdited:
		fmt.Print(" (project.yaml edited since the lock: apply to re-lock)")
	}
	fmt.Println()
	r := p.Resolve()
	for _, e := range r.Enabled {
		fmt.Printf("  %-6s %-24s %-8s %s\n", e.Kind, e.Entry.Name, e.Pkg.Meta.VersionOf(), e.Status)
	}
	for _, pr := range r.Problems {
		fmt.Println("  ", pr)
	}
	fmt.Println("executors")
	for _, n := range []string{"claude", "codex", "pai"} {
		a := rt.Adapters[n]
		state := "available"
		if n == "pai" {
			state = "on demand (needs a PAI server)"
		} else if err := a.Available(ctx); err != nil {
			state = "unavailable: " + err.Error()
		}
		fmt.Printf("  %-7s %s\n", n, state)
	}
	ts, _ := rt.Store.List()
	counts := map[task.State]int{}
	for _, t := range ts {
		counts[t.State]++
	}
	fmt.Printf("tasks     %d", len(ts))
	for _, s := range []task.State{task.Running, task.Blocked, task.Paused, task.Pending, task.Accepted, task.Failed} {
		if counts[s] > 0 {
			fmt.Printf(", %d %s", counts[s], s)
		}
	}
	fmt.Println()
}

func ultraInit(rt *runtime.Runtime, o opts) {
	s := rt.Project.Begin()
	for _, p := range o.multi["--profile"] {
		s.ApplyProfile(p)
	}
	if e := o.one("--executor"); e != "" {
		s.SetExecutor(e)
	}
	for i, c := range o.multi["--check"] {
		s.Manifest.Criteria = append(s.Manifest.Criteria, project.Criterion{ID: fmt.Sprintf("check%d", i+1), Command: c})
	}
	if !s.Dirty() && !rt.Project.HasManifest {
		s.Log = append(s.Log, "create .pai/project.yaml and .pai/lock.json")
	}
	applyStage(s, o.bools["--dry-run"])
}

// applyStage previews, then applies unless dry.
func applyStage(s *project.Stage, dry bool) {
	pv, err := s.Preview()
	if err != nil {
		die(err)
	}
	for _, l := range s.Log {
		fmt.Println("·", l)
	}
	for _, d := range pv.Diffs {
		fmt.Printf("--- %s\n%s", d.Path, d.Diff)
	}
	for _, p := range pv.Problems {
		fmt.Println(p)
	}
	if dry {
		fmt.Println("dry run: nothing written")
		return
	}
	rev, err := s.Apply()
	if err != nil {
		die(err)
	}
	fmt.Printf("applied: configuration %s; running tasks keep the revision they started with\n", project.RevisionLabel(rev))
}

func ultraPackages(rt *runtime.Runtime, kind string, o opts) {
	verb := "list"
	if len(o.pos) > 0 {
		verb = o.pos[0]
	}
	p := rt.Project
	if verb == "list" {
		installed, probs := p.Installed()
		r := p.Resolve()
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVERSION\tLIBRARY\tSTATE\t~TOKENS\tDESCRIPTION")
		for _, pk := range installed {
			if pk.Kind != kind {
				continue
			}
			state := "installed"
			if en, ok := r.Find(kind, pk.Name); ok {
				state = "enabled, " + en.Status
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", pk.Name, pk.Meta.VersionOf(), pk.Library, state, pk.Tokens, cut(pk.Meta.Description, 60))
		}
		w.Flush()
		for _, pr := range probs {
			fmt.Println(pr)
		}
		return
	}
	name := o.arg(1, "name")
	if verb == "show" {
		r := p.Resolve()
		var pk *project.Package
		if en, ok := r.Find(kind, name); ok {
			pk = &en.Pkg
			fmt.Printf("enabled via %s, %s, activation %s, paths %v\n", en.Via, en.Status, en.ActivationOf(), en.PathsOf())
			if en.Override != nil {
				fmt.Println("project override:", en.Override.Path)
			}
		} else {
			installed, _ := p.Installed()
			if i := slices.IndexFunc(installed, func(x project.Package) bool { return x.Kind == kind && x.Name == name }); i >= 0 {
				pk = &installed[i]
				fmt.Println("installed, not enabled for this project")
			}
		}
		if pk == nil {
			die(fmt.Errorf("no %s %s", kind, name))
		}
		fmt.Printf("%s %s (%s) %s ~%d tokens (estimate)\nfile: %s\n", pk.Name, pk.Meta.VersionOf(), pk.Library, project.Short(pk.Hash), pk.Tokens, pk.Path)
		if rs := pk.Resources(); len(rs) > 0 {
			fmt.Println("resources:", strings.Join(rs, ", "))
		}
		fmt.Println(strings.Repeat("─", 40))
		fmt.Println(string(pk.Raw))
		return
	}
	s := p.Begin()
	switch verb {
	case "enable":
		s.Enable(kind, name, o.one("--source"))
	case "disable":
		s.Disable(kind, name)
	case "update":
		s.AcceptUpdate(kind, name)
	case "pin":
		s.Pin(kind, name, o.arg(2, "version"))
	case "unpin":
		s.Pin(kind, name, "")
	case "activation":
		s.SetActivation(kind, name, project.Activation(o.arg(2, "auto, always or manual")))
	case "paths":
		s.SetPaths(kind, name, o.pos[2:])
	case "new":
		if kind != project.KindRule {
			die(errors.New("skills are installed into a library, not created here"))
		}
		path := p.Path("rules", name+".md")
		if _, err := os.Stat(path); err == nil {
			die(fmt.Errorf("%s exists", path))
		}
		body := editText(fmt.Sprintf("---\ndescription: \nmandatory: true\n---\n# %s\n\n", name))
		if err := s.SetFile(path, []byte(body)); err != nil {
			die(err)
		}
	case "override":
		if err := s.Override(kind, name, nil); err != nil {
			die(err)
		}
		fmt.Println("the package itself is not changed; edit", p.OverridePath(kind, name), "after applying")
	default:
		die(fmt.Errorf("unknown %s command %q", kind, verb))
	}
	applyStage(s, o.bools["--dry-run"])
}

// editText opens $VISUAL or $EDITOR on start and returns the result; with
// neither set it returns start unchanged, for the person to edit later.
func editText(start string) string {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		return start
	}
	f, err := os.CreateTemp("", "pai-ultra-*.md")
	if err != nil {
		die(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(start)
	f.Close()
	if err := runEditor(ed, f.Name()); err != nil {
		die(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		die(err)
	}
	return string(b)
}

func ultraInstructions(rt *runtime.Runtime, o opts) {
	p := rt.Project
	executor := o.one("--executor")
	if executor == "" {
		executor = rt.DefaultExecutor()
	}
	paths := o.multi["--path"]
	sel := project.Apply(p.Inventory(p.Resolve()), executor, paths, nil)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "\tID\tSOURCE\tOWNER\tREV\t~TOKENS\tREASON")
	for _, a := range sel.Applied {
		mark := "-"
		switch {
		case a.Include:
			mark = "+"
		case a.Native:
			mark = "n"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", mark, a.ID, a.Source, a.Owner, a.Revision, a.Tokens, a.Reason)
	}
	w.Flush()
	fmt.Println("+ injected by PAI   n loaded by the executor itself (status unknown)   - not applied")
	fmt.Println("precedence:", strings.Join(p.Manifest.Order(), " > "), "(configured; PAI does not judge free-text contradictions)")
	for _, c := range sel.Conflicts {
		state := "UNRESOLVED"
		if c.Resolved {
			state = "resolved by precedence: " + c.Winner
		}
		fmt.Printf("conflict on %q: %s (%s)\n", c.Key, strings.Join(c.IDs, " vs "), state)
	}
	for _, d := range sel.Duplicates {
		fmt.Printf("duplicate: %q in %s\n", cut(d.Line, 60), strings.Join(d.IDs, ", "))
	}
}

func ultraContext(rt *runtime.Runtime, o opts) {
	var t *task.Task
	if len(o.pos) > 0 {
		got, err := rt.Store.Get(o.pos[0])
		if err != nil {
			die(err)
		}
		t = got
	} else {
		obj := o.one("--objective")
		if obj == "" {
			die(errors.New("context TASK, or --objective TEXT"))
		}
		t = &task.Task{ID: "preview", Objective: obj, Paths: o.multi["--path"], Criteria: criteria(o), Workspace: rt.Project.Root}
	}
	executor := o.one("--executor")
	if executor == "" {
		executor = cmpOr(t.Executor, rt.DefaultExecutor())
	}
	cx, _, err := rt.Context(t, executor)
	if err != nil {
		die(err)
	}
	fmt.Print(cx.Report())
	if o.bools["--json"] {
		fmt.Println(runtime.MarshalIndent(cx))
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// criteria reads --check, --file and --review flags.
func criteria(o opts) []task.Criterion {
	var cs []task.Criterion
	for i, c := range o.multi["--check"] {
		cs = append(cs, task.Criterion{ID: fmt.Sprintf("check%d", i+1), Kind: task.CriterionCommand, Command: c})
	}
	for i, f := range o.multi["--file"] {
		path, text := splitFileArg(f)
		cs = append(cs, task.Criterion{ID: fmt.Sprintf("file%d", i+1), Kind: task.CriterionFile, Path: path, Contains: text})
	}
	for i, r := range o.multi["--review"] {
		cs = append(cs, task.Criterion{ID: fmt.Sprintf("review%d", i+1), Kind: task.CriterionReview, Text: r})
	}
	return cs
}

func taskOpts(rt *runtime.Runtime, o opts, at int) coord.NewTask {
	return coord.NewTask{
		Objective: strings.Join(o.pos[min(at, len(o.pos)):], " "), Criteria: criteria(o), Paths: o.multi["--path"],
		Skills: o.multi["--skill"], Executor: o.one("--executor"), Constraints: o.multi["--constraint"], NoDefaults: o.bools["--no-defaults"],
	}
}

func newTask(rt *runtime.Runtime, o opts, at int) *task.Task {
	t, err := rt.Create(taskOpts(rt, o, at))
	if err != nil {
		die(err)
	}
	return t
}

func runTask(ctx context.Context, rt *runtime.Runtime, id, executor string) {
	start := time.Now()
	out, err := rt.Run(ctx, id, executor, func(e exec.Event) {
		switch e.Kind {
		case "text":
			fmt.Println("│", cut(strings.ReplaceAll(e.Text, "\n", " "), 160))
		case "tool":
			fmt.Println("│ tool:", cut(e.Text, 120))
		case "error":
			fmt.Println("│ error:", e.Text)
		case "start", "done":
			fmt.Printf("· %s %s\n", e.Kind, e.Text)
		}
	})
	if out.Task != nil {
		fmt.Printf("task %s: %s after %d attempt(s), %s\n", out.Task.ID, out.Task.State, out.Task.Attempts, time.Since(start).Round(time.Second))
		fmt.Printf("executor claimed %q; PAI checked:\n", out.Result.Status)
		if out.Validation != nil {
			for _, r := range out.Validation.Results {
				fmt.Printf("  %-12s %-12s %s\n", r.Criterion, r.Status, r.Detail)
			}
		}
		u := out.Result.Usage
		src := "estimated"
		if u.Reported {
			src = "reported by the executor"
		}
		fmt.Printf("tokens: %d in, %d out, %d cache read (%s); context injected ~%d (estimate)\n", u.Input, u.Output, u.CacheRead, src, out.Context.Tokens)
		for _, w := range out.Context.Warnings {
			fmt.Println("warning:", w)
		}
	}
	if err != nil {
		die(err)
	}
}

func ultraTasks(rt *runtime.Runtime) {
	ts, err := rt.Store.List()
	if err != nil {
		die(err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tATTEMPTS\tEXECUTOR\tCONFIG\tOWNER\tCHECKPOINTS\tTITLE")
	for _, t := range ts {
		owner := "-"
		if t.Owner != nil {
			owner = fmt.Sprintf("%s (lease to %s)", t.Owner.ID, t.Owner.Until.Local().Format("15:04:05"))
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\t%s\t%d\t%s\n", t.ID, t.State, t.Attempts, t.MaxAttempts, t.Executor, project.RevisionLabel(t.ConfigRev), owner, len(t.Checkpoints), cut(t.Title, 50))
	}
	w.Flush()
}

func ultraShow(rt *runtime.Runtime, id string) {
	t, err := rt.Store.Get(id)
	if err != nil {
		die(err)
	}
	fmt.Printf("%s  %s  attempts %d/%d  executor %s  config %s (latest %s)\n", t.ID, t.State, t.Attempts, t.MaxAttempts, t.Executor,
		project.RevisionLabel(t.ConfigRev), project.RevisionLabel(rt.Project.Revision()))
	fmt.Println("objective:", t.Objective)
	for _, c := range t.Criteria {
		fmt.Printf("criterion %s: %s\n", c.ID, c.Describe())
	}
	if t.Validation != nil {
		for _, r := range t.Validation.Results {
			fmt.Printf("  checked %s: %s %s\n", r.Criterion, r.Status, r.Detail)
		}
	}
	if t.Report != nil {
		fmt.Printf("executor claimed: %s: %s\n", t.Report.Status, t.Report.Summary)
	}
	for _, b := range t.Blockers {
		fmt.Println("blocker:", b)
	}
	for _, op := range t.Ops {
		fmt.Printf("op %s %s %s %s %s\n", op.ID, op.Kind, op.State, op.Ref, op.Note)
	}
	for _, l := range t.Links {
		fmt.Println("link:", l)
	}
	fmt.Println("checkpoints:", strings.Join(t.Checkpoints, ", "))
	evs, _ := rt.Store.Events(id)
	for _, e := range evs {
		fmt.Printf("  r%-3d %s %-16s %s %s\n", e.Rev, e.Time.Local().Format("15:04:05"), e.Type, e.Actor, e.Note)
	}
}

func ultraMemory(ctx context.Context, rt *runtime.Runtime, o opts) {
	k := rt.Knowledge
	verb := "list"
	if len(o.pos) > 0 {
		verb = o.pos[0]
	}
	print := func(hits []knowledge.Hit) {
		for _, h := range hits {
			fmt.Println(knowledge.Line(h), "·", h.Origin)
		}
	}
	switch verb {
	case "list":
		f := knowledge.Filter{Limit: 200}
		if o.bools["--all"] {
			f.Statuses = knowledge.AllStatuses
		}
		hits, err := k.Search(ctx, "", f)
		if err != nil {
			die(err)
		}
		print(hits)
		cs, _ := k.Conflicts(ctx)
		for _, c := range cs {
			fmt.Printf("conflict on %q: %s\n", c.Key, strings.Join(c.IDs, " vs "))
		}
	case "search":
		hits, err := k.Search(ctx, strings.Join(o.pos[1:], " "), knowledge.Filter{Limit: 50})
		if err != nil {
			die(err)
		}
		print(hits)
	case "add":
		it := knowledge.Item{Kind: o.arg(1, "kind"), Text: strings.Join(o.pos[2:], " "), Origin: "person", Sources: []knowledge.Source{{Kind: "person", Ref: "cli"}}}
		for _, d := range o.multi["--dep"] {
			dep, err := k.FileDep(d)
			if err != nil {
				die(err)
			}
			it.Deps = append(it.Deps, dep)
		}
		got, err := k.Add(ctx, it)
		if err != nil {
			die(err)
		}
		fmt.Println(got.ID, got.Status)
	case "verify":
		method, ev := "person", o.one("--evidence")
		got, err := k.Verify(ctx, o.arg(1, "id"), knowledge.Verification{Method: method, By: "person", Evidence: ev})
		if err != nil {
			die(err)
		}
		fmt.Println(got.ID, got.Status)
	case "invalidate":
		got, err := k.Invalidate(ctx, o.arg(1, "id"), "person", strings.Join(o.pos[2:], " "))
		if err != nil {
			die(err)
		}
		fmt.Println(got.ID, got.Status)
	case "import":
		limit := 500
		if l := o.one("--limit"); l != "" {
			n, err := strconv.Atoi(l)
			if err != nil || n < 1 {
				die(fmt.Errorf("--limit wants a positive number, not %q", l))
			}
			limit = n
		}
		items, err := rt.ClassicMemory(ctx, limit)
		if err != nil {
			die(fmt.Errorf("Classic memory: %w", err))
		}
		plan, err := k.PreviewImport(ctx, items)
		if err != nil {
			die(err)
		}
		for _, it := range plan.Add {
			fmt.Printf("+ candidate %s: %s\n", it.Origin, cut(it.Text, 100))
		}
		for _, s := range plan.Skip {
			fmt.Printf("- skip classic:%s: %s\n", s.ID, s.Why)
		}
		if !o.bools["--apply"] {
			fmt.Printf("preview: %d to import as unverified candidates; run with --apply\n", len(plan.Add))
			return
		}
		n, err := k.Import(ctx, plan)
		if err != nil {
			die(err)
		}
		fmt.Printf("imported %d candidate(s); Classic memory is unchanged\n", n)
	default:
		die(fmt.Errorf("unknown memory command %q", verb))
	}
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// runEditor runs the person's editor on path, attached to this terminal.
func runEditor(editor, path string) error {
	f := strings.Fields(editor)
	if len(f) == 0 {
		return errors.New("the editor setting is empty")
	}
	cmd := osexec.Command(f[0], append(f[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func ultraHandoff(ctx context.Context, rt *runtime.Runtime, o opts) {
	c, err := rt.Server.Client(ctx)
	if err != nil {
		die(fmt.Errorf("a handoff needs the PAI server Classic runs on: %w", err))
	}
	if sid := o.one("--from-classic"); sid != "" {
		p, err := handoff.PlanFromClassic(ctx, c, sid)
		if err != nil {
			die(err)
		}
		fmt.Printf("from Classic session %s (%d messages)\nobjective:\n%s\n\nnot carried:\n", sid, p.Messages, p.Objective)
		for _, u := range p.Unsupported {
			fmt.Println("  -", u)
		}
		if !o.bools["--apply"] {
			fmt.Println("preview only: run again with --apply to create the ULTRA task (it will not start)")
			return
		}
		t, err := handoff.ApplyFromClassic(rt.Coordinator, p)
		if err != nil {
			if t != nil {
				fmt.Println("task", t.ID, "was created; finishing it failed:")
			}
			die(err)
		}
		fmt.Printf("task %s created and linked; the Classic session is unchanged. Start it with: pai ultra run %s\n", t.ID, t.ID)
		return
	}
	if o.one("--to") != "classic" {
		die(errors.New("handoff TASK --to classic, or handoff --from-classic SESSION"))
	}
	id := o.arg(0, "task")
	fmt.Println("handing task", id, "to a new PAI Classic session; not carried:")
	for _, u := range handoff.ExplainToClassic() {
		fmt.Println("  -", u)
	}
	if !o.bools["--apply"] {
		fmt.Println("preview only: run again with --apply to hand off")
		return
	}
	out, err := handoff.TaskToClassic(ctx, c, rt.Coordinator, id)
	if err != nil {
		if out.Session != "" {
			fmt.Println("Classic session", out.Session, "was created; handoff text:", out.HandoffPath)
		}
		die(err)
	}
	fmt.Printf("Classic session %s created (nothing sent to a model yet).\nhandoff text (~%d tokens, estimate): %s\nopen it with `pai %s`: the TUI puts the handoff in the input for you to send; or paste the file\n",
		out.Session, out.Tokens, out.HandoffPath, out.Session)
}

// splitFileArg splits PATH[:TEXT], keeping a Windows drive letter (C:\x) in
// the path.
func splitFileArg(f string) (string, string) {
	skip := 0
	if len(f) >= 2 && f[1] == ':' && (f[0]|0x20 >= 'a' && f[0]|0x20 <= 'z') {
		skip = 2
	}
	if i := strings.Index(f[skip:], ":"); i >= 0 {
		return f[:skip+i], f[skip+i+1:]
	}
	return f, ""
}

// appUltra opens the TUI on PAI ULTRA, the project's default mode. The
// server is optional: without one ULTRA runs offline with local executors.
func appUltra(dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), startTime)
	defer cancel()
	var cp *connector.Client
	note := ""
	c, err := connector.Load(ctx)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		note = "offline: not signed in to a PAI server (pai login); local executors work"
	case err != nil:
		note = "offline: the PAI server is unreachable (" + err.Error() + "); local executors work"
	default:
		v, verr := c.Version(ctx)
		switch {
		case verr != nil:
			note = "offline: the PAI server did not answer (" + verr.Error() + "); local executors work"
		case !version.Match(v):
			rerun(v)
		default:
			cp = c
		}
	}
	v, err := tui.RunUltra(cp, dir, note)
	if err != nil {
		log.Fatal(err)
	}
	if v != "" {
		rerun(v)
	}
}
