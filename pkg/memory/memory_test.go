package memory

import (
	"context"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRememberMergesRepeats(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, fresh, err := s.Remember(ctx, Item{Text: "The user deploys PAI on a Hetzner VPS named atlas", Source: "test"})
	if err != nil || !fresh {
		t.Fatalf("first remember: %v fresh=%v", err, fresh)
	}
	b, fresh, err := s.Remember(ctx, Item{Text: "The user deploys PAI on a Hetzner VPS named atlas.", Source: "test"})
	if err != nil || fresh || b.ID != a.ID {
		t.Fatalf("repeat should merge into %s, got %s fresh=%v err=%v", a.ID, b.ID, fresh, err)
	}
	if _, fresh, _ := s.Remember(ctx, Item{Text: "The user prefers Go over Python for services", Kind: Preference, Source: "test"}); !fresh {
		t.Fatal("a different item merged")
	}
}

func TestSearchRanksRelevantFirstAndSkipsInvalid(t *testing.T) {
	s, ctx := open(t), context.Background()
	for _, txt := range []string{
		"The staging database is Postgres 17 on db-stage.internal port 5433",
		"The user's favourite editor is Helix",
		"Deploys go through GitHub Actions and then Fly.io",
		"The production database password lives in 1Password vault Infra",
	} {
		if _, _, err := s.Remember(ctx, Item{Text: txt, Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(ctx, "which port does the staging database use?", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Item.Text[:11] != "The staging" {
		t.Fatalf("want the staging database first, got %+v", hits)
	}
	if _, err := s.Invalidate(ctx, hits[0].Item.ID, "moved to 5432"); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.Search(ctx, "which port does the staging database use?", "", 3)
	for _, h := range hits {
		if h.Item.Text[:11] == "The staging" {
			t.Fatal("an invalidated item was recalled")
		}
	}
	all, _ := s.List(ctx, Filter{Invalid: true})
	if len(all) != 4 {
		t.Fatalf("invalidated items must stay: %d", len(all))
	}
}

func TestUsedIn(t *testing.T) {
	words := map[string]bool{}
	for _, w := range Words("I connected to the staging Postgres on port 5433 as you noted") {
		words[w], words[stem(w)] = true, true
	}
	if !usedIn("The staging database is Postgres 17 on db-stage.internal port 5433", words) {
		t.Fatal("used item not seen as used")
	}
	if usedIn("The user's favourite editor is Helix", words) {
		t.Fatal("unused item seen as used")
	}
}

// The bandit should learn to skip recall on short chatter and to recall on
// questions, from rewards alone.
func TestBanditLearnsWhenToRecall(t *testing.T) {
	s, ctx := open(t), context.Background()
	chat := features(Turn{Query: "ok thanks"}, 50)
	ask := features(Turn{Query: "what did we decide about the staging database migration last week?"}, 50)
	for range 60 {
		for _, k := range Arms {
			rc, ra := -0.2, 0.05 // chatter: recall only costs. questions: recall pays, most at 6
			if k == 0 {
				rc, ra = skipReward, 0.0
			}
			if k == 6 {
				ra = 0.8
			}
			if err := s.update(ctx, k, chat, rc); err != nil {
				t.Fatal(err)
			}
			if err := s.update(ctx, k, ask, ra); err != nil {
				t.Fatal(err)
			}
		}
	}
	if k, _ := s.choose(ctx, chat); k != 0 {
		t.Fatalf("chatter: want skip (0), got %d", k)
	}
	if k, _ := s.choose(ctx, ask); k != 6 {
		t.Fatalf("question: want 6, got %d", k)
	}
}

func TestRecallSettleUpdatesUtility(t *testing.T) {
	s, ctx := open(t), context.Background()
	it, _, _ := s.Remember(ctx, Item{Text: "The release signing key is stored in the RELEASE_SIGNING_KEY secret", Source: "test"})
	_, _, _ = s.Remember(ctx, Item{Text: "The user likes dark themes", Kind: Preference, Source: "test"})
	r, err := s.Recall(ctx, Turn{Session: "s1", Query: "where is the release signing key stored?"})
	if err != nil {
		t.Fatal(err)
	}
	if r.K == 0 || len(r.Hits) == 0 {
		t.Fatalf("a cold bandit should recall: %+v", r)
	}
	if err := s.Settle(ctx, "s1", "The signing key is in the RELEASE_SIGNING_KEY secret of the release workflow.", false); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, it.ID)
	if got.Used != 1 || got.Utility <= 0.5 {
		t.Fatalf("used item: want used=1 and utility up, got %+v", got)
	}
	rs, _ := s.Retrievals(ctx, "s1", 10)
	if len(rs) != 1 || rs[0].Reward == nil || *rs[0].Reward <= 0 {
		t.Fatalf("retrieval not settled with a positive reward: %+v", rs)
	}
}
