package memory

import (
	"context"
	"errors"
	"testing"
)

func TestUsefulRefusesTrivialAndSecrets(t *testing.T) {
	for _, bad := range []string{"ok", "The OpenAI key is sk-proj-abcdefghijklmnopqrstuvwxyz123", "db password: hunter2hunter2", "-----BEGIN OPENSSH PRIVATE KEY-----"} {
		if err := Useful(bad); !errors.Is(err, ErrNotUseful) {
			t.Errorf("%q: want refused, got %v", bad, err)
		}
	}
	if err := Useful("The user deploys with GitHub Actions to Fly.io"); err != nil {
		t.Fatal(err)
	}
	s := open(t)
	if _, _, err := s.Remember(context.Background(), Item{Text: "token = ghp_abcdefghijklmnopqrstuvwxyz0123456789", Source: "test"}); !errors.Is(err, ErrNotUseful) {
		t.Fatalf("a secret was stored: %v", err)
	}
}

func TestMergeKeepsLineageAndStats(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, _, _ := s.Remember(ctx, Item{Text: "Staging database is Postgres 17", Topic: "database", Source: "test"})
	b, _, _ := s.Remember(ctx, Item{Text: "Staging database listens on port 5433", Topic: "Database", Source: "test"})
	_, _ = s.Pin(ctx, b.ID, true)
	m, err := s.Merge(ctx, []string{a.ID, b.ID}, Item{Text: "Staging database is Postgres 17 on port 5433", Source: "consolidation"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Topic != "database" || !m.Pinned || len(m.Merged) != 2 {
		t.Fatalf("merged item lost topic, pin or lineage: %+v", m)
	}
	valid, _ := s.List(ctx, Filter{})
	if len(valid) != 1 || valid[0].ID != m.ID {
		t.Fatalf("want only the merged item valid, got %d", len(valid))
	}
	old, _ := s.Get(ctx, a.ID)
	if old.Invalid.IsZero() || old.Reason != "merged into "+m.ID {
		t.Fatalf("old item not retired with lineage: %+v", old)
	}
	if _, err := s.Merge(ctx, []string{a.ID}, Item{Text: "again from a retired item"}); err == nil {
		t.Fatal("merged an invalid item")
	}
	e, err := s.Edit(ctx, m.ID, "Staging database is Postgres 17 on port 5432", "", "", "")
	if err != nil || e.ID == m.ID || e.Merged[0] != m.ID || e.Topic != "database" {
		t.Fatalf("an edit should be a new version: %+v %v", e, err)
	}
}

func TestClustersGroupBySubject(t *testing.T) {
	s, ctx := open(t), context.Background()
	for _, it := range []Item{
		{Text: "Deploys go through GitHub Actions", Topic: "deploy"},
		{Text: "Production runs on the VPS named atlas", Topic: "deploy"},
		{Text: "The user's editor is Helix with vim keys", Topic: "editor"},
		{Text: "Tests run with go test ./... before every push", Topic: "testing"},
	} {
		it.Source = "test"
		if _, _, err := s.Remember(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := s.Clusters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || len(cs[0]) != 2 || cs[0][0].Topic != "deploy" {
		t.Fatalf("want one deploy cluster of 2, got %+v", cs)
	}
	rel, _ := s.Related(ctx, Item{Text: "Deploys need the FLY_API_TOKEN secret set", Topic: "deploy"})
	if len(rel) != 2 {
		t.Fatalf("want the 2 deploy items related, got %d", len(rel))
	}
}

func TestSupersedeKeepsNewestSummary(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, _, _ := s.Remember(ctx, Item{Kind: Summary, Text: "Summary one of the session about deploys", Session: "s1", Scope: "session:s1", Source: "compaction"})
	b, _, _ := s.Remember(ctx, Item{Kind: Summary, Text: "Summary two: deploys done, now on monitoring and alerts", Session: "s1", Scope: "session:s1", Source: "compaction"})
	if err := s.Supersede(ctx, "s1", b.ID); err != nil {
		t.Fatal(err)
	}
	if old, _ := s.Get(ctx, a.ID); old.Invalid.IsZero() {
		t.Fatal("older summary still valid")
	}
	if nw, _ := s.Get(ctx, b.ID); !nw.Invalid.IsZero() {
		t.Fatal("newest summary retired")
	}
}
