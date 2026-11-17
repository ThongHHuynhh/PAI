package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"pai/pkg/ultra/internal/atomicfile"
	"pai/pkg/ultra/mode"
)

// Library names that always exist.
const (
	UserLibrary    = "user"    // ~/.pai/ultra/library: installed on this machine
	ProjectLibrary = "project" // .pai/library: packages committed with the project
	overridesDir   = "overrides"
	rulesDir       = "rules"
)

// Project is an opened ULTRA project.
type Project struct {
	mode.Project
	Manifest Manifest
	Lock     Lock
	// HasManifest is false for a project not yet initialized for ULTRA:
	// everything works on the zero manifest, nothing is written until an apply.
	HasManifest bool
	// ManifestEdited is true when project.yaml no longer matches the lock: a
	// person edited it by hand. The lock still pins content; the next apply
	// re-locks from the edited manifest.
	ManifestEdited bool
	manifestRaw    []byte
}

// Path is a path inside the project's .pai directory.
func (p *Project) Path(parts ...string) string {
	return filepath.Join(append([]string{p.Root, Dir}, parts...)...)
}

// Open reads the project of dir. It first finishes or discards an apply a
// crash interrupted, so the manifest and lock are never half written.
func Open(dir string) (*Project, error) {
	mp, err := mode.Find(dir)
	if err != nil {
		return nil, err
	}
	p := &Project{Project: mp}
	if err := p.recoverApply(); err != nil {
		return nil, fmt.Errorf("finish an interrupted apply: %w", err)
	}
	return p, p.readConsistent()
}

func (p *Project) reload() error {
	raw, err := os.ReadFile(p.Path(ManifestFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.Manifest, p.HasManifest, p.manifestRaw = Manifest{Version: ManifestVersion}, false, nil
	case err != nil:
		return err
	default:
		m, err := ParseManifest(raw)
		if err != nil {
			return err
		}
		p.Manifest, p.HasManifest, p.manifestRaw = m, true, raw
	}
	p.Lock = Lock{Version: LockVersion}
	if err := atomicfile.ReadJSON(p.Path(LockFile), &p.Lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", LockFile, err)
	}
	if p.Lock.Version > LockVersion {
		return fmt.Errorf("%s: version %d is newer than this PAI understands", LockFile, p.Lock.Version)
	}
	p.ManifestEdited = p.HasManifest && p.Lock.Revision > 0 && p.Lock.ManifestHash != hashBytes(raw)
	return nil
}

// readConsistent reloads, and when the manifest and lock disagree while an
// apply is pending (a reader raced its renames), waits for that apply and
// reads again.
func (p *Project) readConsistent() error {
	if err := p.reload(); err != nil {
		return err
	}
	if !p.ManifestEdited {
		return nil
	}
	if _, err := os.Stat(p.Path(pendingDir)); err != nil {
		return nil // a hand edit, not an apply in flight
	}
	if err := p.recoverApply(); err != nil {
		return err
	}
	return p.reload()
}

// Revision is the applied configuration revision.
func (p *Project) Revision() int { return p.Lock.Revision }

// Libraries is every library in order: the project's, the machine's, then
// the manifest's.
func (p *Project) Libraries() []Library {
	libs := []Library{{Name: ProjectLibrary, Path: filepath.Join(p.Root, Dir, "library")}}
	if home, err := mode.Home(); err == nil {
		libs = append(libs, Library{Name: UserLibrary, Path: filepath.Join(home, "library")})
	}
	for _, l := range p.Manifest.Libraries {
		if !slices.ContainsFunc(libs, func(o Library) bool { return o.Name == l.Name }) {
			libs = append(libs, l)
		}
	}
	return libs
}

// Installed is every package in every library.
func (p *Project) Installed() ([]Package, []Problem) {
	var all []Package
	var ps []Problem
	for _, l := range p.Libraries() {
		pk, pr := Scan(l, p.Root)
		all = append(all, pk...)
		ps = append(ps, pr...)
	}
	return all, ps
}

// cacheDir holds locked package content by hash, so a lock still means the
// same bytes after a library is updated.
func cacheDir(hash string) (string, error) {
	if !lockHash.MatchString(hash) {
		return "", fmt.Errorf("bad content hash %q", hash)
	}
	home, err := mode.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "cache", strings.TrimPrefix(hash, "sha256:")), nil
}

var lockHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// safeLockPath is whether a lock's file path is relative and stays inside
// its package: a lockfile comes with the repository and is not trusted.
func safeLockPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, ":") {
		return false
	}
	return !slices.ContainsFunc(strings.Split(p, "/"), func(s string) bool { return s == ".." || s == "." || s == "" })
}

// cachePut copies a package's files into the cache.
func cachePut(pk Package) error {
	dir, err := cacheDir(pk.Hash)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return nil
	}
	for _, f := range pk.Files {
		src := filepath.Join(pk.Dir, filepath.FromSlash(f.Path))
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if hashBytes(b) != f.Hash {
			return fmt.Errorf("%s changed while being locked", src)
		}
		if err := atomicfile.Write(filepath.Join(dir, filepath.FromSlash(f.Path)), b, 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, ".complete"), nil, 0o644)
}

