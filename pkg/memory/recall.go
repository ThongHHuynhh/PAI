package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Search weighs: rrfK flattens rank fusion, so a first place in one list
// does not drown a good place in both; an item's learnt utility and its age
// then move it at most this far.
const (
	rrfK         = 10
	candidates   = 40   // from each list before fusion
	minCosine    = 0.12 // below this, a vector match is noise
	utilityWeigh = 0.6
	recencyWeigh = 0.1
	recencyDays  = 30.0
	sessionBoost = 0.1 // an item learnt in the same session
)

// skipReward is what a turn with no recall earns: it cost nothing and
// helped nothing, so a recall whose items get used beats it.
const skipReward = 0.1

// Hit is an item a search found, with why: its place in each list and the
// parts of its score.
type Hit struct {
	Item    Item    `json:"item"`
	Score   float64 `json:"score"`
	Lexical int     `json:"lexical,omitempty"` // place in the BM25 list, from 1; 0 for absent
	Vector  int     `json:"vector,omitempty"`  // place by cosine, from 1
	Cosine  float64 `json:"cosine"`
	Pinned  bool    `json:"pinned,omitempty"` // here because it is pinned
	Used    bool    `json:"used,omitempty"`   // once settled: the reply carried its words
}

// Search is the k valid items most relevant to query, best first. session,
// when set, favours what was learnt there.
func (s *Store) Search(ctx context.Context, query, session string, k int) ([]Hit, error) {
	if k <= 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	vecs, err := s.embed.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	items, err := s.query(ctx, `WHERE invalid=0`)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Hit{}
	for _, it := range items {
		byID[it.ID] = &Hit{Item: it, Cosine: float64(cosine(vecs[0], it.vec))}
	}
	// vector list
	vl := make([]*Hit, 0, len(byID))
	for _, h := range byID {
		if h.Cosine >= minCosine {
			vl = append(vl, h)
		}
	}
	slices.SortFunc(vl, func(a, b *Hit) int { return cmpFloat(b.Cosine, a.Cosine) })
	for i, h := range vl[:min(len(vl), candidates)] {
		h.Vector = i + 1
	}
	// lexical list
	if q := ftsQuery(query); q != "" {
		rows, err := s.db.QueryContext(ctx, `SELECT items.id FROM items_fts JOIN items ON items.rowid=items_fts.rowid WHERE items_fts MATCH ? AND items.invalid=0 ORDER BY bm25(items_fts) LIMIT ?`, q, candidates)
		if err != nil {
			return nil, fmt.Errorf("memory: search: %w", err)
		}
		i := 0
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			i++
			if h, ok := byID[id]; ok {
				h.Lexical = i
			}
		}
		_ = rows.Close()
	}
	now := time.Now()
	var hits []Hit
	for _, h := range byID {
		if h.Lexical == 0 && h.Vector == 0 {
			continue
		}
		rrf := 0.0
		for _, r := range []int{h.Lexical, h.Vector} {
			if r > 0 {
				rrf += 1 / float64(rrfK+r)
			}
		}
		base := rrf / (2 / float64(rrfK+1)) // 1 for first in both lists
		age := now.Sub(h.Item.Updated).Hours() / 24
		h.Score = base*(1-utilityWeigh/2+utilityWeigh*h.Item.Utility) + recencyWeigh*math.Exp(-age/recencyDays)
		if session != "" && h.Item.Session == session {
			h.Score += sessionBoost
		}
		hits = append(hits, *h)
	}
	slices.SortFunc(hits, func(a, b Hit) int { return cmpFloat(b.Score, a.Score) })
	return hits[:min(len(hits), k)], nil
}

