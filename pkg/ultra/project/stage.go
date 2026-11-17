package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"pai/pkg/ultra/internal/atomicfile"
	"pai/pkg/ultra/internal/lockfile"
)

const (
	pendingDir = ".pending"
	commitMark = "COMMIT"
	planFile   = "plan.json"
	applyLock  = ".apply.lock"
	// applyWait and applyStale bound waiting for, and trusting, another
	// process's apply. An apply writes a few small files.
	applyWait  = 5 * time.Second
	applyStale = 2 * time.Minute
	gitignore  = ".pending/\n.apply.lock\n"
)

var (
	// ErrConflict: the configuration changed since this stage began.
	ErrConflict = errors.New("the project's configuration changed since these edits began")
	// ErrInvalid: the staged configuration has errors; nothing was written.
	ErrInvalid = errors.New("the staged configuration is invalid")
	// ErrReadOnly: the file is not the project's to edit.
	ErrReadOnly = errors.New("not a project-owned file")
)

// Stage collects edits to apply together, or discard. Nothing touches the
// project's files until Apply, which writes the manifest, lock and any
// project files all or none.
type Stage struct {
	p            *Project
	baseRev      int
	baseManifest string
	Manifest     Manifest
	files        map[string][]byte // absolute path -> new content; nil deletes
	accept       map[string]bool
	Log          []string // what each edit did, in order, for review
}

// Begin starts staging from the project's current configuration.
func (p *Project) Begin() *Stage {
	m := p.Manifest
	m.Skills, m.Rules, m.Profiles = slices.Clone(m.Skills), slices.Clone(m.Rules), slices.Clone(m.Profiles)
	m.Libraries, m.Criteria = slices.Clone(m.Libraries), slices.Clone(m.Criteria)
	return &Stage{p: p, baseRev: p.Lock.Revision, baseManifest: hashBytes(p.manifestRaw), Manifest: m, files: map[string][]byte{}, accept: map[string]bool{}}
}

// Dirty is whether anything is staged.
func (s *Stage) Dirty() bool { return len(s.Log) > 0 }

func (s *Stage) logf(format string, a ...any) { s.Log = append(s.Log, fmt.Sprintf(format, a...)) }

func (s *Stage) list(kind string) *[]Entry {
	if kind == KindSkill {
		return &s.Manifest.Skills
	}
	return &s.Manifest.Rules
}

// entry is the manifest's own entry for kind/name, copying one a profile
// brought in, or making one.
func (s *Stage) entry(kind, name string) *Entry {
	l := s.list(kind)
	if i := slices.IndexFunc(*l, func(e Entry) bool { return e.Name == name }); i >= 0 {
		return &(*l)[i]
	}
	e := Entry{Name: name}
	r := s.p.resolve(s.Manifest, s.p.Lock, s.accept, s.files)
	if en, ok := r.Find(kind, name); ok {
		e = en.Entry
	}
	*l = append(*l, e)
	return &(*l)[len(*l)-1]
}

// Enable enables kind/name from source ("" for wherever it is installed).
func (s *Stage) Enable(kind, name, source string) {
	e := s.entry(kind, name)
	e.Disabled = false
	if source != "" {
		e.Source = source
	}
	s.logf("enable %s %s", kind, name)
}

// Disable disables kind/name. One a profile brings in stays listed as disabled.
func (s *Stage) Disable(kind, name string) {
	viaProfile := false
	if en, ok := s.p.resolve(s.Manifest, s.p.Lock, s.accept, s.files).Find(kind, name); ok && strings.HasPrefix(en.Via, "profile:") {
		viaProfile = true
	}
	l := s.list(kind)
	*l = slices.DeleteFunc(*l, func(e Entry) bool { return e.Name == name })
	if viaProfile || s.fromProfile(kind, name) {
		*l = append(*l, Entry{Name: name, Disabled: true})
	}
	s.logf("disable %s %s", kind, name)
}

