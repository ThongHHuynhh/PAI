package project

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pai/pkg/ultra/internal/atomicfile"
)

// fixture makes an isolated ULTRA home with a user library, and a project.
func fixture(t *testing.T) (*Project, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PAI_ULTRA_HOME", home)
	t.Setenv("HOME", t.TempDir()) // no real global instructions
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	lib := filepath.Join(home, "library")
	write(t, filepath.Join(lib, "skills", "go-testing", "SKILL.md"), "---\nname: go-testing\ndescription: Write table-driven Go tests.\nmetadata:\n  version: \"1.0.0\"\n  tags: go,testing\n---\n# Go testing\nUse t.Run for each case.\n")
	write(t, filepath.Join(lib, "skills", "go-testing", "reference.md"), "Extra reference.\n")
	write(t, filepath.Join(lib, "rules", "no-push.md"), "---\ndescription: Never push without asking.\nversion: \"1\"\nkey: push\nvalue: ask-first\n---\nNever push to a remote without asking the person first.\n")
	write(t, filepath.Join(lib, "rules", "web-style.md"), "---\ndescription: Web style.\npaths: [\"web/**\"]\n---\nUse two-space indentation in web sources always.\n")
	write(t, filepath.Join(lib, "profiles", "go-service.yaml"), "name: go-service\ndescription: Go service defaults\nskills:\n  - name: go-testing\nrules:\n  - name: no-push\n")
	root := t.TempDir()
	p, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return p, lib
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := atomicfile.Write(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func apply(t *testing.T, s *Stage) int {
	t.Helper()
	rev, err := s.Apply()
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestUninitializedProjectWritesNothing(t *testing.T) {
	p, _ := fixture(t)
	if p.HasManifest || p.Revision() != 0 {
		t.Fatal("fresh project claims configuration")
	}
	r := p.Resolve()
	if len(r.Enabled) != 0 || Blocking(r.Problems) {
		t.Fatalf("zero manifest resolved to %+v", r)
	}
	if _, err := os.Stat(p.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening created .pai")
	}
}

func TestEnableApplyLocksExactContent(t *testing.T) {
	p, _ := fixture(t)
	s := p.Begin()
	s.Enable(KindSkill, "go-testing", "")
	s.Enable(KindRule, "no-push", "")
	pv, err := s.Preview()
	if err != nil || Blocking(pv.Problems) {
		t.Fatalf("preview: %v %+v", err, pv.Problems)
	}
	if len(pv.Diffs) < 2 {
		t.Fatalf("preview shows no diff: %+v", pv.Diffs)
	}
	if _, err := os.Stat(p.Path(ManifestFile)); err == nil {
		t.Fatal("preview wrote the manifest")
	}
	if rev := apply(t, s); rev != 1 {
		t.Fatalf("revision %d", rev)
	}
	p2, _ := Open(p.Root)
	if !p2.HasManifest || p2.Revision() != 1 || len(p2.Lock.Packages) != 2 {
		t.Fatalf("reopened: %+v", p2.Lock)
	}
	sk, _ := p2.Lock.Find(KindSkill, "go-testing")
	if len(sk.Files) != 2 || sk.Version != "1.0.0" {
		t.Fatalf("skill lock: %+v", sk)
	}
	if b, _ := os.ReadFile(p.Path(".gitignore")); !strings.Contains(string(b), ".pending/") {
		t.Fatal("no .pai/.gitignore")
	}
}

func TestLibraryUpdateIsPreviewedNotApplied(t *testing.T) {
	p, lib := fixture(t)
	s := p.Begin()
	s.Enable(KindRule, "no-push", "")
	apply(t, s)
	// The library changes after locking.
	write(t, filepath.Join(lib, "rules", "no-push.md"), "---\ndescription: changed\nversion: \"2\"\n---\nPush freely.\n")
	p, _ = Open(p.Root)
	r := p.Resolve()
	en, _ := r.Find(KindRule, "no-push")
	if en.Status != StatusUpdate || !strings.Contains(en.Text(), "without asking") {
		t.Fatalf("locked content not kept: %s %q", en.Status, en.Text())
	}
	// Pinning to the locked version keeps it; accepting the update moves the lock.
	s = p.Begin()
	s.Pin(KindRule, "no-push", "1")
	apply(t, s)
	s = p.Begin()
	s.AcceptUpdate(KindRule, "no-push")
	if _, err := s.Apply(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update past a pin was not refused: %v", err)
	}
	s = p.Begin()
	s.Pin(KindRule, "no-push", "")
	s.AcceptUpdate(KindRule, "no-push")
	pv, _ := s.Preview()
	if !slices.ContainsFunc(pv.Diffs, func(d FileDiff) bool { return strings.Contains(d.Diff, "~ rule no-push 1") }) {
		t.Fatalf("lock diff does not show the update: %+v", pv.Diffs)
	}
	apply(t, s)
	en, _ = p.Resolve().Find(KindRule, "no-push")
	if !strings.Contains(en.Text(), "Push freely") {
		t.Fatal("accepted update not in effect")
	}
}

func TestInvalidConfigurationWritesNothing(t *testing.T) {
	p, _ := fixture(t)
	s := p.Begin()
	s.Enable(KindSkill, "go-testing", "")
	apply(t, s)
	before, _ := os.ReadFile(p.Path(ManifestFile))
	beforeLock, _ := os.ReadFile(p.Path(LockFile))
	s = p.Begin()
	s.Enable(KindSkill, "does-not-exist", "")
	s.SetActivation(KindSkill, "go-testing", "sometimes")
	if err := s.SetFile(filepath.Join(p.Root, Dir, "rules", "new.md"), []byte("new rule")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Apply()
	var inv *InvalidError
	if !errors.As(err, &inv) || len(inv.Problems) == 0 {
		t.Fatalf("invalid apply: %v", err)
	}
	after, _ := os.ReadFile(p.Path(ManifestFile))
	afterLock, _ := os.ReadFile(p.Path(LockFile))
	if string(before) != string(after) || string(beforeLock) != string(afterLock) {
		t.Fatal("invalid apply changed files")
	}
	if _, err := os.Stat(p.Path("rules", "new.md")); err == nil {
		t.Fatal("invalid apply wrote a staged file")
	}
}

func TestConcurrentStagesConflict(t *testing.T) {
	p, _ := fixture(t)
	a, b := p.Begin(), p.Begin()
	a.Enable(KindSkill, "go-testing", "")
	b.Enable(KindRule, "no-push", "")
	apply(t, a)
	if _, err := b.Apply(); !errors.Is(err, ErrConflict) {
		t.Fatalf("second apply over a newer revision: %v", err)
	}
}

func TestCrashBeforeCommitDiscards(t *testing.T) {
	p, _ := fixture(t)
	apply(t, func() *Stage { s := p.Begin(); s.Enable(KindSkill, "go-testing", ""); return s }())
	good, _ := os.ReadFile(p.Path(ManifestFile))
	// A crash after staging files but before the commit mark.
	write(t, p.Path(pendingDir, "1"), "version: 99\n")
	write(t, p.Path(pendingDir, planFile), `{"writes":[{"target":".pai/project.yaml","staged":"1"}]}`)
	p2, err := Open(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p2.Path(ManifestFile))
	if string(got) != string(good) {
		t.Fatal("uncommitted apply leaked into the manifest")
	}
	if _, err := os.Stat(p.Path(pendingDir)); err == nil {
		t.Fatal("pending not cleaned")
	}
}

func TestCrashAfterCommitFinishes(t *testing.T) {
	p, _ := fixture(t)
	apply(t, func() *Stage { s := p.Begin(); s.Enable(KindSkill, "go-testing", ""); return s }())
	m := p.Manifest
	m.Executor.Default = "codex"
	mb, _ := m.Marshal()
	lock := p.Lock
	lock.Revision = 2
	lb, _ := marshalLock(lock)
	write(t, p.Path(pendingDir, "1"), string(mb))
	write(t, p.Path(pendingDir, "2"), string(lb))
	write(t, p.Path(pendingDir, planFile), `{"writes":[{"target":".pai/project.yaml","staged":"1"},{"target":".pai/lock.json","staged":"2"}]}`)
	write(t, p.Path(pendingDir, commitMark), "1\n")
	// Half the renames happened before the crash.
	if err := os.Rename(p.Path(pendingDir, "1"), p.Path(ManifestFile)); err != nil {
		t.Fatal(err)
	}
	p2, err := Open(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Manifest.Executor.Default != "codex" || p2.Revision() != 2 {
		t.Fatalf("committed apply not finished: %+v rev %d", p2.Manifest.Executor, p2.Revision())
	}
}

func TestProfilesAndDisable(t *testing.T) {
	p, _ := fixture(t)
	s := p.Begin()
	s.ApplyProfile("go-service")
	apply(t, s)
	r := p.Resolve()
	if len(r.Enabled) != 2 || r.Enabled[0].Via != "profile:go-service" {
		t.Fatalf("profile not expanded: %+v", r.Enabled)
	}
	s = p.Begin()
	s.Disable(KindRule, "no-push")
	s.SetActivation(KindSkill, "go-testing", Always)
	apply(t, s)
	r = p.Resolve()
	if _, ok := r.Find(KindRule, "no-push"); ok {
		t.Fatal("disabling a profile's rule did not stick")
	}
	sk, _ := r.Find(KindSkill, "go-testing")
	if sk.ActivationOf() != Always || sk.Via != "manifest" {
		t.Fatalf("activation override: %+v", sk)
	}
}

func TestOverrideLeavesPackageUntouched(t *testing.T) {
	p, lib := fixture(t)
	apply(t, func() *Stage { s := p.Begin(); s.Enable(KindRule, "no-push", ""); return s }())
	src := filepath.Join(lib, "rules", "no-push.md")
	orig, _ := os.ReadFile(src)
	s := p.Begin()
	if err := s.Override(KindRule, "no-push", []byte("---\nkey: push\nvalue: ask-first\n---\nNever push; open a PR draft instead.\n")); err != nil {
		t.Fatal(err)
	}
	// A global or library file is not the project's to write.
	if err := s.SetFile(src, []byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("library file write: %v", err)
	}
	pv, _ := s.Preview()
	if !slices.ContainsFunc(pv.Diffs, func(d FileDiff) bool { return d.Path == ".pai/overrides/rules/no-push.md" }) {
		t.Fatalf("override not previewed: %+v", pv.Diffs)
	}
	apply(t, s)
	now, _ := os.ReadFile(src)
	if string(now) != string(orig) {
		t.Fatal("package source edited")
	}
	ins := p.Inventory(p.Resolve())
	sel := Apply(ins, "pai", nil, nil)
	var texts []string
	for _, a := range sel.Applied {
		if a.Include {
			texts = append(texts, a.Text)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "PR draft") {
		t.Fatalf("override not in effect: %q", texts)
	}
}

func TestInstructionSelection(t *testing.T) {
	p, _ := fixture(t)
	write(t, filepath.Join(p.Root, "AGENTS.md"), "# Agents\nRun go vet and go test before every commit.\n")
	write(t, filepath.Join(p.Root, "CLAUDE.md"), "Run go vet and go test before every commit.\n")
	write(t, p.Path("rules", "push-freely.md"), "---\nkey: push\nvalue: allowed\n---\nPushing is allowed.\n")
	apply(t, func() *Stage {
		s := p.Begin()
		s.Enable(KindRule, "no-push", "")
		s.Enable(KindRule, "web-style", "")
		return s
	}())
	ins := p.Inventory(p.Resolve())
	sel := Apply(ins, "codex", []string{"pkg/a.go"}, nil)
	get := func(id string) Applied {
		i := slices.IndexFunc(sel.Applied, func(a Applied) bool { return a.ID == id })
		if i < 0 {
			t.Fatalf("%s missing from %+v", id, sel.Applied)
		}
		return sel.Applied[i]
	}
	if a := get("project:AGENTS.md"); !a.Native || a.Include {
		t.Fatalf("codex's own AGENTS.md was injected: %+v", a)
	}
	if a := get("project:CLAUDE.md"); !a.Include {
		t.Fatalf("CLAUDE.md not given to codex: %+v", a)
	}
	if a := get("package:user/web-style"); a.Include {
		t.Fatal("path-scoped rule applied outside its scope")
	}
	if a := Apply(ins, "codex", nil, nil); !a.Applied[slices.IndexFunc(a.Applied, func(x Applied) bool { return x.Name == "web-style" })].Include {
		t.Fatal("path-scoped rule dropped when task paths are unknown")
	}
	// Project rule outranks the package rule on the declared key.
	if len(sel.Conflicts) != 1 || !sel.Conflicts[0].Resolved || sel.Conflicts[0].Winner != "project:push-freely" {
		t.Fatalf("conflict: %+v", sel.Conflicts)
	}
	if len(sel.Duplicates) != 1 {
		t.Fatalf("duplicates: %+v", sel.Duplicates)
	}
}

func TestSameRankConflictIsUnresolved(t *testing.T) {
	p, _ := fixture(t)
	write(t, p.Path("rules", "a.md"), "---\nkey: style\nvalue: tabs\n---\nTabs.\n")
	write(t, p.Path("rules", "b.md"), "---\nkey: style\nvalue: spaces\n---\nSpaces.\n")
	sel := Apply(p.Inventory(p.Resolve()), "pai", nil, nil)
	if len(sel.Unresolved()) != 1 {
		t.Fatalf("same-precedence conflict not reported: %+v", sel.Conflicts)
	}
}

func TestStrictManifest(t *testing.T) {
	if _, err := ParseManifest([]byte("version: 1\nskils: []\n")); err == nil {
		t.Fatal("typo key accepted")
	}
	if m, err := ParseManifest(nil); err != nil || m.Version != ManifestVersion {
		t.Fatalf("empty manifest: %v", err)
	}
	if _, err := ParseManifest([]byte("version: 9\n")); err == nil {
		t.Fatal("future version accepted")
	}
}

func TestGlobs(t *testing.T) {
	for _, c := range []struct {
		g, p string
		ok   bool
	}{
		{"web/**", "web/src/a.ts", true},
		{"web/**", "pkg/a.go", false},
		{"**/*.go", "pkg/x/a.go", true},
		{"*.md", "docs/a.md", false},
		{"pkg/*/a.go", "pkg/x/a.go", true},
	} {
		if MatchGlob(c.g, c.p) != c.ok {
			t.Errorf("%s ~ %s != %v", c.g, c.p, c.ok)
		}
	}
	if checkGlob("../x") == nil || checkGlob("/abs") == nil {
		t.Fatal("unsafe glob accepted")
	}
}

func TestSkillFormat(t *testing.T) {
	p, lib := fixture(t)
	write(t, filepath.Join(lib, "skills", "Bad Name", "SKILL.md"), "---\nname: Bad Name\n---\nbody\n")
	_, ps := p.Installed()
	if !Blocking(ps) {
		t.Fatal("invalid skill accepted")
	}
	pk, _ := Scan(Library{Name: UserLibrary, Path: lib}, p.Root)
	i := slices.IndexFunc(pk, func(x Package) bool { return x.Name == "go-testing" })
	if i < 0 || pk[i].Tokens == 0 || len(pk[i].Resources()) != 1 || !slices.Contains(pk[i].Meta.TagsOf(), "testing") {
		t.Fatalf("skill parse: %+v", pk)
	}
}

func TestDiff(t *testing.T) {
	d := Diff("a\nb\nc\n", "a\nB\nc\n")
	if !strings.Contains(d, "-b") || !strings.Contains(d, "+B") {
		t.Fatalf("diff: %q", d)
	}
	if Diff("same", "same") != "" {
		t.Fatal("diff of equal text")
	}
}
