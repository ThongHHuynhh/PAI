package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStructuredConflictRequiresClarification(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, _, err := s.Remember(ctx, Item{Text: "Staging database listens on port 5432", Contract: Contract{Subject: "staging database", Property: "port", Value: "5432"}, Source: "person"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.Remember(ctx, Item{Text: "Staging database listens on port 5433", Contract: Contract{Subject: "staging database", Property: "port", Value: "5433"}, Source: "extract"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Conflicts) != 1 || b.Conflicts[0] != a.ID || !strings.Contains(Line(b), "ASK user") {
		t.Fatalf("missing conflict: %s", Line(b))
	}
	for _, id := range []string{a.ID, b.ID} {
		if _, err := s.Invalidate(ctx, id, "prefer other value"); err == nil {
			t.Fatal("invalidation chose a winner")
		}
		if _, err := s.Edit(ctx, id, "Staging database listens on port 9999", "", "", "", "person"); err == nil {
			t.Fatal("edit chose a winner")
		}
	}
	if err := s.ResolveConflict(ctx, a.ID, b.ID, Resolution{}); err == nil {
		t.Fatal("empty clarification accepted")
	}
	s.VerifyResolution = func(_ context.Context, a, b string, p Resolution) error {
		if p.Call != "trusted-ask" || p.Answer != "5433" {
			return fmt.Errorf("not a verified reply")
		}
		return nil
	}
	if err := s.ResolveConflict(ctx, a.ID, b.ID, Resolution{Session: "s", Call: "trusted-ask", Answer: "5433"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Invalidate(ctx, a.ID, "User confirms port moved to 5433"); err != nil {
		t.Fatal(err)
	}
	all, err := s.List(ctx, Filter{Invalid: true})
	if err != nil || len(all) != 2 {
		t.Fatalf("lost sources: %d %v", len(all), err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE id=?`, a.ID).Scan(&n); err != nil || n < 2 {
		t.Fatalf("missing audit %d %v", n, err)
	}
}

func TestNoUtilityFromMentionOrRepeat(t *testing.T) {
	s, ctx := open(t), context.Background()
	it, _, err := s.Remember(ctx, Item{Text: "The staging database listens on port 5433"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Recall(ctx, Turn{Session: "s", Query: it.Text})
	if err != nil || len(r.Hits) == 0 {
		t.Fatalf("recall %v", err)
	}
	if err := s.Settle(ctx, "s", it.Text, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Remember(ctx, Item{Text: it.Text, Source: "another source"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, it.ID)
	if err != nil || got.Used != 0 || got.Utility != it.Utility {
		t.Fatalf("mention rewarded: %v", err)
	}
	if err := s.RetrievalFeedback(ctx, "wrong-session", r.ID, it.ID, "used", "changed action"); err == nil {
		t.Fatal("cross-session feedback")
	}
	if err := s.RetrievalFeedback(ctx, "s", r.ID, it.ID, "harmful", "This led to an incorrect connection attempt"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, it.ID)
	if got.Utility >= it.Utility || !got.Invalid.IsZero() {
		t.Fatal("feedback must lower utility without deciding truth")
	}
	if err := s.RetrievalFeedback(ctx, "s", r.ID, it.ID, "used", "repeat"); err == nil {
		t.Fatal("duplicate feedback rewarded")
	}
}

func TestScopeAndReadiness(t *testing.T) {
	s, ctx := open(t), context.Background()
	for _, scope := range []string{Global, "project:other", "session:mine", "session:other"} {
		if _, _, err := s.Remember(ctx, Item{Text: "The staging database listens on port 5433", Scope: scope, Pinned: true}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Recall(ctx, Turn{Session: "mine", Query: "staging database"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range r.Hits {
		if h.Item.Scope != Global && h.Item.Scope != "session:mine" {
			t.Fatalf("scope leak: %s", h.Item.Scope)
		}
	}
	dep, _, err := s.Remember(ctx, Item{Text: "Deployment requires the staging database", Contract: Contract{Expires: time.Now().Add(-time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	it, _, err := s.Remember(ctx, Item{Text: "Deployment procedure uses staging database", Kind: Procedure, Contract: Contract{Dependencies: []string{dep.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, "Deployment procedure staging database", "mine", 30)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Item.ID == it.ID {
			found = true
			if !strings.Contains(HitLine(h), "dependency not ready") {
				t.Fatal("expired prerequisite looked usable")
			}
		}
	}
	if !found {
		t.Fatal("missing contextual memory")
	}
}

func TestSecretEnvelopeAndContractRoundTrip(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, _, err := s.Remember(ctx, Item{Text: "The staging service uses this credential", Contract: Contract{Uncertainty: "token = ghp_abcdefghijklmnopqrstuvwxyz0123456789"}}); err == nil {
		t.Fatal("metadata secret stored")
	}
	it, _, err := s.Remember(ctx, Item{Text: "The editor preference is Helix", Kind: Preference, Contract: Contract{Subject: "user", Property: "editor", Value: "Helix", Uncertainty: "reported by user"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, it.ID)
	if err != nil || got.Version != ContractVersion || got.Value != "Helix" || got.Observed.IsZero() {
		t.Fatalf("contract lost: %v", err)
	}
	if _, err := s.Merge(ctx, []string{it.ID}, Item{Text: "The editor preference is VS Code", Source: "consolidation"}); err == nil {
		t.Fatal("automatic consolidation changed a fact")
	}
}
