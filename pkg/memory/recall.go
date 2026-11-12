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
	minCosine    = 0.12 // below this, a vector match is noise, for an embedder with no Floor of its own
	utilityWeigh = 0.6
	recencyWeigh = 0.1
	recencyDays  = 30.0
	sessionBoost = 0.1 // an item learnt in the same session
)

// skipReward is what a turn with no recall earns: it cost nothing and
// helped nothing, so a recall whose items get used beats it.
const skipReward = 0.1

// maxPinned bounds the pinned items one recall carries: they come whatever
// the turn asks, so each one costs context on every turn.
const maxPinned = 10

// Hit is an item a search found, with why: its place in each list and the
// parts of its score.
type Hit struct {
	Item      Item    `json:"item"`
	Score     float64 `json:"score"`
	Lexical   int     `json:"lexical,omitempty"` // place in the BM25 list, from 1; 0 for absent
	Vector    int     `json:"vector,omitempty"`  // place by cosine, from 1
	Cosine    float64 `json:"cosine"`
	Pinned    bool    `json:"pinned,omitempty"`    // here because it is pinned
	Readiness string  `json:"readiness,omitempty"` // reason this is context only, not ready for action
	Used      bool    `json:"used,omitempty"`      // once settled: the reply carried its words
}

// Query is what Find looks for and where. The filters apply before items are
// ranked and cut to K, so a narrow scope still gets its K best.
type Query struct {
	Text              string
	Session           string   // favours what was learnt there
	Subject, Property string   // optional exact identity filters, applied before ranking
	Scope             string   // only this scope and global; none is global plus the current session and Scopes
	Scopes            []string // more scopes a session may read, such as the projects it works on
	K                 int
	NoSummaries       bool // leave out compactions' accounts of sessions
}

// filter is the query's narrowing as SQL to follow a WHERE, and its arguments.
func (q Query) filter() (string, []any) {
	sql, args := "", []any{}
	if q.Scope == "" {
		in := []any{Global, "session:" + q.Session}
		for _, sc := range q.Scopes {
			if sc = NormScope(sc); !slices.Contains(in, any(sc)) {
				in = append(in, sc)
			}
		}
		sql, args = sql+` AND scope IN (`+placeholders(len(in))+`)`, append(args, in...)
	} else {
		sql, args = sql+` AND scope IN (?,?)`, append(args, NormScope(q.Scope), Global)
	}
	if q.Subject != "" {
		sql += " AND json_extract(contract,'$.subject')=?"
		args = append(args, strings.ToLower(strings.TrimSpace(q.Subject)))
	}
	if q.Property != "" {
		sql += " AND json_extract(contract,'$.property')=?"
		args = append(args, strings.ToLower(strings.TrimSpace(q.Property)))
	}
	if q.NoSummaries {
		sql, args = sql+` AND kind<>?`, append(args, Summary)
	}
	return sql, args
}

// Search is the k valid items most relevant to query, best first. session,
// when set, favours what was learnt there.
func (s *Store) Search(ctx context.Context, query, session string, k int) ([]Hit, error) {
	return s.Find(ctx, Query{Text: query, Session: session, K: k})
}

// Pinned is the newest valid pinned items that q's filters allow; q's text
// and K do not matter.
func (s *Store) Pinned(ctx context.Context, q Query) ([]Item, error) {
	filter, args := q.filter()
	return s.query(ctx, `WHERE invalid=0 AND pinned=1`+filter+fmt.Sprintf(` ORDER BY updated DESC LIMIT %d`, maxPinned), args...)
}