// cacheGet rebuilds a locked package from the cache, checking every hash.
func cacheGet(l Locked) (Package, error) {
	dir, err := cacheDir(l.Hash)
	if err != nil {
		return Package{}, err
	}
	main := skillFile
	if l.Kind != KindSkill && len(l.Files) == 1 {
		main = l.Files[0].Path
	}
	if hashFiles(l.Files) != l.Hash {
		return Package{}, fmt.Errorf("lock entry %s does not match its file list", l.Name)
	}
	for _, f := range l.Files {
		if !safeLockPath(f.Path) {
			return Package{}, fmt.Errorf("lock entry %s names an unsafe path %q", l.Name, f.Path)
		}
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return Package{}, err
		}
		if hashBytes(b) != f.Hash {
			return Package{}, fmt.Errorf("cached %s/%s is corrupt", l.Name, f.Path)
		}
	}
	pk, ps := parsePackage(l.Kind, l.Source, filepath.Join(dir, filepath.FromSlash(main)))
	if l.Kind == KindSkill {
		pk.Files, pk.Hash, _ = hashDir(dir) // .complete is a dot file and skipped
		if pk.Hash != l.Hash {
			return Package{}, fmt.Errorf("cached %s holds files the lock does not name", l.Name)
		}
	}
	if Blocking(ps) {
		return pk, errors.New(ps[0].Message)
	}
	return pk, nil
}

// Enabled is one package the project selects, as resolved.
type Enabled struct {
	Entry     Entry
	Kind      string
	Via       string   // "manifest" or "profile:<name>"
	Pkg       Package  // the content in effect
	Installed *Package // the library's current content, when installed
	Locked    *Locked
	Override  *Package // the project's override, for a package it edits
	Status    string   // see the Status constants
}

// Statuses of an enabled package.
const (
	StatusLocked  = "locked"           // in effect exactly as locked
	StatusNew     = "not locked yet"   // enabled, locked at the next apply
	StatusUpdate  = "update available" // the library holds newer content than the lock
	StatusMissing = "missing"          // neither the cache nor a library has it
)

// Mandatory is whether a rule must be loaded when it applies.
func (e Enabled) Mandatory() bool {
	if e.Entry.Mandatory != nil {
		return *e.Entry.Mandatory
	}
	if e.Pkg.Meta.Mandatory != nil {
		return *e.Pkg.Meta.Mandatory
	}
	return e.Kind == KindRule
}

// ActivationOf is the activation in effect: the manifest's, else the package's, else auto.
func (e Enabled) ActivationOf() Activation {
	if e.Entry.Activation != "" {
		return e.Entry.Activation
	}
	if e.Pkg.Meta.Activation != "" {
		return e.Pkg.Meta.Activation
	}
	return Auto
}

// PathsOf is the path scope in effect.
func (e Enabled) PathsOf() []string {
	if len(e.Entry.Paths) > 0 {
		return e.Entry.Paths
	}
	return e.Pkg.Meta.Paths
}

// Text is the instructions in effect: the override's when there is one.
func (e Enabled) Text() string {
	if e.Override != nil {
		return e.Override.Body
	}
	return e.Pkg.Body
}

// Resolved is the manifest applied to the libraries and the lock.
type Resolved struct {
	Enabled  []Enabled
	Lock     Lock // the lock this resolution would write; Revision unchanged
	Problems []Problem
}

// Find is the enabled package kind/name.
func (r Resolved) Find(kind, name string) (Enabled, bool) {
	for _, e := range r.Enabled {
		if e.Kind == kind && (e.Pkg.Name == name || e.Entry.Name == name) {
			return e, true
		}
	}
	return Enabled{}, false
}

// Resolve resolves the project's manifest and lock.
func (p *Project) Resolve() Resolved { return p.resolve(p.Manifest, p.Lock, nil, nil) }

type entryVia struct {
	Entry
	kind, via string
}

// effectiveEntries expands profiles, then lets the manifest's own entries
// replace a profile's by name.
func effectiveEntries(m Manifest, installed []Package) ([]entryVia, []Problem) {
	var out []entryVia
	var ps []Problem
	put := func(kind, via string, e Entry) {
		i := slices.IndexFunc(out, func(o entryVia) bool { return o.kind == kind && o.Name == e.Name })
		if i >= 0 {
			out = slices.Delete(out, i, i+1)
		}
		if !e.Disabled {
			out = append(out, entryVia{e, kind, via})
		}
	}
	for _, name := range m.Profiles {
		i := slices.IndexFunc(installed, func(p Package) bool { return p.Kind == KindProfile && p.Name == name })
		if i < 0 {
			ps = append(ps, errorf(KindProfile, name, "not installed"))
			continue
		}
		for _, e := range installed[i].Meta.Skills {
			put(KindSkill, "profile:"+name, e)
		}
		for _, e := range installed[i].Meta.Rules {
			put(KindRule, "profile:"+name, e)
		}
	}
	for _, e := range m.Skills {
		put(KindSkill, "manifest", e)
	}
	for _, e := range m.Rules {
		put(KindRule, "manifest", e)
	}
	return out, ps
}

