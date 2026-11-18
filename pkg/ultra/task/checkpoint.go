package task

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"pai/pkg/contextflow"
	"pai/pkg/ultra/internal/atomicfile"
)

const (
	// CheckpointVersion is the portable checkpoint schema this build writes.
	CheckpointVersion = 1
	// Untracked files are copied into the checkpoint up to these sizes;
	// bigger ones are listed as not recoverable.
	maxUntrackedFile  = 2 << 20
	maxUntrackedTotal = 32 << 20
	// A directory without git is fingerprinted from at most this many files.
	maxFingerprintFiles = 5000
	emptyTree           = "4b825dc642cb6eb9a060e54bf8d69288fbee4904" // git's empty tree
	patchName           = "patch.diff"
	gitTimeout          = 2 * time.Minute
)

// FileState is one changed file's content hash at checkpoint time.
type FileState struct {
	Path     string `json:"path"` // slash path from the root
	Hash     string `json:"sha256,omitempty"`
	Size     int64  `json:"size"`
	Artifact string `json:"artifact,omitempty"` // copy inside the checkpoint, for untracked files
	Omitted  string `json:"omitted,omitempty"`  // why no copy was kept
	Deleted  bool   `json:"deleted,omitempty"`
	Mode     uint32 `json:"mode,omitempty"` // permission bits, restored with the file
}

// Workspace is the repository state a checkpoint can be restored to.
type Workspace struct {
	Root        string      `json:"root"`
	Git         bool        `json:"git"`
	Head        string      `json:"head,omitempty"`
	Branch      string      `json:"branch,omitempty"`
	Patch       string      `json:"patch,omitempty"` // tracked changes against Head, as an artifact
	PatchHash   string      `json:"patch_sha256,omitempty"`
	Changed     []FileState `json:"changed,omitempty"` // tracked files with changes
	Untracked   []FileState `json:"untracked,omitempty"`
	Fingerprint string      `json:"fingerprint"`
	Notes       []string    `json:"notes,omitempty"`
}

// Checkpoint is a versioned, portable snapshot of a task: enough for a fresh
// executor, in either mode, to continue without the original session.
type Checkpoint struct {
	Version        int               `json:"version"`
	ID             string            `json:"id"`
	TaskID         string            `json:"task_id"`
	TaskRev        int               `json:"task_rev"`
	State          State             `json:"state"`
	Created        time.Time         `json:"created"`
	Mode           string            `json:"mode"`
	Objective      string            `json:"objective"`
	Criteria       []Criterion       `json:"criteria"`
	CriteriaHash   string            `json:"criteria_sha256"`
	Constraints    []string          `json:"constraints,omitempty"`
	Decisions      []string          `json:"decisions,omitempty"`
	Paths          []string          `json:"paths,omitempty"`
	Workspace      Workspace         `json:"workspace"`
	Completed      []string          `json:"completed,omitempty"`
	Verification   []CheckResult     `json:"verification,omitempty"`
	Hypotheses     []string          `json:"hypotheses,omitempty"`
	Blockers       []string          `json:"blockers,omitempty"`
	FailedAttempts []string          `json:"failed_attempts,omitempty"`
	NextAction     string            `json:"next_action,omitempty"`
	Inflight       []Op              `json:"inflight,omitempty"`
	Owner          string            `json:"owner,omitempty"`
	ConfigRev      int               `json:"config_revision"`
	Executor       string            `json:"executor,omitempty"`
	Native         map[string]string `json:"native,omitempty"` // optional: never required to resume
	Report         *Report           `json:"report,omitempty"`
	Links          []string          `json:"links,omitempty"`
	Dir            string            `json:"-"` // where it is stored
}