// ftsQuery is query's words as an FTS5 OR of quoted terms, so no word of a
// user's text is read as FTS syntax.
func ftsQuery(query string) string {
	var terms []string
	for _, w := range Words(query) {
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, ``)+`"`)
	}
	return strings.Join(terms, " OR ")
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Retrieval is one automatic recall: what the bandit chose and what came of
// it.
type Retrieval struct {
	ID       string    `json:"id"`
	Session  string    `json:"session"`
	At       time.Time `json:"at"`
	Query    string    `json:"query"`
	K        int       `json:"k"` // the arm: how many items the bandit chose to recall, 0 to skip
	Features []float64 `json:"features"`
	Hits     []Hit     `json:"hits"`
	Reward   *float64  `json:"reward,omitempty"` // once settled
	Ms       int64     `json:"ms"`               // how long the recall took, embedding included
}

// Turn is what a session tells recall about the turn it recalls for.
type Turn struct {
	Session string
	Query   string  // the new messages' text
	Fill    float64 // context used over the limit, 0 to 1
}

// Recall is the automatic recall for a new turn: the bandit picks how many
// items, pinned items always come, and the retrieval is kept for Settle.
func (s *Store) Recall(ctx context.Context, t Turn) (Retrieval, error) {
	st, err := s.Stats(ctx)
	if err != nil {
		return Retrieval{}, err
	}
	start := time.Now()
	x := features(t, st.Items)
	r := Retrieval{ID: newID(), Session: t.Session, At: time.Now().UTC(), Query: t.Query, Features: x}
	if st.Items == 0 {
		return r, nil // nothing to recall, so nothing to choose or learn from
	}
	k, err := s.choose(ctx, x)
	if err != nil {
		return Retrieval{}, err
	}
	r.K = k
	if r.Hits, err = s.Search(ctx, t.Query, t.Session, k); err != nil {
		return r, err
	}
	pinned, err := s.query(ctx, `WHERE invalid=0 AND pinned=1 ORDER BY updated DESC LIMIT 10`)
	if err != nil {
		return r, err
	}
	for _, p := range pinned {
		if !slices.ContainsFunc(r.Hits, func(h Hit) bool { return h.Item.ID == p.ID }) {
			r.Hits = append(r.Hits, Hit{Item: p, Pinned: true})
		}
	}
	ids := make([]string, len(r.Hits))
	for i, h := range r.Hits {
		ids[i] = h.Item.ID
	}
	fb, _ := json.Marshal(x)
	ib, _ := json.Marshal(r.Hits)
	r.Ms = time.Since(start).Milliseconds()
	s.mu.Lock()
	_, err = s.db.ExecContext(ctx, `INSERT INTO retrievals (id,session,at,query,arm,features,items,ms) VALUES (?,?,?,?,?,?,?,?)`, r.ID, r.Session, r.At.UnixMilli(), r.Query, k, string(fb), string(ib), r.Ms)
	if err == nil && len(ids) > 0 {
		_, err = s.db.ExecContext(ctx, `UPDATE items SET shown=shown+1 WHERE id IN (`+placeholders(len(ids))+`)`, anys(ids)...)
	}
	s.mu.Unlock()
	if err != nil {
		return r, err
	}
	s.emit(Event{Type: "recalled", Session: t.Session, Detail: r})
	return r, nil
}

