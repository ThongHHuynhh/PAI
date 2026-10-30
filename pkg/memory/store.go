// Package memory is what PAI knows across sessions, models and machines: the
// facts, preferences, decisions, todos and procedures it learnt, each with
// where it came from. Nothing is deleted. A wrong or old item is invalidated:
// it stops being recalled, and stays to be seen and restored.
//
// Recall is hybrid: BM25 over the text (SQLite FTS5) and cosine over
// embeddings, fused by rank, then weighed by what each item proved worth.
// A contextual bandit learns, per kind of turn, whether to recall at all and
// how much, from whether recalled items were used.
package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Kinds of item. A procedure is how to do something; a summary is a
// compaction's account of a stretch of a session.
const (
	Fact       = "fact"
	Preference = "preference"
	Decision   = "decision"
	Todo       = "todo"
	Procedure  = "procedure"
	Summary    = "summary"
)

var kinds = []string{Fact, Preference, Decision, Todo, Procedure, Summary}

// Global is the scope of what holds everywhere; a project's scope is
// "project:<name>", a session's "session:<id>".
const Global = "global"

// dupCosine is how alike two items' embeddings are before the newer one is
// taken as the same item said again. Hash embeddings of a reworded sentence
// sit near 0.8; a repeat sits above 0.95.
const dupCosine = 0.93

// Item is one thing PAI knows.
type Item struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	Scope   string    `json:"scope"`
	Session string    `json:"session,omitempty"` // where it was learnt
	From    int       `json:"from,omitempty"`    // the records of that session it was learnt from
	To      int       `json:"to,omitempty"`
	Source  string    `json:"source"` // who wrote it: extract, compaction, the model's remember, or a person
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Invalid is when it stopped holding, and Reason why; zero while valid.
	Invalid time.Time `json:"invalid,omitzero"`
	Reason  string    `json:"reason,omitempty"`
	Pinned  bool      `json:"pinned"`
	Utility float64   `json:"utility"` // moving average of whether it was used when recalled, 0 to 1
	Shown   int       `json:"shown"`   // times recalled
	Used    int       `json:"used"`    // times used once recalled
	vec     []float32
}

// Event is a change to memory, as the event stream shows it.
type Event struct {
	Type    string `json:"type"` // remembered, merged, invalidated, restored, pinned, recalled, settled, compacted
	Session string `json:"session,omitempty"`
	Items   []Item `json:"items,omitempty"`
	Detail  any    `json:"detail,omitempty"`
}