func git(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func splitZ(b []byte) []string {
	var out []string
	for f := range bytes.SplitSeq(b, []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out
}

// Capture records root's state into dir, copying the patch and untracked
// files there so the checkpoint is recoverable on its own.
//
// Capture is consistent: it captures again when the workspace changed while
// it read, up to captureTries times, and says so when it never settled.
func Capture(root, dir string) (Workspace, error) {
	var w Workspace
	var err error
	for try := 1; try <= captureTries; try++ {
		if err := os.RemoveAll(dir); err != nil {
			return w, err
		}
		w, err = captureOnce(root, dir)
		if err != nil || !w.Git || settled(root, w) {
			return w, err
		}
	}
	w.Notes = append(w.Notes, "the workspace kept changing during capture; the checkpoint may mix two moments")
	return w, nil
}

// captureTries bounds re-captures of a workspace that changes while read.
const captureTries = 3

// settled is whether root still matches what was captured.
func settled(root string, w Workspace) bool {
	base := w.Head
	if base == "" {
		base = emptyTree
	}
	patch, err := git(root, "diff", "--binary", base)
	if err != nil || (len(patch) > 0) != (w.PatchHash != "") || (len(patch) > 0 && sum(patch) != w.PatchHash) {
		return false
	}
	for _, f := range append(slices.Clone(w.Changed), w.Untracked...) {
		if fileState(root, f.Path).Hash != f.Hash {
			return false
		}
	}
	return true
}

func captureOnce(root, dir string) (Workspace, error) {
	w := Workspace{Root: root}
	if _, err := git(root, "rev-parse", "--is-inside-work-tree"); err != nil {
		return captureTree(root)
	}
	w.Git = true
	base := emptyTree
	if out, err := git(root, "rev-parse", "HEAD"); err == nil {
		w.Head = strings.TrimSpace(string(out))
		base = w.Head
	} else {
		w.Notes = append(w.Notes, "no commit yet: changes are recorded against the empty tree")
	}
	if out, err := git(root, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		w.Branch = strings.TrimSpace(string(out))
	}
	// Tracked changes, staged or not, against HEAD. Intent-to-add files show here too.
	patch, err := git(root, "diff", "--binary", base)
	if err != nil {
		return w, err
	}
	if len(patch) > 0 {
		if err := atomicfile.Write(filepath.Join(dir, patchName), patch, 0o644); err != nil {
			return w, err
		}
		w.Patch, w.PatchHash = patchName, sum(patch)
	}
	names, err := git(root, "diff", "--name-only", "-z", base)
	if err != nil {
		return w, err
	}
	for _, n := range splitZ(names) {
		w.Changed = append(w.Changed, fileState(root, n))
	}
	if _, err := git(root, "diff", "--cached", "--quiet", base); err != nil {
		w.Notes = append(w.Notes, "staged and unstaged changes are recorded together; a restore leaves them unstaged")
	}
	others, err := git(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return w, err
	}
	var total int64
	for _, n := range splitZ(others) {
		fsx := FileState{Path: n}
		p := filepath.Join(root, filepath.FromSlash(n))
		st, lerr := os.Lstat(p)
		switch {
		case lerr != nil:
			fsx.Omitted = lerr.Error()
		case st.Mode()&fs.ModeSymlink != 0:
			fsx.Omitted = "symbolic link"
		case st.Size() > maxUntrackedFile:
			fsx.Size, fsx.Omitted = st.Size(), fmt.Sprintf("larger than %d bytes", maxUntrackedFile)
		case total+st.Size() > maxUntrackedTotal:
			fsx.Size, fsx.Omitted = st.Size(), fmt.Sprintf("untracked copies past %d bytes", maxUntrackedTotal)
		default:
			// Read once: the hash and the copy are of the same bytes.
			b, err := os.ReadFile(p)
			if err == nil {
				fsx.Hash, fsx.Size, fsx.Mode = sum(b), int64(len(b)), uint32(st.Mode().Perm())
				fsx.Artifact = "untracked/" + n
				err = atomicfile.Write(filepath.Join(dir, "untracked", filepath.FromSlash(n)), b, 0o644)
				total += fsx.Size
			}
			if err != nil {
				fsx.Artifact, fsx.Omitted = "", err.Error()
			}
		}
		if fsx.Hash == "" {
			fsx.Hash = fileState(root, n).Hash
		}
		if fsx.Omitted != "" {
			w.Notes = append(w.Notes, "untracked "+n+" not recoverable: "+fsx.Omitted)
		}
		w.Untracked = append(w.Untracked, fsx)
	}
	w.Fingerprint = fingerprint(w)
	return w, nil
}

func fileState(root, rel string) FileState {
	f := FileState{Path: rel}
	p := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		f.Deleted = true
		return f
	}
	if err != nil {
		return f
	}
	f.Mode = uint32(st.Mode().Perm())
	if st.Mode()&fs.ModeSymlink != 0 {
		target, _ := os.Readlink(p)
		f.Hash = sum([]byte("symlink:" + target))
		return f
	}
	if b, err := os.ReadFile(p); err == nil {
		f.Hash, f.Size = sum(b), int64(len(b))
	}
	return f
}

// safeRel is whether p is a relative slash path that stays inside its base.
func safeRel(p string) bool {
	if p == "" || strings.Contains(p, `\`) || strings.HasPrefix(p, "/") || filepath.VolumeName(filepath.FromSlash(p)) != "" {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}

// within is whether path, with symbolic links resolved for its existing
// part, lies under base.
func within(base, path string) bool {
	b, err := filepath.EvalSymlinks(base)
	if err != nil {
		return false
	}
	dir := filepath.Dir(path)
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			rel, err := filepath.Rel(b, r)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func fingerprint(w Workspace) string {
	var b strings.Builder
	b.WriteString(w.Head + "\n" + w.PatchHash + "\n")
	for _, set := range [][]FileState{w.Changed, w.Untracked} {
		xs := slices.Clone(set)
		slices.SortFunc(xs, func(a, b FileState) int { return strings.Compare(a.Path, b.Path) })
		for _, f := range xs {
			fmt.Fprintf(&b, "%s %s %v\n", f.Path, f.Hash, f.Deleted)
		}
	}
	return sum([]byte(b.String()))
}

// captureTree fingerprints a directory that is not a git work tree from file
// sizes and times, not contents, and within bounds: such a directory can be
// as large as a home folder. Its changes cannot be restored from the
// checkpoint; that is noted.
func captureTree(root string) (Workspace, error) {
	w := Workspace{Root: root, Notes: []string{"not a git repository: file changes are fingerprinted by size and time, not recoverable"}}
	n, deadline := 0, time.Now().Add(treeTime)
	cut := ""
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != root && (strings.HasPrefix(d.Name(), ".") || slices.Contains(treeSkip, d.Name())) {
			return filepath.SkipDir
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if n++; n > maxFingerprintFiles {
			cut = fmt.Sprintf("fingerprint covers the first %d files", maxFingerprintFiles)
			return filepath.SkipAll
		}
		if time.Now().After(deadline) {
			cut = fmt.Sprintf("fingerprint stopped after %s", treeTime)
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		w.Changed = append(w.Changed, FileState{Path: filepath.ToSlash(rel), Size: info.Size(),
			Hash: fmt.Sprintf("stat:%d:%d", info.Size(), info.ModTime().UnixNano())})
		return nil
	})
	if cut != "" {
		w.Notes = append(w.Notes, cut)
	}
	w.Fingerprint = fingerprint(w)
	return w, err
}

// Bounds on fingerprinting a directory without git.
const treeTime = 3 * time.Second

// treeSkip is directories never worth fingerprinting: caches and app data.
var treeSkip = []string{"node_modules", "AppData", "Library", "__pycache__", "target", "vendor"}

// Drift is how a workspace differs from a checkpoint's.
type Drift struct {
	Same        bool     `json:"same"`
	HeadChanged bool     `json:"head_changed"`
	Head        string   `json:"head,omitempty"`
	Changed     []string `json:"changed,omitempty"` // files whose content differs from the checkpoint's
	Missing     []string `json:"missing,omitempty"` // artifacts the checkpoint lacks
	Notes       []string `json:"notes,omitempty"`
}

// Compare checks root against the checkpoint's workspace.
func Compare(cp *Checkpoint, root string) Drift {
	d := Drift{}
	tmp, err := os.MkdirTemp("", "pai-ultra-compare-")
	if err != nil {
		d.Notes = append(d.Notes, err.Error())
		return d
	}
	defer os.RemoveAll(tmp)
	now, err := Capture(root, tmp)
	if err != nil {
		d.Notes = append(d.Notes, "could not read the workspace: "+err.Error())
		return d
	}
	old := cp.Workspace
	d.Head = now.Head
	d.HeadChanged = old.Git && now.Head != old.Head
	prev := map[string]FileState{}
	for _, f := range append(slices.Clone(old.Changed), old.Untracked...) {
		prev[f.Path] = f
	}
	cur := map[string]FileState{}
	for _, f := range append(slices.Clone(now.Changed), now.Untracked...) {
		cur[f.Path] = f
	}
	for p, f := range cur {
		if o, ok := prev[p]; !ok || o.Hash != f.Hash || o.Deleted != f.Deleted {
			d.Changed = append(d.Changed, p)
		}
	}
	for p := range prev {
		if _, ok := cur[p]; !ok {
			d.Changed = append(d.Changed, p) // reverted or committed since
		}
	}
	slices.Sort(d.Changed)
	for _, f := range old.Untracked {
		if f.Omitted != "" {
			d.Missing = append(d.Missing, f.Path)
		}
	}
	d.Same = !d.HeadChanged && len(d.Changed) == 0 && now.Fingerprint == old.Fingerprint
	return d
}

// Restore rebuilds the checkpoint's workspace in dir, a new git worktree of
// repo at the checkpoint's head, with its patch applied and untracked files
// copied. It never touches the original work tree. Paths in the checkpoint
// are checked to stay inside the checkpoint and dir. Untracked files the
// checkpoint could not keep make it fail, unless partial accepts that.
func Restore(cp *Checkpoint, repo, dir string, partial bool) error {
	w := cp.Workspace
	if !w.Git || w.Head == "" {
		return errors.New("this checkpoint's workspace cannot be restored: no git commit to start from")
	}
	if w.Patch != "" && w.Patch != patchName {
		return fmt.Errorf("checkpoint patch %q is not %s", w.Patch, patchName)
	}
	var missing []string
	for _, f := range w.Untracked {
		if !safeRel(f.Path) || (f.Artifact != "" && (!safeRel(f.Artifact) || !strings.HasPrefix(f.Artifact, "untracked/"))) {
			return fmt.Errorf("checkpoint names an unsafe path %q", f.Path)
		}
		if f.Artifact == "" {
			missing = append(missing, f.Path)
		}
	}
	if len(missing) > 0 && !partial {
		return fmt.Errorf("these untracked files were not kept and cannot be restored: %s", strings.Join(capList(missing, 10), ", "))
	}
	if _, err := git(repo, "cat-file", "-e", w.Head+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s is not in %s: fetch it first", w.Head, repo)
	}
	if _, err := git(repo, "worktree", "add", "--detach", dir, w.Head); err != nil {
		return err
	}
	if w.Patch != "" {
		patch := filepath.Join(cp.Dir, w.Patch)
		b, err := os.ReadFile(patch)
		if err != nil {
			return err
		}
		if sum(b) != w.PatchHash {
			return fmt.Errorf("%s does not match its recorded hash", patch)
		}
		if _, err := git(dir, "apply", "--binary", "--whitespace=nowarn", patch); err != nil {
			return err
		}
	}
	for _, f := range w.Untracked {
		if f.Artifact == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(cp.Dir, filepath.FromSlash(f.Artifact)))
		if err != nil {
			return err
		}
		if sum(b) != f.Hash {
			return fmt.Errorf("artifact %s does not match its recorded hash", f.Artifact)
		}
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if !within(dir, dst) {
			return fmt.Errorf("untracked %s would land outside %s", f.Path, dir)
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := atomicfile.Write(dst, b, mode); err != nil {
			return err
		}
	}
	return nil
}

// checkpointsDir is where a task's checkpoints live.
func (s *Store) checkpointsDir(id string) string { return filepath.Join(s.dir(id), "checkpoints") }

// Checkpoint captures the task and its workspace and records the checkpoint
// on the task. Any owner, or none, may checkpoint; it changes no state.
func (s *Store) Checkpoint(id string) (*Checkpoint, error) {
	for try := 1; ; try++ {
		cp, err := s.checkpointOnce(id)
		if !errors.Is(err, ErrRevision) || try == captureTries {
			return cp, err
		}
	}
}

// checkpointOnce publishes a checkpoint only if the task's contract did not
// change while it was captured: same state, criteria and decisions. Lease
// renewals in between do not matter.
func (s *Store) checkpointOnce(id string) (*Checkpoint, error) {
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	cpID := NewID("cp")
	dir := filepath.Join(s.checkpointsDir(id), cpID)
	root := t.Workspace
	if t.Worktree != "" {
		root = t.Worktree
	}
	ws, err := Capture(root, dir)
	if err != nil {
		return nil, fmt.Errorf("capture workspace: %w", err)
	}
	cp := FromTask(t, ws)
	cp.ID, cp.Dir = cpID, dir
	if err := atomicfile.WriteJSON(filepath.Join(dir, "checkpoint.json"), cp, 0o644); err != nil {
		return nil, err
	}
	_, err = s.Update(id, Guard{}, func(cur *Task) (Event, error) {
		if cur.State != t.State || cur.CriteriaHash != t.CriteriaHash || len(cur.Decisions) != len(t.Decisions) || len(cur.Ops) != len(t.Ops) {
			return Event{}, ErrRevision
		}
		cur.Checkpoints = append(cur.Checkpoints, cpID)
		return Event{Type: "checkpoint", Note: cpID}, nil
	})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return cp, nil
}

// FromTask builds a checkpoint from a task and a captured workspace.
func FromTask(t *Task, ws Workspace) *Checkpoint {
	cp := &Checkpoint{
		Version: CheckpointVersion, TaskID: t.ID, TaskRev: t.Rev, State: t.State, Created: time.Now().UTC(), Mode: t.Mode,
		Objective: t.Objective, Criteria: t.Criteria, CriteriaHash: t.CriteriaHash, Constraints: t.Constraints, Decisions: t.Decisions,
		Paths: t.Paths, Workspace: ws, Completed: t.Completed, Hypotheses: t.Hypotheses, Blockers: t.Blockers,
		FailedAttempts: t.FailedAttempts, NextAction: t.NextAction, Inflight: t.Unknown(), ConfigRev: t.ConfigRev,
		Executor: t.Executor, Native: t.Native, Report: t.Report, Links: t.Links,
	}
	if t.Owner != nil {
		cp.Owner = t.Owner.ID
	}
	if t.Validation != nil {
		cp.Verification = t.Validation.Results
	}
	return cp
}

// LoadCheckpoint reads a checkpoint; "" is the task's newest.
func (s *Store) LoadCheckpoint(taskID, cpID string) (*Checkpoint, error) {
	if cpID == "" {
		t, err := s.Get(taskID)
		if err != nil {
			return nil, err
		}
		if len(t.Checkpoints) == 0 {
			return nil, fmt.Errorf("task %s has no checkpoint", taskID)
		}
		cpID = t.Checkpoints[len(t.Checkpoints)-1]
	}
	return ReadCheckpoint(filepath.Join(s.checkpointsDir(taskID), cpID))
}

// ReadCheckpoint reads the checkpoint stored in dir.
func ReadCheckpoint(dir string) (*Checkpoint, error) {
	var cp Checkpoint
	if err := atomicfile.ReadJSON(filepath.Join(dir, "checkpoint.json"), &cp); err != nil {
		return nil, err
	}
	if cp.Version > CheckpointVersion {
		return nil, fmt.Errorf("checkpoint version %d is newer than this PAI understands", cp.Version)
	}
	cp.Dir = dir
	return &cp, nil
}

// Handoff is a checkpoint rendered for a fresh executor.
type Handoff struct {
	Text      string `json:"text"`
	Tokens    int    `json:"tokens"` // estimated
	Budget    int    `json:"budget"`
	Overrun   bool   `json:"overrun"` // mandatory content alone exceeds the budget
	Truncated int    `json:"truncated"`
}

// Render writes the checkpoint as a bounded handoff. Objective, criteria,
// constraints, decisions, blockers, unknown operations and the next action
// are always kept whole; history fills what budget is left, newest first.
func (cp *Checkpoint) Render(budget int, drift *Drift) Handoff {
	mustText, opt := cp.Parts(drift, true)
	h := Handoff{Budget: budget}
	text := mustText
	used := tokens(text)
	if used > budget {
		h.Overrun = true
	}
	// Reserve room for the footer that says how much was left out.
	footer := fmt.Sprintf("(%d older entries omitted; full checkpoint: %s)\n", len(opt), filepath.Join(cp.Dir, "checkpoint.json"))
	reserve := tokens(footer)
	var kept []string
	for _, o := range opt {
		if t := tokens(o + "\n"); used+t+reserve <= budget {
			kept = append(kept, o)
			used += t
		} else {
			h.Truncated++
		}
	}
	if len(kept) > 0 {
		text += strings.Join(kept, "\n") + "\n"
	}
	if h.Truncated > 0 {
		text += fmt.Sprintf("(%d older entries omitted; full checkpoint: %s)\n", h.Truncated, filepath.Join(cp.Dir, "checkpoint.json"))
	}
	h.Text, h.Tokens = text, tokens(text)
	return h
}

// Parts splits the checkpoint into what a resume must carry whole and its
// history, newest first, one line each. Without contract it leaves out the
// objective, criteria, constraints and decisions, for a caller that states
// those itself.
func (cp *Checkpoint) Parts(drift *Drift, contract bool) (string, []string) {
	var must strings.Builder
	line := func(b *strings.Builder, format string, a ...any) { fmt.Fprintf(b, format+"\n", a...) }
	line(&must, "## Task %s (checkpoint %s, task rev %d, config r%d)", cp.TaskID, cp.ID, cp.TaskRev, cp.ConfigRev)
	if contract {
		line(&must, "Objective: %s", cp.Objective)
		if len(cp.Criteria) > 0 {
			line(&must, "Acceptance criteria (checked by PAI, not by you; do not change them):")
			for _, c := range cp.Criteria {
				line(&must, "- [%s] %s", c.ID, c.Describe())
			}
		}
		for _, c := range cp.Constraints {
			line(&must, "Constraint: %s", c)
		}
		for _, d := range cp.Decisions {
			line(&must, "Decision: %s", d)
		}
	}
	ws := cp.Workspace
	if ws.Git {
		line(&must, "Workspace: %s at %s (%s), %d changed and %d untracked files at checkpoint.", ws.Root, short(ws.Head), ws.Branch, len(ws.Changed), len(ws.Untracked))
	}
	for _, n := range ws.Notes {
		line(&must, "Workspace note: %s", n)
	}
	if drift != nil && !drift.Same {
		line(&must, "The workspace CHANGED since this checkpoint. Re-read before relying on earlier work:")
		if drift.HeadChanged {
			line(&must, "- HEAD moved to %s", short(drift.Head))
		}
		for _, f := range capList(drift.Changed, 30) {
			line(&must, "- %s", f)
		}
	}
	for _, o := range cp.Inflight {
		line(&must, "UNCERTAIN: operation %s (%s) was %s; it may have partly or fully happened. Check its effects before redoing it.", o.ID, o.Desc, o.State)
	}
	for _, b := range cp.Blockers {
		line(&must, "Blocker: %s", b)
	}
	if cp.NextAction != "" {
		line(&must, "Next action: %s", cp.NextAction)
	}
	var opt []string
	if cp.Report != nil && cp.Report.Summary != "" {
		opt = append(opt, fmt.Sprintf("Last executor report (%s, unverified): %s", cp.Report.Status, cp.Report.Summary))
	}
	for _, v := range slices.Backward(cp.Verification) {
		opt = append(opt, fmt.Sprintf("Check %s: %s %s", v.Criterion, v.Status, firstLine(v.Detail)))
	}
	for _, f := range slices.Backward(cp.FailedAttempts) {
		opt = append(opt, "Failed attempt: "+f)
	}
	for _, x := range slices.Backward(cp.Hypotheses) {
		opt = append(opt, "Hypothesis (unverified): "+x)
	}
	for _, c := range slices.Backward(cp.Completed) {
		opt = append(opt, "Done: "+c)
	}
	return must.String(), opt
}

func tokens(s string) int {
	return (len(s) + contextflow.BytesPerToken - 1) / contextflow.BytesPerToken
}

func short(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

func capList(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(slices.Clone(xs[:n]), fmt.Sprintf("… and %d more", len(xs)-n))
}

// EnvFingerprint identifies the environment a check ran in: the platform and
// the resolved command binary with its size and time.
func EnvFingerprint(command string) string {
	parts := []string{runtime.GOOS + "/" + runtime.GOARCH}
	if f := strings.Fields(command); len(f) > 0 {
		if p, err := exec.LookPath(f[0]); err == nil {
			if st, err := os.Stat(p); err == nil {
				parts = append(parts, fmt.Sprintf("%s:%d:%d", p, st.Size(), st.ModTime().Unix()))
			}
		}
	}
	return strings.Join(parts, " ")
}

// Inside is whether path lies under base once symbolic links in its
// existing part are resolved: a link or junction cannot carry a read or a
// write outside base.
func Inside(base, path string) bool { return within(base, path) }