// resolve takes the library's content instead of the lock's for every
// kind/name in accept: an accepted update.
func (p *Project) resolve(m Manifest, lock Lock, accept map[string]bool, overlay map[string][]byte) Resolved {
	r := Resolved{Problems: m.Validate()}
	installed, ps := p.Installed()
	r.Problems = append(r.Problems, ps...)
	entries, ps := effectiveEntries(m, installed)
	r.Problems = append(r.Problems, ps...)
	r.Lock = Lock{Version: LockVersion, Revision: lock.Revision}
	for _, ev := range entries {
		cands := slices.DeleteFunc(slices.Clone(installed), func(pk Package) bool {
			return pk.Kind != ev.kind || pk.Name != ev.Name || (ev.Source != "" && pk.Library != ev.Source)
		})
		en := Enabled{Entry: ev.Entry, Kind: ev.kind, Via: ev.via}
		if len(cands) > 1 {
			var libs []string
			for _, c := range cands {
				libs = append(libs, c.Library)
			}
			r.Problems = append(r.Problems, errorf(ev.kind, ev.Name, "in several libraries (%s): set source", strings.Join(libs, ", ")))
			continue
		}
		if len(cands) == 1 {
			en.Installed = &cands[0]
		}
		if l, ok := lock.Find(ev.kind, ev.Name); ok && !accept[ev.kind+"/"+ev.Name] {
			en.Locked = &l
		}
		switch {
		case en.Locked != nil && en.Installed != nil && en.Installed.Hash == en.Locked.Hash:
			en.Pkg, en.Status = *en.Installed, StatusLocked
		case en.Locked != nil:
			pk, err := cacheGet(*en.Locked)
			if err != nil {
				en.Status = StatusMissing
				r.Problems = append(r.Problems, errorf(ev.kind, ev.Name, "locked content %s is not in the cache or a library: %v", short(en.Locked.Hash), err))
				r.Enabled = append(r.Enabled, en)
				continue
			}
			en.Pkg, en.Status = pk, StatusLocked
			if en.Installed != nil {
				en.Status = StatusUpdate
			}
		case en.Installed != nil:
			en.Pkg, en.Status = *en.Installed, StatusNew
		default:
			en.Status = StatusMissing
			r.Problems = append(r.Problems, errorf(ev.kind, ev.Name, "enabled but not installed in any library"))
			r.Enabled = append(r.Enabled, en)
			continue
		}
		if ev.Version != "" && en.Pkg.Meta.VersionOf() != ev.Version {
			r.Problems = append(r.Problems, errorf(ev.kind, ev.Name, "pinned to version %s, but the content in effect declares %q", ev.Version, en.Pkg.Meta.VersionOf()))
		}
		if ov, err := p.override(ev.kind, ev.Name, overlay); err == nil {
			en.Override = &ov
		} else if !errors.Is(err, fs.ErrNotExist) {
			r.Problems = append(r.Problems, errorf(ev.kind, ev.Name, "override: %v", err))
		}
		lk := Locked{Kind: ev.kind, Name: ev.Name, Source: en.Pkg.Library, Version: en.Pkg.Meta.VersionOf(), Hash: en.Pkg.Hash, Files: en.Pkg.Files}
		if en.Locked != nil {
			lk.Source = en.Locked.Source
		}
		if en.Override != nil {
			lk.Override = en.Override.Hash
		}
		r.Lock.Packages = append(r.Lock.Packages, lk)
		r.Enabled = append(r.Enabled, en)
	}
	slices.SortFunc(r.Lock.Packages, func(a, b Locked) int { return strings.Compare(a.Kind+"/"+a.Name, b.Kind+"/"+b.Name) })
	return r
}

// OverridePath is where the project's edit of a package's instructions lives.
func (p *Project) OverridePath(kind, name string) string {
	return p.Path(overridesDir, kind+"s", name+".md")
}

func (p *Project) override(kind, name string, overlay map[string][]byte) (Package, error) {
	f := p.OverridePath(kind, name)
	raw, staged := overlay[f]
	switch {
	case staged && raw == nil:
		return Package{}, fs.ErrNotExist // staged for deletion
	case !staged:
		b, err := os.ReadFile(f)
		if err != nil {
			return Package{}, err
		}
		raw = b
	}
	pk, ps := parseRaw(KindRule, ProjectLibrary, f, raw)
	if Blocking(ps) {
		return pk, errors.New(ps[0].Message)
	}
	return pk, nil
}

func short(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Short is a hash cut for display.
func Short(h string) string { return short(h) }