// Find is the q.K valid items most relevant to q.Text, best first.
func (s *Store) Find(ctx context.Context, q Query) ([]Hit, error) {
	query, session, k := q.Text, q.Session, q.K
	if k <= 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	qv, floor, err := embedQuery(ctx, s.embed, query)
	if err != nil {
		return nil, err
	}
	filter, filterArgs := q.filter()
	items, err := s.query(ctx, `WHERE invalid=0`+filter, filterArgs...)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Hit{}
	for _, it := range items {
		if len(qv) != len(it.vec) {
			return nil, fmt.Errorf("memory: embedding dimensions changed for %s; use a versioned model name and reindex before searching", it.ID)
		}
		why, err := s.readiness(ctx, it, map[string]bool{})
		if err != nil {
			return nil, err
		}
		byID[it.ID] = &Hit{Item: it, Readiness: why, Cosine: float64(cosine(qv, it.vec))}
	}
	// vector list
	vl := make([]*Hit, 0, len(byID))
	for _, h := range byID {
		if h.Cosine >= floor {
			vl = append(vl, h)
		}
	}
	slices.SortFunc(vl, func(a, b *Hit) int {
		if c := cmpFloat(b.Cosine, a.Cosine); c != 0 {
			return c
		}
		return strings.Compare(a.Item.ID, b.Item.ID)
	})
	for i, h := range vl[:min(len(vl), candidates)] {
		h.Vector = i + 1
	}
	// lexical list
	if fts := ftsQuery(query); s.Hybrid && fts != "" {
		args := append(append([]any{fts}, filterArgs...), candidates)
		rows, err := s.db.QueryContext(ctx, `SELECT items.id FROM items_fts JOIN items ON items.rowid=items_fts.rowid WHERE items_fts MATCH ? AND items.invalid=0`+filter+` ORDER BY bm25(items_fts) LIMIT ?`, args...)
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
		if err := rows.Err(); err != nil {
			return nil, err
		}
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
		base := h.Cosine // semantic relevance is primary by default
		if s.Hybrid {
			base = rrf / (2 / float64(rrfK+1))
		}
		age := now.Sub(h.Item.Updated).Hours() / 24
		h.Score = base*(1-utilityWeigh/2+utilityWeigh*h.Item.Utility) + recencyWeigh*math.Exp(-age/recencyDays)
		if session != "" && h.Item.Session == session {
			h.Score += sessionBoost
		}
		hits = append(hits, *h)
	}
	slices.SortFunc(hits, func(a, b Hit) int {
		if c := cmpFloat(b.Score, a.Score); c != 0 {
			return c
		}
		return strings.Compare(a.Item.ID, b.Item.ID)
	})
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
	Query   string   // the new messages' text
	Fill    float64  // context used over the limit, 0 to 1
	Scopes  []string // project scopes the session works on, beside global and its own
	Exclude []string // items already in the session's context: not recalled again
}

// Projects is the project scopes that hold valid items, such as
// "project:pai": what a session can be matched against.
func (s *Store) Projects(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT scope FROM items WHERE invalid=0 AND scope LIKE 'project:%' ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var sc string
		if err := rows.Scan(&sc); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
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
	// A session's own summary is already its compaction record, and summaries
	// of other sessions are out of scope: automatic recall never carries one.
	var hits []Hit
	if k > 0 {
		q := Query{Text: t.Query, Session: t.Session, Scopes: t.Scopes, K: k + len(t.Exclude), NoSummaries: true}
		if hits, err = s.Find(ctx, q); err != nil {
			return r, err
		}
	}
	for _, h := range hits {
		if len(r.Hits) < k && !slices.Contains(t.Exclude, h.Item.ID) {
			r.Hits = append(r.Hits, h)
		}
	}
	pinned, err := s.Pinned(ctx, Query{Session: t.Session, Scopes: t.Scopes})
	if err != nil {
		return r, err
	}
	for _, p := range pinned {
		if !slices.Contains(t.Exclude, p.ID) && !slices.ContainsFunc(r.Hits, func(h Hit) bool { return h.Item.ID == p.ID }) {
			why, err := s.readiness(ctx, p, map[string]bool{})
			if err != nil {
				return r, err
			}
			r.Hits = append(r.Hits, Hit{Item: p, Pinned: true, Readiness: why})
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

// Settle trains only from explicit retrieval feedback. reply and calledRecall
// remain for caller compatibility; word overlap and asking for more memory
// are never evidence of utility or failure. Unreported retrievals stay unscored.
func (s *Store) Settle(ctx context.Context, session, reply string, calledRecall bool) error {
	return s.score(ctx, session, `settled=0`)
}

// Rescore scores session's retrievals ids that were settled with no
// feedback, now that feedback came after the turn.
func (s *Store) Rescore(ctx context.Context, session string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.score(ctx, session, `reward IS NULL AND id IN (`+placeholders(len(ids))+`)`, anys(ids)...)
}

// score settles session's retrievals that match where: those with feedback
// get a reward and train the bandit, the rest stay unscored.
func (s *Store) score(ctx context.Context, session, where string, args ...any) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, arm, features, items FROM retrievals WHERE session=? AND `+where, append([]any{session}, args...)...)
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

	for _, o := range opens {
		used, recalled, reported := 0, 0, 0
		for i, h := range o.hits {
			var outcome string
			err := s.db.QueryRowContext(ctx, `SELECT outcome FROM memory_feedback WHERE retrieval=? AND item=?`, o.id, h.Item.ID).Scan(&outcome)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			u := outcome == "used"
			if outcome != "" {
				reported++
			}
			o.hits[i].Used = u
			if h.Pinned {
				continue // pinned items come whatever the bandit chose, so they are marked but not scored
			}
			recalled++
			if u {
				used++
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
		// Asking for more memory is encouraged, never penalized.
		if reported == 0 {
			if _, err := s.db.ExecContext(ctx, `UPDATE retrievals SET settled=1 WHERE id=?`, o.id); err != nil {
				return err
			}
			continue
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
