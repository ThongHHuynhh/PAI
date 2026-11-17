package coord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"

	"pai/pkg/ultra/task"
)

// Delegate creates a child task that works in its own git worktree, on a
// branch from the parent workspace's HEAD, so it cannot disturb the parent's
// tree. The base commit is recorded. Its result reaches the parent only
// through Integrate, after the child is accepted.
func (c *Coordinator) Delegate(parentID string, nt NewTask) (*task.Task, error) {
	parent, err := c.Store.Get(parentID)
	if err != nil {
		return nil, err
	}
	if parent.Parent != "" {
		return nil, errors.New("a delegated task cannot delegate again")
	}
	base, err := gitRun(workdir(parent), "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("the parent workspace needs a commit to branch from: %w", err)
	}
	id := task.NewID("t")
	dir := filepath.Join(c.Project.State, "worktrees", id)
	if _, err := gitRun(workdir(parent), "worktree", "add", "-b", "ultra/"+id, dir, "HEAD"); err != nil {
		return nil, fmt.Errorf("isolated workspace: %w", err)
	}
	nt.ID, nt.Parent, nt.Worktree = id, parentID, dir
	if nt.Executor == "" {
		nt.Executor = parent.Executor
	}
	nt.Constraints = append(nt.Constraints, "Work only in this isolated worktree; the parent integrates accepted results.")
	t, err := c.Create(nt)
	if err != nil {
		gitRun(workdir(parent), "worktree", "remove", "--force", dir)
		return nil, err
	}
	return c.Store.Update(t.ID, task.Guard{}, func(t *task.Task) (task.Event, error) {
		t.Base = strings.TrimSpace(base)
		return task.Event{Type: "delegated", Note: "base " + t.Base}, nil
	})
}

// Integrate applies an accepted child's work to its parent's workspace. It
// takes the work as it was accepted, from the checkpoint the decision was
// made on, relative to the recorded base: commits the child made, then its
// uncommitted changes, then its untracked files. Edits made after acceptance
// are not carried. Patches apply all or nothing (git apply --check first);
// untracked files must not exist in the parent and cannot be written
// through a link out of it. It returns the files changed.
func (c *Coordinator) Integrate(ctx context.Context, childID string) ([]string, error) {
	child, err := c.Store.Get(childID)
	if err != nil {
		return nil, err
	}
	if child.State != task.Accepted {
		return nil, fmt.Errorf("child is %s: only accepted work is integrated", child.State)
	}
	if child.Parent == "" || child.Worktree == "" || child.Base == "" {
		return nil, errors.New("not a delegated task with a recorded base")
	}
	cp, err := c.Store.LoadCheckpoint(childID, "")
	if err != nil {
		return nil, fmt.Errorf("accepted checkpoint: %w", err)
	}
	for _, r := range cp.Verification {
		if r.Status != "pass" {
			return nil, fmt.Errorf("the newest checkpoint was not the accepted state (%s: %s)", r.Criterion, r.Status)
		}
	}
	parent, err := c.Store.Get(child.Parent)
	if err != nil {
		return nil, err
	}
	dst := workdir(parent)
	ws := cp.Workspace
	var patches []string
	if ws.Head != "" && ws.Head != child.Base {
		committed, err := gitRun(child.Worktree, "diff", "--binary", child.Base, ws.Head)
		if err != nil {
			return nil, err
		}
		f := filepath.Join(cp.Dir, "committed.diff")
		if err := os.WriteFile(f, []byte(committed), 0o644); err != nil {
			return nil, err
		}
		patches = append(patches, f)
	}
	if ws.Patch != "" {
		patches = append(patches, filepath.Join(cp.Dir, ws.Patch))
	}
	for _, f := range ws.Untracked {
		if f.Artifact == "" {
			return nil, fmt.Errorf("untracked %s was not kept: %s", f.Path, f.Omitted)
		}
		p := filepath.Join(dst, filepath.FromSlash(f.Path))
		if !filepath.IsLocal(filepath.FromSlash(f.Path)) || !task.Inside(dst, p) {
			return nil, fmt.Errorf("untracked %s would land outside the parent workspace", f.Path)
		}
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("%s already exists in the parent workspace", f.Path)
		}
	}
	for _, p := range patches {
		if _, err := gitRun(dst, "apply", "--check", "--binary", p); err != nil {
			return nil, fmt.Errorf("the child's work does not apply cleanly: %w", err)
		}
	}
	var files []string
	for _, p := range patches {
		if _, err := gitRun(dst, "apply", "--binary", p); err != nil {
			return files, err
		}
	}
	names, _ := gitRun(child.Worktree, "diff", "--name-only", child.Base, ws.Head)
	for n := range strings.Lines(names) {
		if n = strings.TrimSpace(n); n != "" {
			files = append(files, n)
		}
	}
	for _, f := range ws.Changed {
		files = append(files, f.Path)
	}
	for _, f := range ws.Untracked {
		b, err := os.ReadFile(filepath.Join(cp.Dir, filepath.FromSlash(f.Artifact)))
		if err != nil {
			return files, err
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f.Hash {
			return files, fmt.Errorf("artifact %s does not match its recorded hash", f.Artifact)
		}
		p := filepath.Join(dst, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return files, err
		}
		if !task.Inside(dst, p) {
			return files, fmt.Errorf("untracked %s would land outside the parent workspace", f.Path)
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return files, err
		}
		_, werr := out.Write(b)
		if err := errors.Join(werr, out.Close()); err != nil {
			return files, err
		}
		files = append(files, f.Path)
	}
	_, err = c.Store.Update(parent.ID, task.Guard{}, func(t *task.Task) (task.Event, error) {
		t.Completed = append(t.Completed, fmt.Sprintf("integrated child %s (checkpoint %s): %s", childID, cp.ID, strings.Join(files, ", ")))
		return task.Event{Type: "integrated", Note: childID}, nil
	})
	return files, err
}

func gitRun(dir string, args ...string) (string, error) {
	cmd := osexec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