// Settle scores a session's open retrievals once the model has answered:
// reply is everything the model wrote and called since. An item counts as
// used when the reply carries its distinctive words. The reward is the share
// used, less a little per item for the context it took; a turn with no recall
// earns skipReward, unless the model had to call recall itself. Each
// item's utility follows whether it was used.
func (s *Store) Settle(ctx context.Context, session, reply string, calledRecall bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, arm, features, items FROM retrievals WHERE session=? AND settled=0`, session)
	if err != nil {
		return err
	}
	type open struct {
		id   string
		arm  int
		x    []float64
		hits []Hit
	}
	var opens []open
	for rows.Next() {
		var o open
		var fb, ib string
		if err := rows.Scan(&o.id, &o.arm, &fb, &ib); err != nil {
			_ = rows.Close()
			return err
		}
		_ = json.Unmarshal([]byte(fb), &o.x)
		_ = json.Unmarshal([]byte(ib), &o.hits)
		opens = append(opens, o)
	}
	_ = rows.Close()
	words := map[string]bool{}
	for _, w := range Words(reply) {
		words[w], words[stem(w)] = true, true
	}
	for _, o := range opens {
		used, recalled := 0, 0
		for i, h := range o.hits {
			u := usedIn(h.Item.Text, words)
			o.hits[i].Used = u
			if h.Pinned {
				continue // pinned items come whatever the bandit chose, so they are marked but not scored
			}
			recalled++
			if u {
				used++
			}
			s.mu.Lock()
			_, err := s.db.ExecContext(ctx, `UPDATE items SET used=used+?, utility=utility*0.8+? WHERE id=?`, b2i(u), 0.2*b2f(u), h.Item.ID)
			s.mu.Unlock()
			if err != nil {
				return err
			}
		}
		reward := skipReward
		if o.arm > 0 {
			reward = 0.0
			if recalled > 0 {
				reward = float64(used) / float64(recalled)
			}
			reward -= 0.01 * float64(o.arm)
		}
		if calledRecall {
			reward -= 0.3 // what came was not enough
		}
		if err := s.update(ctx, o.arm, o.x, reward); err != nil {
			return err
		}
		ib, _ := json.Marshal(o.hits)
		s.mu.Lock()
		_, err := s.db.ExecContext(ctx, `UPDATE retrievals SET reward=?, settled=1, items=? WHERE id=?`, reward, string(ib), o.id)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		s.emit(Event{Type: "settled", Session: session, Detail: map[string]any{"retrieval": o.id, "k": o.arm, "used": used, "recalled": recalled, "reward": reward}})
	}
	return nil
}

// Feedback is a person's word on an item: up or down. It moves the item's
// utility a step, more than one use does.
func (s *Store) Feedback(ctx context.Context, id string, up bool) (Item, error) {
	delta := -0.25
	if up {
		delta = 0.25
	}
	return s.change(ctx, id, "feedback", `UPDATE items SET utility=MAX(0, MIN(1, utility+?)), updated=? WHERE id=?`, delta, time.Now().UnixMilli(), id)
}

// usedIn is whether a reply with words carries text's distinctive words:
// two of them, or half when it has fewer than four.
func usedIn(text string, words map[string]bool) bool {
	var distinct []string
	for _, w := range Words(text) {
		if len([]rune(w)) >= 4 && !slices.Contains(distinct, w) {
			distinct = append(distinct, w)
		}
	}
	if len(distinct) == 0 {
		return false
	}
	hit := 0
	for _, w := range distinct {
		if words[w] || words[stem(w)] {
			hit++
		}
	}
	return hit >= 2 || len(distinct) < 4 && 2*hit >= len(distinct)
}

// Retrievals is the newest automatic recalls, of session when set.
func (s *Store) Retrievals(ctx context.Context, session string, limit int) ([]Retrieval, error) {
	if limit <= 0 {
		limit = 50
	}
	q, args := `SELECT id, session, at, query, arm, features, items, reward, ms FROM retrievals`, []any{}
	if session != "" {
		q, args = q+` WHERE session=?`, append(args, session)
	}
	rows, err := s.db.QueryContext(ctx, q+fmt.Sprintf(` ORDER BY at DESC LIMIT %d`, limit), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Retrieval
	for rows.Next() {
		var r Retrieval
		var at int64
		var fb, ib string
		var reward sql.NullFloat64
		if err := rows.Scan(&r.ID, &r.Session, &at, &r.Query, &r.K, &fb, &ib, &reward, &r.Ms); err != nil {
			return nil, err
		}
		r.At = time.UnixMilli(at).UTC()
		_ = json.Unmarshal([]byte(fb), &r.Features)
		_ = json.Unmarshal([]byte(ib), &r.Hits)
		if reward.Valid {
			r.Reward = &reward.Float64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func b2f(b bool) float64 { return float64(b2i(b)) }