type Store struct {
	db    *sql.DB
	embed Embedder
	mu    sync.Mutex // one writer: SQLite has one, and dedup reads before it writes
	// OnEvent, when set, hears every change. It must not block.
	OnEvent func(Event)
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id TEXT PRIMARY KEY, kind TEXT NOT NULL, text TEXT NOT NULL, scope TEXT NOT NULL,
	session TEXT NOT NULL DEFAULT '', rec_from INTEGER NOT NULL DEFAULT 0, rec_to INTEGER NOT NULL DEFAULT 0,
	source TEXT NOT NULL, created INTEGER NOT NULL, updated INTEGER NOT NULL,
	invalid INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '',
	pinned INTEGER NOT NULL DEFAULT 0, utility REAL NOT NULL DEFAULT 0.5,
	shown INTEGER NOT NULL DEFAULT 0, used INTEGER NOT NULL DEFAULT 0,
	embedder TEXT NOT NULL DEFAULT '', vec BLOB
);
CREATE INDEX IF NOT EXISTS items_scope ON items(scope, invalid);
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(text, content='items', content_rowid='rowid', tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS items_ai AFTER INSERT ON items BEGIN
	INSERT INTO items_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TRIGGER IF NOT EXISTS items_ad AFTER DELETE ON items BEGIN
	INSERT INTO items_fts(items_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;
CREATE TRIGGER IF NOT EXISTS items_au AFTER UPDATE OF text ON items BEGIN
	INSERT INTO items_fts(items_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
	INSERT INTO items_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TABLE IF NOT EXISTS retrievals (
	id TEXT PRIMARY KEY, session TEXT NOT NULL, at INTEGER NOT NULL, query TEXT NOT NULL,
	arm INTEGER NOT NULL, features TEXT NOT NULL, items TEXT NOT NULL,
	reward REAL, settled INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS retrievals_session ON retrievals(session, settled);
CREATE TABLE IF NOT EXISTS bandit (arm INTEGER PRIMARY KEY, a TEXT NOT NULL, b TEXT NOT NULL, n INTEGER NOT NULL, reward REAL NOT NULL);
CREATE TABLE IF NOT EXISTS extracted (session TEXT PRIMARY KEY, upto INTEGER NOT NULL);
`

// Open is the store in dir, made there the first time, with e to embed.
func Open(dir string, e Embedder) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "memory.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	s := &Store{db: db, embed: e}
	return s, s.reembed(context.Background())
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) emit(e Event) {
	if s.OnEvent != nil {
		s.OnEvent(e)
	}
}

func newID() string { return fmt.Sprintf("m%x", time.Now().UnixNano()&0xffffffffffff) }

// Remember stores it, unless an item of its scope already says the same:
// then that one is refreshed and returned. The text is one self-contained
// statement.
func (s *Store) Remember(ctx context.Context, it Item) (Item, bool, error) {
	it.Text = strings.TrimSpace(it.Text)
	if it.Text == "" {
		return it, false, errors.New("memory: empty text")
	}
	if it.Kind == "" {
		it.Kind = Fact
	}
	if !valid(it.Kind) {
		return it, false, fmt.Errorf("memory: kind %q is not one of %s", it.Kind, strings.Join(kinds, ", "))
	}
	if it.Scope == "" {
		it.Scope = Global
	}
	vecs, err := s.embed.Embed(ctx, []string{it.Text})
	if err != nil {
		return it, false, fmt.Errorf("memory: embed: %w", err)
	}
	it.vec = vecs[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	if dup, ok, err := s.nearest(ctx, it); err != nil {
		return it, false, err
	} else if ok {
		dup.Updated = time.Now().UTC()
		dup.Utility = min(1, dup.Utility+0.05) // said again: worth a little more
		if _, err := s.db.ExecContext(ctx, `UPDATE items SET updated=?, utility=? WHERE id=?`, dup.Updated.UnixMilli(), dup.Utility, dup.ID); err != nil {
			return it, false, err
		}
		s.emit(Event{Type: "merged", Session: it.Session, Items: []Item{dup}})
		return dup, false, nil
	}
	now := time.Now().UTC()
	it.ID, it.Created, it.Updated, it.Utility = newID(), now, now, 0.5
	if _, err := s.db.ExecContext(ctx, `INSERT INTO items (id,kind,text,scope,session,rec_from,rec_to,source,created,updated,pinned,utility,embedder,vec) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.ID, it.Kind, it.Text, it.Scope, it.Session, it.From, it.To, it.Source, now.UnixMilli(), now.UnixMilli(), it.Pinned, it.Utility, s.embed.Name(), pack(it.vec)); err != nil {
		return it, false, err
	}
	s.emit(Event{Type: "remembered", Session: it.Session, Items: []Item{it}})
	return it, true, nil
}

// nearest is the valid item of it's scope and kind most like it, if it is
// alike past dupCosine. Caller holds mu.
func (s *Store) nearest(ctx context.Context, it Item) (Item, bool, error) {
	items, err := s.query(ctx, `WHERE invalid=0 AND scope=? AND kind=?`, it.Scope, it.Kind)
	if err != nil {
		return Item{}, false, err
	}
	best, bi := float32(0), -1
	for i, o := range items {
		if c := cosine(it.vec, o.vec); c > best {
			best, bi = c, i
		}
	}
	if bi < 0 || best < dupCosine {
		return Item{}, false, nil
	}
	return items[bi], true, nil
}

// Invalidate stops it from being recalled, with why. It stays in the store.
func (s *Store) Invalidate(ctx context.Context, id, reason string) (Item, error) {
	return s.change(ctx, id, "invalidated", `UPDATE items SET invalid=?, reason=?, updated=? WHERE id=?`, time.Now().UnixMilli(), reason, time.Now().UnixMilli(), id)
}

// Restore makes an invalidated item valid again.
func (s *Store) Restore(ctx context.Context, id string) (Item, error) {
	return s.change(ctx, id, "restored", `UPDATE items SET invalid=0, reason='', updated=? WHERE id=?`, time.Now().UnixMilli(), id)
}

// Pin keeps an item in every recall of its scope, or stops doing so.
func (s *Store) Pin(ctx context.Context, id string, on bool) (Item, error) {
	return s.change(ctx, id, "pinned", `UPDATE items SET pinned=?, updated=? WHERE id=?`, on, time.Now().UnixMilli(), id)
}

// Edit replaces an item's text, kind or scope, as a person corrects it.
func (s *Store) Edit(ctx context.Context, id, text, kind, scope string) (Item, error) {
	it, err := s.Get(ctx, id)
	if err != nil {
		return it, err
	}
	if text = strings.TrimSpace(text); text == "" {
		text = it.Text
	}
	if kind == "" {
		kind = it.Kind
	}
	if !valid(kind) {
		return it, fmt.Errorf("memory: kind %q is not one of %s", kind, strings.Join(kinds, ", "))
	}
	if scope == "" {
		scope = it.Scope
	}
	vecs, err := s.embed.Embed(ctx, []string{text})
	if err != nil {
		return it, err
	}
	return s.change(ctx, id, "edited", `UPDATE items SET text=?, kind=?, scope=?, vec=?, embedder=?, updated=? WHERE id=?`, text, kind, scope, pack(vecs[0]), s.embed.Name(), time.Now().UnixMilli(), id)
}

func (s *Store) change(ctx context.Context, id, event, q string, args ...any) (Item, error) {
	s.mu.Lock()
	res, err := s.db.ExecContext(ctx, q, args...)
	s.mu.Unlock()
	if err != nil {
		return Item{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Item{}, fmt.Errorf("memory: no item %s", id)
	}
	it, err := s.Get(ctx, id)
	if err == nil {
		s.emit(Event{Type: event, Items: []Item{it}})
	}
	return it, err
}

// Get is one item, valid or not.
func (s *Store) Get(ctx context.Context, id string) (Item, error) {
	items, err := s.query(ctx, `WHERE id=?`, id)
	if err != nil {
		return Item{}, err
	}
	if len(items) == 0 {
		return Item{}, fmt.Errorf("memory: no item %s", id)
	}
	return items[0], nil
}

// Filter narrows List. Zero values match everything; invalid items show
// only with Invalid.
type Filter struct {
	Kind, Scope, Session string
	Invalid              bool
	Limit                int
}

// List is the items that match f, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Item, error) {
	where, args := []string{"1=1"}, []any{}
	if !f.Invalid {
		where = append(where, "invalid=0")
	}
	for col, v := range map[string]string{"kind": f.Kind, "scope": f.Scope, "session": f.Session} {
		if v != "" {
			where, args = append(where, col+"=?"), append(args, v)
		}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	return s.query(ctx, "WHERE "+strings.Join(where, " AND ")+fmt.Sprintf(" ORDER BY updated DESC LIMIT %d", limit), args...)
}

const cols = `id,kind,text,scope,session,rec_from,rec_to,source,created,updated,invalid,reason,pinned,utility,shown,used,vec`

func (s *Store) query(ctx context.Context, where string, args ...any) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols+" FROM items "+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		var it Item
		var created, updated, invalid int64
		var vec []byte
		if err := rows.Scan(&it.ID, &it.Kind, &it.Text, &it.Scope, &it.Session, &it.From, &it.To, &it.Source, &created, &updated, &invalid, &it.Reason, &it.Pinned, &it.Utility, &it.Shown, &it.Used, &vec); err != nil {
			return nil, err
		}
		it.Created, it.Updated = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		if invalid != 0 {
			it.Invalid = time.UnixMilli(invalid).UTC()
		}
		it.vec = unpack(vec)
		out = append(out, it)
	}
	return out, rows.Err()
}

// Stats is what the knowledge view heads with.
type Stats struct {
	Items    int            `json:"items"`
	Invalid  int            `json:"invalid"`
	Pinned   int            `json:"pinned"`
	ByKind   map[string]int `json:"by_kind"`
	ByScope  map[string]int `json:"by_scope"`
	Recalls  int            `json:"recalls"`
	Reward   float64        `json:"reward"`   // mean reward of settled recalls
	UsedRate float64        `json:"used_rate"` // share of recalled items that were used
	Embedder string         `json:"embedder"`
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{ByKind: map[string]int{}, ByScope: map[string]int{}, Embedder: s.embed.Name()}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, scope, invalid<>0, pinned, shown, used FROM items`)
	if err != nil {
		return st, err
	}
	shown, used := 0, 0
	for rows.Next() {
		var kind, scope string
		var invalid, pinned bool
		var sh, us int
		if err := rows.Scan(&kind, &scope, &invalid, &pinned, &sh, &us); err != nil {
			_ = rows.Close()
			return st, err
		}
		if invalid {
			st.Invalid++
			continue
		}
		st.Items++
		st.ByKind[kind]++
		st.ByScope[scope]++
		if pinned {
			st.Pinned++
		}
		shown, used = shown+sh, used+us
	}
	_ = rows.Close()
	if shown > 0 {
		st.UsedRate = float64(used) / float64(shown)
	}
	var reward sql.NullFloat64
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), AVG(reward) FROM retrievals WHERE settled=1`).Scan(&st.Recalls, &reward)
	st.Reward = reward.Float64
	return st, err
}

