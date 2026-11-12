package memory

import (
	"context"
	"errors"
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
	if len(b.Conflicts) != 1 || b.Conflicts[0] != a.ID || !strings.Contains(Line(b), "disputed by "+a.ID) {
		t.Fatalf("missing conflict: %s", Line(b))
	}
	// A dispute is a note, never a lock: pinning still works.
	if _, err := s.Pin(ctx, a.ID, true); err != nil {
		t.Fatalf("a disputed item could not be pinned: %v", err)
	}
	if _, err := s.Pin(ctx, a.ID, false); err != nil {
		t.Fatal(err)
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
	// Consolidation may correct a fact, as a new version that Restore undoes.
	m, err := s.Merge(ctx, []string{it.ID}, Item{Text: "The editor preference is VS Code", Source: "consolidation"})
	if err != nil || len(m.Merged) != 1 || m.Merged[0] != it.ID {
		t.Fatalf("consolidation update: %+v %v", m, err)
	}
	if old, _ := s.Get(ctx, it.ID); old.Invalid.IsZero() || old.Text != "The editor preference is Helix" {
		t.Fatal("old version lost")
	}
}

// An item can never conflict with itself, and a peer that is no longer valid
// does not dispute anything. Opening a store closes the self-conflicts and
// v0.4.x consolidation flags it holds, and moves capitalised scopes to lower
// case, all as added rows.
func TestRepairOnOpen(t *testing.T) {
	dir, ctx := t.TempDir(), context.Background()
	s, err := Open(dir, Hash{})
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := s.Remember(ctx, Item{Text: "The worker abi-3957 is the user's Windows PC", Source: "test"})
	b, _, _ := s.Remember(ctx, Item{Text: "The PAI server runs on jetson1", Source: "test"})
	c, _, _ := s.Remember(ctx, Item{Text: "The PAI server runs on jetson2", Source: "test"})
	// what v0.4.2 wrote: a self-pair, and a proposal flagged as a conflict
	for _, row := range [][3]string{{a.ID, a.ID, "Automatic consolidation proposed changing these sources. ASK the user; no source has been replaced."}, {b.ID, c.ID, "Automatic consolidation proposed changing these sources. ASK the user; no source has been replaced."}} {
		if _, err := s.db.Exec(`INSERT INTO memory_conflicts(a,b,reason) VALUES(?,?,?)`, row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}
	// and a scope written before scopes were normalized
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO items (id,kind,text,scope,source,created,updated,vec,embedder) VALUES ('mOLD','fact','The PAI repo is on GitHub','project:PAI','test',?,?,NULL,'')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO items (id,kind,text,scope,source,created,updated,vec,embedder) VALUES ('mDUP','fact','The PAI server runs on jetson1','project:PAI','test',?,?,NULL,'')`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO items (id,kind,text,scope,source,created,updated,vec,embedder) VALUES ('mLOW','fact','the pai server runs on jetson1.','project:pai','test',?,?,NULL,'')`, now, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, a.ID); len(got.Conflicts) != 0 {
		t.Fatalf("an item reads as conflicting with itself: %v", got.Conflicts)
	}
	_ = s.Close()
	for range 2 { // opening twice changes nothing more
		if s, err = Open(dir, Hash{}); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(ctx, b.ID); len(got.Conflicts) != 0 {
			t.Fatalf("legacy consolidation flag still open: %v", got.Conflicts)
		}
		old, _ := s.Get(ctx, "mOLD")
		if old.Invalid.IsZero() || !strings.HasPrefix(old.Reason, MergedInto) {
			t.Fatalf("capitalised scope not moved: %+v", old)
		}
		moved, err := s.Get(ctx, strings.TrimPrefix(old.Reason, MergedInto))
		if err != nil || moved.Scope != "project:pai" || moved.Text != old.Text || moved.Merged[0] != "mOLD" {
			t.Fatalf("moved: %+v %v", moved, err)
		}
		if dup, _ := s.Get(ctx, "mDUP"); dup.Reason != MergedInto+"mLOW" {
			t.Fatalf("a repeat in the lower case scope should merge into it: %q", dup.Reason)
		}
		valid, _ := s.List(ctx, Filter{Scope: "project:pai"})
		if len(valid) != 2 {
			t.Fatalf("want 2 valid project items, got %d", len(valid))
		}
		_ = s.Close()
	}
	// A conflict whose peer was retired no longer shows.
	s, _ = Open(dir, Hash{})
	defer s.Close()
	if err := s.FlagConflict(ctx, []string{b.ID, c.ID}, "different hosts"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, b.ID); len(got.Conflicts) != 1 {
		t.Fatal("fresh conflict missing")
	}
	if _, err := s.Invalidate(ctx, c.ID, "the server moved back to jetson1"); err != nil {
		t.Fatalf("a disputed item could not be forgotten: %v", err)
	}
	if got, _ := s.Get(ctx, b.ID); len(got.Conflicts) != 0 {
		t.Fatal("a retired peer still disputes")
	}
}

func TestOneFactPerItem(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, _, err := s.Remember(ctx, Item{Text: strings.Repeat("The worker runs Windows. ", 40)}); !errors.Is(err, ErrNotUseful) {
		t.Fatalf("a %d character item was stored: %v", 25*40, err)
	}
	a, _, err := s.Remember(ctx, Item{Text: "The Staging   database listens on port 5433."})
	if err != nil {
		t.Fatal(err)
	}
	if b, fresh, _ := s.Remember(ctx, Item{Text: "the staging database listens on port 5433"}); fresh || b.ID != a.ID {
		t.Fatal("same words in another case or spacing stored twice")
	}
	if _, fresh, _ := s.Remember(ctx, Item{Text: "The staging database listens on port 5434"}); !fresh {
		t.Fatal("a different number was taken for a repeat")
	}
}