func (s *Stage) fromProfile(kind, name string) bool {
	installed, _ := s.p.Installed()
	for _, prof := range s.Manifest.Profiles {
		for _, pk := range installed {
			if pk.Kind != KindProfile || pk.Name != prof {
				continue
			}
			es := pk.Meta.Skills
			if kind == KindRule {
				es = pk.Meta.Rules
			}
			if slices.ContainsFunc(es, func(e Entry) bool { return e.Name == name }) {
				return true
			}
		}
	}
	return false
}

// SetActivation sets when kind/name is selected.
func (s *Stage) SetActivation(kind, name string, a Activation) {
	s.entry(kind, name).Activation = a
	s.logf("%s %s activation: %s", kind, name, a)
}

// SetPaths sets kind/name's path scope; none is the whole project.
func (s *Stage) SetPaths(kind, name string, paths []string) {
	s.entry(kind, name).Paths = paths
	s.logf("%s %s paths: %s", kind, name, strings.Join(paths, ", "))
}

// ApplyProfile adds a profile.
func (s *Stage) ApplyProfile(name string) {
	if !slices.Contains(s.Manifest.Profiles, name) {
		s.Manifest.Profiles = append(s.Manifest.Profiles, name)
	}
	s.logf("apply profile %s", name)
}

// RemoveProfile removes a profile; its packages go unless listed on their own.
func (s *Stage) RemoveProfile(name string) {
	s.Manifest.Profiles = slices.DeleteFunc(s.Manifest.Profiles, func(n string) bool { return n == name })
	s.logf("remove profile %s", name)
}

// AcceptUpdate locks kind/name to the library's current content.
func (s *Stage) AcceptUpdate(kind, name string) {
	s.accept[kind+"/"+name] = true
	s.logf("update %s %s to the library's content", kind, name)
}

// Pin requires kind/name's content to declare version; "" unpins.
func (s *Stage) Pin(kind, name, version string) {
	s.entry(kind, name).Version = version
	if version == "" {
		s.logf("unpin %s %s", kind, name)
	} else {
		s.logf("pin %s %s to %s", kind, name, version)
	}
}

// SetExecutor sets the default executor.
func (s *Stage) SetExecutor(name string) {
	s.Manifest.Executor.Default = name
	s.logf("default executor: %s", name)
}

// projectOwned is whether ULTRA may write rel, a slash path from the root.
func projectOwned(rel string) bool {
	switch rel {
	case "AGENTS.md", "CLAUDE.md", "CLAUDE.local.md":
		return true
	}
	for _, pre := range []string{Dir + "/rules/", Dir + "/overrides/", Dir + "/library/"} {
		if strings.HasPrefix(rel, pre) && !strings.Contains(rel, "..") {
			return true
		}
	}
	return false
}

// SetFile stages new content for a project-owned file; nil deletes it.
func (s *Stage) SetFile(path string, content []byte) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(s.p.Root, abs)
	if err != nil || !projectOwned(filepath.ToSlash(rel)) {
		return fmt.Errorf("%w: %s", ErrReadOnly, path)
	}
	s.files[abs] = content
	if content == nil {
		s.logf("delete %s", filepath.ToSlash(rel))
	} else {
		s.logf("write %s", filepath.ToSlash(rel))
	}
	return nil
}

// Override stages a project override of a package's instructions, leaving
// the package itself untouched. With nil content it starts from the
// package's current text.
func (s *Stage) Override(kind, name string, content []byte) error {
	r := s.p.resolve(s.Manifest, s.p.Lock, s.accept, s.files)
	en, ok := r.Find(kind, name)
	if !ok {
		return fmt.Errorf("%s %s is not enabled", kind, name)
	}
	if content == nil {
		content = en.Pkg.Raw
	}
	return s.SetFile(s.p.OverridePath(kind, name), content)
}

// FileDiff is one file's staged change.
type FileDiff struct {
	Path string // relative to the root
	Diff string
}

// Preview is what Apply would do.
type Preview struct {
	Diffs    []FileDiff
	Problems []Problem
	Lock     Lock
	Revision int // the revision Apply would make
	manifest []byte
}