// Extracted is the last record of session that extraction has read.
func (s *Store) Extracted(ctx context.Context, session string) int {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT upto FROM extracted WHERE session=?`, session).Scan(&n)
	return n
}

// SetExtracted notes that extraction read session through record upto.
func (s *Store) SetExtracted(ctx context.Context, session string, upto int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO extracted (session, upto) VALUES (?,?) ON CONFLICT(session) DO UPDATE SET upto=excluded.upto`, session, upto)
	return err
}

// reembed gives every item a vector from the embedder in use: items written
// before it changed, or with none.
func (s *Store) reembed(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text FROM items WHERE embedder<>? OR vec IS NULL`, s.embed.Name())
	if err != nil {
		return err
	}
	var ids, texts []string
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			_ = rows.Close()
			return err
		}
		ids, texts = append(ids, id), append(texts, text)
	}
	_ = rows.Close()
	for i := 0; i < len(ids); i += 64 {
		j := min(len(ids), i+64)
		vecs, err := s.embed.Embed(ctx, texts[i:j])
		if err != nil {
			return fmt.Errorf("memory: re-embed: %w", err)
		}
		for k, v := range vecs {
			if _, err := s.db.ExecContext(ctx, `UPDATE items SET vec=?, embedder=? WHERE id=?`, pack(v), s.embed.Name(), ids[i+k]); err != nil {
				return err
			}
		}
	}
	return nil
}

func valid(kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
