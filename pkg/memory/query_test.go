package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFindFiltersBeforeCutting(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.Hybrid = true
	// many better matches in another scope must not crowd the scope out
	for i := range 12 {
		if _, _, err := s.Remember(ctx, Item{Text: fmt.Sprintf("deploy deploy deploy step number %d of the rollout", i), Scope: "project:other", Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	mine, _, err := s.Remember(ctx, Item{Text: "the deploy for this project uses a blue green switch", Scope: "project:pai", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	everywhere, _, err := s.Remember(ctx, Item{Text: "every deploy waits for the smoke tests to pass first", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.Find(ctx, Query{Text: "deploy", Scope: "project:pai", K: 2})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.Item.ID)
	}
	if len(hits) != 2 || !strings.Contains(strings.Join(got, " "), mine.ID) || !strings.Contains(strings.Join(got, " "), everywhere.ID) {
		t.Errorf("want the scope's item and the global one, got %v", hits)
	}
}

func TestFindAndListCanLeaveOutSummaries(t *testing.T) {
	s, ctx := open(t), context.Background()
	sum, _, err := s.Remember(ctx, Item{Kind: Summary, Text: "the session worked on the deploy pipeline", Scope: "session:abc", Source: "compaction"})
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.Search(ctx, "deploy pipeline", "abc", 5)
	if err != nil || len(all) != 1 || all[0].Item.ID != sum.ID {
		t.Fatalf("search keeps summaries: %v %v", all, err)
	}
	if hits, err := s.Find(ctx, Query{Text: "deploy pipeline", K: 5, NoSummaries: true}); err != nil || len(hits) != 0 {
		t.Errorf("find: %v %v", hits, err)
	}
	if items, err := s.List(ctx, Filter{NoSummaries: true}); err != nil || len(items) != 0 {
		t.Errorf("list: %v %v", items, err)
	}
}

func TestPinnedFiltersAndCaps(t *testing.T) {
	s, ctx := open(t), context.Background()
	add := func(text, scope string) Item {
		it, _, err := s.Remember(ctx, Item{Text: text, Scope: scope, Source: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if it, err = s.Pin(ctx, it.ID, true); err != nil {
			t.Fatal(err)
		}
		return it
	}
	global, mine := add("a pinned item for every project", Global), add("a pinned item of this project", "project:pai")
	other := add("a pinned item of another project", "project:other")
	items, err := s.Pinned(ctx, Query{Scope: "project:pai"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	if len(items) != 2 || !ids[global.ID] || !ids[mine.ID] || ids[other.ID] {
		t.Errorf("got %v", items)
	}
	for i := range maxPinned + 3 {
		add(fmt.Sprintf("yet another pinned item, number %d", i), Global)
	}
	if items, err := s.Pinned(ctx, Query{}); err != nil || len(items) != maxPinned {
		t.Errorf("%d pinned, want %d: %v", len(items), maxPinned, err)
	}
}

func TestEditKeepsWhoEdited(t *testing.T) {
	s, ctx := open(t), context.Background()
	it, _, err := s.Remember(ctx, Item{Text: "the first version of this statement", Source: "model"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Edit(ctx, it.ID, "the second version of this statement", "", "", "", "person")
	if err != nil || e.Source != "person" {
		t.Errorf("%+v %v", e, err)
	}
}

func TestLine(t *testing.T) {
	it := Item{ID: "m1", Kind: Fact, Scope: Global, Text: "words", Source: "model"}
	if got := Line(it); got != "- [m1] (fact, global) \"words\" [source=model]\n" {
		t.Errorf("%q", got)
	}
	long := it
	long.Text = strings.Repeat("word ", 200)
	long.Conflicts = []string{"m2"}
	got := ShortLine(long)
	if len(got) > shownText+200 || !strings.Contains(got, "more characters: recall id m1") || !strings.Contains(got, "disputed by m2") || strings.Contains(got, "ASK") {
		t.Errorf("short line: %q", got)
	}
	if got := Line(long); !strings.Contains(got, long.Text[:len(long.Text)-1]) {
		t.Error("Line must keep the whole text")
	}
	it.Source = SourceMCP
	if got := Line(it); !strings.Contains(got, "[unverified, from an MCP client] \"words\"") {
		t.Errorf("%q", got)
	}
}

func TestRecallK(t *testing.T) {
	for _, c := range [][2]int{{0, defaultRecall}, {-1, defaultRecall}, {3, 3}, {maxRecall + 1, maxRecall}} {
		if got := RecallK(c[0]); got != c[1] {
			t.Errorf("RecallK(%d) = %d, want %d", c[0], got, c[1])
		}
	}
}
