package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := Open(t.TempDir(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}

var ctx = context.Background()

func TestModelConclusionsAreCandidates(t *testing.T) {
	s, _ := open(t)
	it, err := s.Add(ctx, Item{Kind: Fact, Status: Verified, Text: "The parser uses a hand-written lexer", Origin: "executor:t-1"})
	if err != nil || it.Status != Candidate || it.Verification != nil {
		t.Fatalf("model conclusion stored as %s", it.Status)
	}
	p, _ := s.Add(ctx, Item{Kind: Preference, Text: "Use tabs in Go files", Origin: "person"})
	if p.Status != Verified {
		t.Fatalf("explicit preference: %s", p.Status)
	}
	h, _ := s.Add(ctx, Item{Kind: Hypothesis, Text: "flaky test from timezone", Origin: "person"})
	if h.Status != Candidate {
		t.Fatal("hypothesis verified on entry")
	}
	if _, err := s.Verify(ctx, it.ID, Verification{Method: "person", By: "model:claude"}); err == nil {
		t.Fatal("a model verified knowledge")
	}
	if _, err := s.Verify(ctx, it.ID, Verification{Method: "command", By: "check"}); err == nil {
		t.Fatal("check verified without evidence")
	}
	v, err := s.Verify(ctx, it.ID, Verification{Method: "command", By: "check", Evidence: "grep lexer.go: found"})
	if err != nil || v.Status != Verified {
		t.Fatalf("verify: %v %s", err, v.Status)
	}
}

func TestRepetitionDoesNotVerify(t *testing.T) {
	s, _ := open(t)
	for range 5 {
		s.Add(ctx, Item{Kind: Fact, Text: "deploys run on Fridays", Origin: "executor:t"})
	}
	hits, _ := s.Search(ctx, "deploys Fridays", Filter{})
	if len(hits) != 5 {
		t.Fatalf("hits %d", len(hits))
	}
	for _, h := range hits {
		if h.Status != Candidate {
			t.Fatal("repetition promoted an item")
		}
	}
}

func TestStaleWhenDependencyChanges(t *testing.T) {
	s, root := open(t)
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module a\ngo 1.26\n"), 0o644)
	dep, err := s.FileDep("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	it, _ := s.Add(ctx, Item{Kind: Fact, Text: "The project targets Go 1.26", Origin: "person", Deps: []Dep{dep}})
	s.Verify(ctx, it.ID, Verification{Method: "person", By: "person"})
	got, _ := s.Get(ctx, it.ID)
	if got.Effective() != Verified {
		t.Fatalf("fresh item: %s", got.Effective())
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module a\ngo 1.27\n"), 0o644)
	got, _ = s.Get(ctx, it.ID)
	if got.Effective() != Stale || got.StaleFiles[0] != "go.mod" {
		t.Fatalf("changed dependency: %s %v", got.Effective(), got.StaleFiles)
	}
	hits, _ := s.Search(ctx, "Go", Filter{Statuses: []string{Stale}})
	if len(hits) != 1 || !strings.Contains(Render(hits), "stale: changed go.mod") {
		t.Fatalf("stale filter/render: %+v", hits)
	}
}

func TestSupersedeAndInvalidateKeepHistory(t *testing.T) {
	s, _ := open(t)
	a, _ := s.Add(ctx, Item{Kind: Decision, Text: "Use SQLite", Origin: "person", Key: "db", Value: "sqlite"})
	b, err := s.Supersede(ctx, a.ID, Item{Text: "Use Postgres", Origin: "person", Value: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.Get(ctx, a.ID)
	if old.Status != Superseded || old.SupersededBy != b.ID || b.Supersedes != a.ID {
		t.Fatalf("lineage: %+v %+v", old, b)
	}
	if _, err := s.Invalidate(ctx, b.ID, "person", ""); err == nil {
		t.Fatal("invalidated without a reason")
	}
	s.Invalidate(ctx, b.ID, "person", "project dropped the database")
	h, _ := s.History(ctx, b.ID)
	if len(h) != 2 {
		t.Fatalf("history: %v", h)
	}
	all, _ := s.List(ctx, Filter{Statuses: AllStatuses})
	if len(all) != 2 {
		t.Fatal("something was deleted")
	}
}

func TestConflictsAreReportedNotResolved(t *testing.T) {
	s, _ := open(t)
	s.Add(ctx, Item{Kind: Preference, Text: "Indent with tabs", Origin: "person", Key: "indent", Value: "tabs"})
	s.Add(ctx, Item{Kind: Fact, Text: "Code uses spaces", Origin: "executor:t", Key: "indent", Value: "spaces"})
	cs, err := s.Conflicts(ctx)
	if err != nil || len(cs) != 1 || len(cs[0].IDs) != 2 {
		t.Fatalf("conflicts: %+v %v", cs, err)
	}
}

func TestPathScopeFilter(t *testing.T) {
	s, _ := open(t)
	s.Add(ctx, Item{Kind: Fact, Text: "web uses vite", Origin: "person", Paths: []string{"web/"}})
	s.Add(ctx, Item{Kind: Fact, Text: "server uses net/http", Origin: "person"})
	hits, _ := s.Search(ctx, "uses", Filter{Paths: []string{"pkg/server/a.go"}})
	if len(hits) != 1 || !strings.Contains(hits[0].Text, "server") {
		t.Fatalf("path scope: %+v", hits)
	}
}

func TestQueryIsNotFTSSyntax(t *testing.T) {
	s, _ := open(t)
	s.Add(ctx, Item{Kind: Fact, Text: "NEAR the edge", Origin: "person"})
	if _, err := s.Search(ctx, `"unbalanced AND (NEAR`, Filter{}); err != nil {
		t.Fatalf("hostile query: %v", err)
	}
}

func TestClassicImportIsPreviewedCandidates(t *testing.T) {
	s, _ := open(t)
	classic := []ClassicItem{
		{ID: "c1", Kind: "preference", Text: "Answer in short sentences", Source: "extract", Session: "s1", Created: time.Now()},
		{ID: "c2", Kind: "fact", Text: "Old fact", Invalid: time.Now(), Reason: "outdated"},
		{ID: "c3", Kind: "fact", Text: "answer in SHORT sentences"},
	}
	plan, err := s.PreviewImport(ctx, classic)
	if err != nil || len(plan.Add) != 1 || len(plan.Skip) != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if all, _ := s.List(ctx, Filter{}); len(all) != 0 {
		t.Fatal("preview wrote")
	}
	n, _ := s.Import(ctx, plan)
	all, _ := s.List(ctx, Filter{})
	if n != 1 || all[0].Status != Candidate || all[0].Origin != "classic:c1" || all[0].Sources[0].Ref != "c1" {
		t.Fatalf("imported: %+v", all)
	}
	again, _ := s.PreviewImport(ctx, classic)
	if len(again.Add) != 0 {
		t.Fatal("re-import not detected")
	}
}

func TestModelCannotRetireVerifiedKnowledge(t *testing.T) {
	s, _ := open(t)
	d, _ := s.Add(ctx, Item{Kind: Decision, Text: "Ship on Tuesdays", Origin: "person"})
	if _, err := s.Supersede(ctx, d.ID, Item{Text: "Ship daily", Origin: "executor:t1"}); err == nil {
		t.Fatal("a model replaced a person's decision")
	}
	if _, err := s.Invalidate(ctx, d.ID, "executor:t1", "seems old"); err == nil {
		t.Fatal("a model invalidated a person's decision")
	}
	if _, err := s.Verify(ctx, d.ID, Verification{Method: "person", By: "Model:x"}); err == nil {
		t.Fatal("mixed-case model verifier accepted")
	}
	got, _ := s.Get(ctx, d.ID)
	if got.Status != Verified {
		t.Fatalf("decision now %s", got.Status)
	}
	// A candidate may be replaced by a model's newer candidate; scope carries over.
	c, _ := s.Add(ctx, Item{Kind: Fact, Text: "uses make", Origin: "executor:t1", Scope: "t1", Paths: []string{"build/"}})
	n, err := s.Supersede(ctx, c.ID, Item{Text: "uses task", Origin: "executor:t2"})
	if err != nil || n.Scope != "t1" || len(n.Paths) != 1 || n.Status != Candidate {
		t.Fatalf("candidate supersede: %+v %v", n, err)
	}
}

func TestEditedImportPlanCannotVerify(t *testing.T) {
	s, _ := open(t)
	plan := ImportPlan{Add: []Item{{Kind: Preference, Text: "sneaky", Origin: "person"}}}
	if _, err := s.Import(ctx, plan); err == nil {
		t.Fatal("non-classic import item accepted")
	}
}

func TestFiltersApplyBeforeLimit(t *testing.T) {
	s, _ := open(t)
	for range 30 {
		s.Add(ctx, Item{Kind: Fact, Text: "lexer note candidate", Origin: "executor:t"})
	}
	v, _ := s.Add(ctx, Item{Kind: Preference, Text: "lexer note preference", Origin: "person"})
	hits, _ := s.Search(ctx, "lexer note", Filter{Statuses: []string{Verified}, Limit: 1})
	if len(hits) != 1 || hits[0].ID != v.ID {
		t.Fatalf("verified hit lost behind candidates: %+v", hits)
	}
	s.Invalidate(ctx, v.ID, "person", "changed mind")
	hits, _ = s.Search(ctx, "preference", Filter{})
	if len(hits) != 0 {
		t.Fatal("invalid item retrieved by default")
	}
}

func TestDepsCannotLeaveTheRoot(t *testing.T) {
	s, _ := open(t)
	if _, err := s.FileDep("../outside"); err == nil {
		t.Fatal("dependency outside the root accepted")
	}
	if _, err := s.Add(ctx, Item{Kind: Fact, Text: "x", Origin: "person", Deps: []Dep{{Path: "../../etc/passwd", Hash: "x"}}}); err == nil {
		t.Fatal("item with an escaping dependency accepted")
	}
}

func TestStaleIsNotVerified(t *testing.T) {
	s, root := open(t)
	os.WriteFile(filepath.Join(root, "a"), []byte("1"), 0o644)
	dep, _ := s.FileDep("a")
	it, _ := s.Add(ctx, Item{Kind: Preference, Text: "a is one", Origin: "person", Deps: []Dep{dep}})
	os.WriteFile(filepath.Join(root, "a"), []byte("2"), 0o644)
	if hits, _ := s.Search(ctx, "", Filter{Statuses: []string{Verified}}); len(hits) != 0 {
		t.Fatal("stale item passed a verified-only filter")
	}
	if _, err := s.Verify(ctx, it.ID, Verification{Method: "person", By: "person"}); err != nil {
		t.Fatalf("re-verify a stale item: %v", err)
	}
	if _, err := s.Verify(ctx, it.ID, Verification{Method: "person", By: "person"}); err == nil {
		t.Fatal("re-verified a fresh verified item")
	}
}

func TestPathScopeBoundary(t *testing.T) {
	if inScope([]string{"src/foo"}, []string{"src/foobar/a.go"}) || !inScope([]string{"src/foo/"}, []string{"src/foo/a.go"}) || inScope([]string{""}, []string{"x"}) {
		t.Fatal("prefix boundary")
	}
}
