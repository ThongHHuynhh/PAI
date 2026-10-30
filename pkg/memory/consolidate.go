package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Two items are related, and offered to the model to merge, when they share
// a topic or their embeddings are this alike. Lower than dupCosine: related
// items say different things about one subject.
const (
	relatedCosine = 0.5
	clusterCosine = 0.6 // a consolidation pass groups more tightly than one write
	maxRelated    = 6   // existing items offered beside a new one
	maxCluster    = 8   // items one merge reads at once
)

// Related is the valid items of it's scope that share its topic or are like
// it, most alike first: what a new item is checked against before it is
// stored. Summaries are never offered: a session's summary stands alone.
func (s *Store) Related(ctx context.Context, it Item) ([]Item, error) {
	vecs, err := s.embed.Embed(ctx, []string{it.Text})
	if err != nil {
		return nil, err
	}
	items, err := s.query(ctx, `WHERE invalid=0 AND scope=? AND kind<>?`, scopeOr(it.Scope), Summary)
	if err != nil {
		return nil, err
	}
	topic := strings.ToLower(strings.TrimSpace(it.Topic))
	type scored struct {
		it Item
		c  float32
	}
	var out []scored
	for _, o := range items {
		c := cosine(vecs[0], o.vec)
		if c >= relatedCosine || topic != "" && o.Topic == topic {
			out = append(out, scored{o, c})
		}
	}
	slices.SortFunc(out, func(a, b scored) int { return cmpFloat(float64(b.c), float64(a.c)) })
	var res []Item
	for _, o := range out[:min(len(out), maxRelated)] {
		res = append(res, o.it)
	}
	return res, nil
}

func scopeOr(s string) string {
	if s == "" {
		return Global
	}
	return s
}

// Merge replaces the items ids with one, it, in a single step: it is stored
// with their history, the best of their utility and counts, pinned when any
// was, and each of them is invalidated as merged into it. Nothing is lost:
// the old items stay, invalid, and it names them.
func (s *Store) Merge(ctx context.Context, ids []string, it Item) (Item, error) {
	if err := Useful(it.Text); err != nil {
		return it, err
	}
	var olds []Item
	for _, id := range ids {
		o, err := s.Get(ctx, id)
		if err != nil {
			return it, err
		}
		if !o.Invalid.IsZero() {
			return it, fmt.Errorf("memory: %s is already invalid", id)
		}
		olds = append(olds, o)
	}
	for _, o := range olds {
		it.Merged = append(it.Merged, o.ID)
		it.Pinned = it.Pinned || o.Pinned
		it.Utility = max(it.Utility, o.Utility)
		it.Shown += o.Shown
		it.Used += o.Used
		if it.Kind == "" {
			it.Kind = o.Kind
		}
		if it.Scope == "" {
			it.Scope = o.Scope
		}
		if it.Topic == "" {
			it.Topic = o.Topic
		}
	}
	if it.Kind == "" {
		it.Kind = Fact
	}
	if !valid(it.Kind) {
		return it, fmt.Errorf("memory: kind %q is not one of %s", it.Kind, strings.Join(kinds, ", "))
	}
	it.Text, it.Topic, it.Scope = strings.TrimSpace(it.Text), strings.ToLower(strings.TrimSpace(it.Topic)), scopeOr(it.Scope)
	vecs, err := s.embed.Embed(ctx, []string{it.Text})
	if err != nil {
		return it, err
	}
	it.vec = vecs[0]
	now := time.Now().UTC()
	it.ID, it.Created, it.Updated = newID(), now, now
	s.mu.Lock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.mu.Unlock()
		return it, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO items (id,kind,text,scope,topic,merged,session,rec_from,rec_to,source,created,updated,pinned,utility,shown,used,embedder,vec) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.ID, it.Kind, it.Text, it.Scope, it.Topic, strings.Join(it.Merged, ","), it.Session, it.From, it.To, it.Source, now.UnixMilli(), now.UnixMilli(), it.Pinned, it.Utility, it.Shown, it.Used, s.embed.Name(), pack(it.vec))
	for _, o := range olds {
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE items SET invalid=?, reason=?, updated=? WHERE id=?`, now.UnixMilli(), "merged into "+it.ID, now.UnixMilli(), o.ID)
		}
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	s.mu.Unlock()
	if err != nil {
		return it, err
	}
	s.emit(Event{Type: "consolidated", Session: it.Session, Items: append([]Item{it}, olds...)})
	return it, nil
}

// Clusters is groups of valid items that are about one thing: they share a
// topic and scope, or are alike past clusterCosine. Each has two items or
// more, at most maxCluster. Summaries and pinned items are left alone: a
// person pinned an item as it is.
func (s *Store) Clusters(ctx context.Context) ([][]Item, error) {
	items, err := s.query(ctx, `WHERE invalid=0 AND kind<>? AND pinned=0 ORDER BY updated DESC`, Summary)
	if err != nil {
		return nil, err
	}
	parent := make([]int, len(items))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			if a.Scope != b.Scope {
				continue
			}
			if a.Topic != "" && a.Topic == b.Topic || cosine(a.vec, b.vec) >= clusterCosine {
				parent[find(i)] = find(j)
			}
		}
	}
	groups := map[int][]Item{}
	for i, it := range items {
		r := find(i)
		groups[r] = append(groups[r], it)
	}
	var out [][]Item
	for _, g := range groups {
		if len(g) >= 2 {
			out = append(out, g[:min(len(g), maxCluster)])
		}
	}
	slices.SortFunc(out, func(a, b []Item) int { return len(b) - len(a) })
	return out, nil
}

// Supersede invalidates the summaries of session other than keep: a newer
// compaction's summary covers what they did.
func (s *Store) Supersede(ctx context.Context, session, keep string) error {
	items, err := s.query(ctx, `WHERE invalid=0 AND kind=? AND session=? AND id<>?`, Summary, session, keep)
	if err != nil {
		return err
	}
	for _, it := range items {
		if _, err := s.Invalidate(ctx, it.ID, "superseded by summary "+keep); err != nil {
			return err
		}
	}
	return nil
}