// Preview resolves the staged configuration without writing anything.
func (s *Stage) Preview() (Preview, error) {
	mb, err := s.Manifest.Marshal()
	if err != nil {
		return Preview{}, err
	}
	r := s.p.resolve(s.Manifest, s.p.Lock, s.accept, s.files)
	pv := Preview{Problems: r.Problems, Lock: r.Lock, Revision: s.baseRev + 1, manifest: mb}
	pv.Lock.Revision, pv.Lock.ManifestHash = pv.Revision, hashBytes(mb)
	old := string(s.p.manifestRaw)
	if d := Diff(old, string(mb)); d != "" {
		pv.Diffs = append(pv.Diffs, FileDiff{Dir + "/" + ManifestFile, d})
	}
	oldLock, _ := os.ReadFile(s.p.Path(LockFile))
	if d := lockDiff(s.p.Lock, pv.Lock); d != "" || len(oldLock) == 0 {
		pv.Diffs = append(pv.Diffs, FileDiff{Dir + "/" + LockFile, d + fmt.Sprintf("~ revision %s -> %s\n", RevisionLabel(s.baseRev), RevisionLabel(pv.Revision))})
	}
	paths := slices.Sorted(func(yield func(string) bool) {
		for k := range s.files {
			if !yield(k) {
				return
			}
		}
	})
	for _, abs := range paths {
		before, _ := os.ReadFile(abs)
		rel, _ := filepath.Rel(s.p.Root, abs)
		after := s.files[abs]
		d := Diff(string(before), string(after))
		if after == nil {
			d = "~ deleted\n"
		}
		if d != "" {
			pv.Diffs = append(pv.Diffs, FileDiff{filepath.ToSlash(rel), d})
		}
	}
	return pv, nil
}

// lockDiff lists packages added, removed or changed between two locks.
func lockDiff(a, b Lock) string {
	var out strings.Builder
	for _, p := range b.Packages {
		o, ok := a.Find(p.Kind, p.Name)
		switch {
		case !ok:
			fmt.Fprintf(&out, "+ %s %s %s %s\n", p.Kind, p.Name, p.Version, short(p.Hash))
		case o.Hash != p.Hash || o.Override != p.Override:
			fmt.Fprintf(&out, "~ %s %s %s %s -> %s %s\n", p.Kind, p.Name, o.Version, short(o.Hash+o.Override), p.Version, short(p.Hash+p.Override))
		}
	}
	for _, p := range a.Packages {
		if _, ok := b.Find(p.Kind, p.Name); !ok {
			fmt.Fprintf(&out, "- %s %s %s %s\n", p.Kind, p.Name, p.Version, short(p.Hash))
		}
	}
	return out.String()
}

// InvalidError carries the problems that stopped an apply.
type InvalidError struct{ Problems []Problem }

func (e *InvalidError) Error() string {
	var ms []string
	for _, p := range e.Problems {
		if p.Severity == "error" {
			ms = append(ms, p.String())
		}
	}
	return ErrInvalid.Error() + ": " + strings.Join(ms, "; ")
}
func (e *InvalidError) Unwrap() error { return ErrInvalid }

type plannedWrite struct {
	Target string `json:"target"` // relative to the root
	Staged string `json:"staged"` // name in the pending directory
}
type plan struct {
	Writes  []plannedWrite `json:"writes"`
	Deletes []string       `json:"deletes"`
}

