// Package memory is what PAI knows across sessions, models and machines: the
// facts, preferences, decisions, todos and procedures it learnt, each with
// where it came from. Nothing is deleted. A wrong or old item is invalidated:
// it stops being recalled, and stays to be seen and restored.
//
// Recall uses cosine over embeddings, weighed by what each item proved
// worth. Optional hybrid mode fuses vectors and BM25 ranks.
// A contextual bandit learns, per kind of turn, whether to recall at all and
// how much, from whether recalled items were used.
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// considered for exact-text deduplication. Similarity alone is insufficient
// to discard a fact; distinct texts go through model-guided consolidation.
const dupCosine = 0.93

// minText is the shortest item worth keeping: a statement shorter than this
// names nothing a later session could use.
const minText = 12

// secret finds text that looks like a credential, which memory never keeps:
// it would be recalled into any session on any model.
var secret = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{16,}|sk-ant-[a-z0-9_-]{16,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|xox[abpr]-[A-Za-z0-9-]{10,}|pai_(key|join)_[A-Za-z0-9_.-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(password|passwd|secret|token|api[_ -]?key)\s*[:=]\s*\S{6,})`)

// injection finds text that addresses the agent instead of stating a fact:
// a page or a tool output trying to plant orders that every later session
// would recall.
var injection = regexp.MustCompile(`(?i)(ignore|disregard|forget|override) (all |any )?(the )?(previous|prior|above|earlier|system) (instructions|prompts?|rules)|\bnew system prompt\b|\byou are now (in )?(developer|dan|jailbreak)|\b(exfiltrate|send|upload|post) (the |all )?(credentials|secrets|keys|passwords)\b`)

// ErrNotUseful is an item refused before it is stored, and why.
var ErrNotUseful = errors.New("memory: not stored")

// Useful is nil when text is worth storing: long enough to say something,
// and no credential in it.
func Useful(text string) error {
	switch {
	case len([]rune(strings.TrimSpace(text))) < minText:
		return fmt.Errorf("%w: too short to be useful", ErrNotUseful)
	case secret.MatchString(text):
		return fmt.Errorf("%w: it looks like it holds a secret; save the secret with the credential tool and remember only what it is for", ErrNotUseful)
	case injection.MatchString(text):
		return fmt.Errorf("%w: it reads as an instruction planted for agents, not a fact", ErrNotUseful)
	}
	return nil
}

// Item is one thing PAI knows.
type Item struct {
	Contract
	Conflicts []string `json:"conflicts,omitempty"` // unresolved peer IDs; never a winner

	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	Scope   string    `json:"scope"`
	Topic   string    `json:"topic,omitempty"`   // a short name for what it is about, such as "deploy" or "editor": related items share one
	Merged  []string  `json:"merged,omitempty"`  // the items it replaced by a merge or an update
	Session string    `json:"session,omitempty"` // where it was learnt
	From    int       `json:"from,omitempty"`    // the records of that session it was learnt from
	To      int       `json:"to,omitempty"`
	Source  string    `json:"source"` // who wrote it: extract, compaction, the model's remember, a person, or a client over MCP
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
	// Set once by the trusted harness; never supplied by a model or MCP client.
	VerifyResolution func(context.Context, string, string, Resolution) error
	// Hybrid enables BM25 rank fusion; semantic vector retrieval is the default.
	Hybrid bool
	db     *sql.DB
	dir    string // holds the credentials' sealing key
	embed  Embedder
	mu     sync.Mutex // one writer: SQLite has one, and dedup reads before it writes
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

// columns are added to stores made before them.
var columns = []struct{ table, name, alter string }{
	{"items", "contract", `ALTER TABLE items ADD COLUMN contract TEXT NOT NULL DEFAULT '{}'`},
	{"items", "topic", `ALTER TABLE items ADD COLUMN topic TEXT NOT NULL DEFAULT ''`},
	{"items", "merged", `ALTER TABLE items ADD COLUMN merged TEXT NOT NULL DEFAULT ''`},
	{"retrievals", "ms", `ALTER TABLE retrievals ADD COLUMN ms INTEGER NOT NULL DEFAULT 0`},
}

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
	for _, c := range columns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.name).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			if _, err := db.Exec(c.alter); err != nil {
				return nil, fmt.Errorf("memory: %w", err)
			}
		}
	}
	if _, err := db.Exec(contractSchema + credentialSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, dir: dir, embed: e}
	if err := s.reembed(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) emit(e Event) {
	if s.OnEvent != nil {
		s.OnEvent(e)
	}
}

// A clock tick can contain several writes, especially on Windows. Random
// IDs also keep new versions distinct across processes and restarts.
func newID() string { return "m" + rand.Text() }

// Remember stores it, unless an item of its scope already says the same:
// then its provenance is appended and the original is returned. The text is one self-contained
// statement.
func (s *Store) Remember(ctx context.Context, it Item) (Item, bool, error) {
	if err := prepare(&it); err != nil {
		return it, false, err
	}
	it.Text = strings.TrimSpace(it.Text)
	it.Topic = strings.ToLower(strings.TrimSpace(it.Topic))
	if err := Useful(it.Text); err != nil {
		return it, false, err
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
	if err := s.validateDependencies(ctx, it); err != nil {
		return it, false, err
	}
	if dup, ok, err := s.nearest(ctx, it); err != nil {
		return it, false, err
	} else if ok {
		// Repetition is provenance, never evidence of utility.
		if err := s.recordObservation(ctx, dup.ID, it); err != nil {
			return it, false, err
		}

		return dup, false, nil
	}
	now := time.Now().UTC()
	it.ID, it.Created, it.Updated = newID(), now, now
	if it.Utility == 0 {
		it.Utility = 0.5
	}
	peers, err := s.conflictPeers(ctx, it)
	if err != nil {
		return it, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return it, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO items (id,kind,text,scope,topic,merged,session,rec_from,rec_to,source,created,updated,pinned,utility,shown,used,embedder,vec,contract) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.ID, it.Kind, it.Text, it.Scope, it.Topic, strings.Join(it.Merged, ","), it.Session, it.From, it.To, it.Source, now.UnixMilli(), now.UnixMilli(), it.Pinned, it.Utility, it.Shown, it.Used, s.embed.Name(), pack(it.vec), contractJSON(it.Contract)); err != nil {
		return it, false, err
	}
	for _, peer := range peers {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_conflicts(a,b,reason) VALUES(?,?,?)`, peer.ID, it.ID, "Possible contradiction: ASK the user; neither source wins"); err != nil {
			return it, false, err
		}
		it.Conflicts = append(it.Conflicts, peer.ID)
	}
	if err := tx.Commit(); err != nil {
		return it, false, err
	}
	if len(it.Conflicts) > 0 {
		it.Status = "conflict"
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
		// Similar meaning alone cannot prove equality: negations, versions and
		// numbers often have almost identical model vectors. Keep distinct facts.
		if it.Subject != o.Subject || it.Property != o.Property || it.Value != o.Value {
			continue
		}
		if c := cosine(it.vec, o.vec); c > best && strings.TrimSuffix(it.Text, ".") == strings.TrimSuffix(o.Text, ".") {
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
	if strings.TrimSpace(reason) == "" || secret.MatchString(reason) {
		return Item{}, fmt.Errorf("memory: invalidation requires a reason")
	}
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

// Edit is a correction of an item's text, kind, scope or topic by source: a
// new version that replaces it, as a merge of one, so the old text stays.
func (s *Store) Edit(ctx context.Context, id, text, kind, scope, topic, source string) (Item, error) {
	it, err := s.Get(ctx, id)
	if err != nil {
		return it, err
	}
	if text = strings.TrimSpace(text); text != "" {
		it.Text = text
		it.Subject, it.Property, it.Value = "", "", ""
		it.Status, it.Verified, it.Observed = "unverified", time.Time{}, time.Now().UTC()
	}
	for _, f := range []struct {
		to *string
		v  string
	}{{&it.Kind, kind}, {&it.Scope, scope}, {&it.Topic, topic}} {
		if f.v != "" {
			*f.to = f.v
		}
	}
	it.Source, it.Shown, it.Used, it.Merged = source, 0, 0, nil
	return s.Merge(ctx, []string{id}, it)
}

func (s *Store) change(ctx context.Context, id, event, q string, args ...any) (Item, error) {
	s.mu.Lock()
	if event != "feedback" {
		if err := s.guard(ctx, id); err != nil {
			s.mu.Unlock()
			return Item{}, err
		}
	}
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
	NoSummaries          bool // leave out compactions' accounts of sessions
}

// List is the items that match f, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Item, error) {
	where, args := []string{"1=1"}, []any{}
	if !f.Invalid {
		where = append(where, "invalid=0")
	}
	if f.NoSummaries {
		where, args = append(where, "kind<>?"), append(args, Summary)
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

const cols = `id,kind,text,scope,topic,merged,session,rec_from,rec_to,source,created,updated,invalid,reason,pinned,utility,shown,used,vec,contract`

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
		var merged, contract string
		if err := rows.Scan(&it.ID, &it.Kind, &it.Text, &it.Scope, &it.Topic, &merged, &it.Session, &it.From, &it.To, &it.Source, &created, &updated, &invalid, &it.Reason, &it.Pinned, &it.Utility, &it.Shown, &it.Used, &vec, &contract); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(contract), &it.Contract); err != nil {
			return nil, err
		}
		it.Created, it.Updated = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		if invalid != 0 {
			it.Invalid = time.UnixMilli(invalid).UTC()
		}
		it.vec = unpack(vec)
		if merged != "" {
			it.Merged = strings.Split(merged, ",")
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	for i := range out {
		if err := s.decorate(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Stats is what the knowledge view heads with.
type Stats struct {
	Items    int            `json:"items"`
	Invalid  int            `json:"invalid"`
	Pinned   int            `json:"pinned"`
	ByKind   map[string]int `json:"by_kind"`
	ByScope  map[string]int `json:"by_scope"`
	Recalls  int            `json:"recalls"`
	Reward   float64        `json:"reward"`    // mean reward of settled recalls
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
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
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