// Apply writes the staged configuration: all files or none, even across a
// crash. It refuses invalid configuration and a configuration that changed
// since Begin. It returns the new revision.
func (s *Stage) Apply() (int, error) {
	release, err := lockfile.Acquire(s.p.Path(applyLock), applyWait, applyStale)
	if err != nil {
		return 0, err
	}
	defer release()
	if err := s.p.reload(); err != nil {
		return 0, err
	}
	if s.p.Lock.Revision != s.baseRev || hashBytes(s.p.manifestRaw) != s.baseManifest {
		return 0, ErrConflict
	}
	pv, err := s.Preview()
	if err != nil {
		return 0, err
	}
	if Blocking(pv.Problems) {
		return 0, &InvalidError{pv.Problems}
	}
	// Cache every locked package's exact bytes before the lock names them.
	r := s.p.resolve(s.Manifest, s.p.Lock, s.accept, s.files)
	for _, en := range r.Enabled {
		if en.Status == StatusNew || (en.Status == StatusLocked && en.Installed != nil && en.Installed.Hash == en.Pkg.Hash) {
			if err := cachePut(en.Pkg); err != nil {
				return 0, fmt.Errorf("cache %s %s: %w", en.Kind, en.Pkg.Name, err)
			}
		}
	}
	pv.Lock.Generated = time.Now().UTC()
	lockBytes, err := marshalLock(pv.Lock)
	if err != nil {
		return 0, err
	}
	pend := s.p.Path(pendingDir)
	if err := os.RemoveAll(pend); err != nil {
		return 0, err
	}
	var pl plan
	n := 0
	stage := func(target string, b []byte) error {
		n++
		name := strconv.Itoa(n)
		pl.Writes = append(pl.Writes, plannedWrite{Target: target, Staged: name})
		return atomicfile.Write(filepath.Join(pend, name), b, 0o644)
	}
	if err := stage(Dir+"/"+ManifestFile, pv.manifest); err != nil {
		return 0, err
	}
	if err := stage(Dir+"/"+LockFile, lockBytes); err != nil {
		return 0, err
	}
	if _, err := os.Stat(s.p.Path(".gitignore")); errors.Is(err, fs.ErrNotExist) {
		if err := stage(Dir+"/.gitignore", []byte(gitignore)); err != nil {
			return 0, err
		}
	}
	for abs, b := range s.files {
		rel, _ := filepath.Rel(s.p.Root, abs)
		if b == nil {
			pl.Deletes = append(pl.Deletes, filepath.ToSlash(rel))
		} else if err := stage(filepath.ToSlash(rel), b); err != nil {
			return 0, err
		}
	}
	if err := atomicfile.WriteJSON(filepath.Join(pend, planFile), pl, 0o644); err != nil {
		return 0, err
	}
	// The commit mark is the point of no return: before it, recovery
	// discards; after it, recovery finishes.
	if err := atomicfile.Write(filepath.Join(pend, commitMark), []byte("1\n"), 0o644); err != nil {
		return 0, err
	}
	if err := s.p.rollForward(); err != nil {
		return 0, err
	}
	if err := s.p.reload(); err != nil {
		return 0, err
	}
	s.Log, s.files, s.accept = nil, map[string][]byte{}, map[string]bool{}
	s.baseRev, s.baseManifest = s.p.Lock.Revision, hashBytes(s.p.manifestRaw)
	return s.p.Lock.Revision, nil
}

func marshalLock(l Lock) ([]byte, error) {
	if l.Packages == nil {
		l.Packages = []Locked{}
	}
	b, err := json.MarshalIndent(l, "", "  ")
	return append(b, '\n'), err
}

// recoverApply finishes a committed apply and discards an uncommitted one.
func (p *Project) recoverApply() error {
	pend := p.Path(pendingDir)
	if _, err := os.Stat(pend); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	release, err := lockfile.Acquire(p.Path(applyLock), applyWait, applyStale)
	if err != nil {
		return err
	}
	defer release()
	if _, err := os.Stat(filepath.Join(pend, commitMark)); err != nil {
		return os.RemoveAll(pend)
	}
	return p.rollForward()
}

// rollForward moves every staged file into place. It is safe to repeat: a
// staged file already moved is skipped.
func (p *Project) rollForward() error {
	pend := p.Path(pendingDir)
	var pl plan
	if err := atomicfile.ReadJSON(filepath.Join(pend, planFile), &pl); err != nil {
		return err
	}
	for _, w := range pl.Writes {
		src := filepath.Join(pend, w.Staged)
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		dst := filepath.Join(p.Root, filepath.FromSlash(w.Target))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	for _, d := range pl.Deletes {
		if err := os.Remove(filepath.Join(p.Root, filepath.FromSlash(d))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(pend)
}
