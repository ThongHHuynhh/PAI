package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ContractVersion versions the on-disk and model-facing memory envelope.
const ContractVersion = 1

// Contract is optional structured knowledge alongside the required readable prose.
// Source/session/from/to on Item are provenance, never an authority grant.
type Contract struct {
	Version      int       `json:"version"`
	Subject      string    `json:"subject,omitempty"`
	Property     string    `json:"property,omitempty"`
	Value        string    `json:"value,omitempty"`
	Observed     time.Time `json:"observed,omitzero"`
	Verified     time.Time `json:"verified,omitzero"`
	Expires      time.Time `json:"expires,omitzero"`
	Status       string    `json:"status,omitempty"`
	Uncertainty  string    `json:"uncertainty,omitempty"`
	Dependencies []string  `json:"dependencies,omitempty"`
}

// Policy separates durable retention (always history) from retrieval freshness.
type Policy struct {
	ReviewAfter time.Duration
	Retrieval   string
}

// These review intervals request verification, never assert that a fact changed.
var policies = map[string]Policy{
	Preference: {0, "retain until explicitly changed"},
	Fact:       {30 * 24 * time.Hour, "verify operational facts after review interval"},
	Decision:   {0, "retain rationale; check dependencies"},
	Procedure:  {90 * 24 * time.Hour, "verify prerequisites and procedure freshness"},
	Todo:       {7 * 24 * time.Hour, "review open work; completed work is history"},
	Summary:    {0, "session-local context"},
}

func PolicyFor(kind string) Policy { return policies[kind] }

const contractSchema = `
CREATE INDEX IF NOT EXISTS items_identity ON items(scope, json_extract(contract,'$.subject'), json_extract(contract,'$.property'));
CREATE TABLE IF NOT EXISTS memory_conflicts (a TEXT NOT NULL, b TEXT NOT NULL, reason TEXT NOT NULL, PRIMARY KEY(a,b));
CREATE TABLE IF NOT EXISTS memory_resolutions (a TEXT NOT NULL, b TEXT NOT NULL, clarification TEXT NOT NULL, at TEXT NOT NULL, PRIMARY KEY(a,b));
CREATE TABLE IF NOT EXISTS memory_history (seq INTEGER PRIMARY KEY, id TEXT NOT NULL, at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, event TEXT NOT NULL, snapshot TEXT NOT NULL);
CREATE TRIGGER IF NOT EXISTS memory_insert AFTER INSERT ON items BEGIN
 INSERT INTO memory_history(id,event,snapshot) VALUES(new.id,'created',json_object('text',new.text,'kind',new.kind,'scope',new.scope,'source',new.source,'session',new.session,'from',new.rec_from,'to',new.rec_to,'contract',json(new.contract),'merged',new.merged));
END;
CREATE TRIGGER IF NOT EXISTS memory_change AFTER UPDATE OF invalid,reason,pinned ON items BEGIN
 INSERT INTO memory_history(id,event,snapshot) VALUES(new.id,'state',json_object('before_invalid',old.invalid,'before_reason',old.reason,'invalid',new.invalid,'reason',new.reason,'pinned',new.pinned));
END;
CREATE TABLE IF NOT EXISTS memory_feedback (seq INTEGER PRIMARY KEY, retrieval TEXT NOT NULL, item TEXT NOT NULL, outcome TEXT NOT NULL, reason TEXT NOT NULL, at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(retrieval,item));
INSERT INTO memory_history(id,event,snapshot)
 SELECT id,'legacy_snapshot',json_object('text',text,'kind',kind,'scope',scope,'source',source,'session',session,'from',rec_from,'to',rec_to,'contract',json(contract),'merged',merged,'invalid',invalid,'reason',reason)
 FROM items WHERE NOT EXISTS(SELECT 1 FROM memory_history h WHERE h.id=items.id);
CREATE TRIGGER IF NOT EXISTS memory_no_delete BEFORE DELETE ON items BEGIN SELECT RAISE(ABORT,'memory never deletes'); END;
CREATE TRIGGER IF NOT EXISTS memory_content_immutable BEFORE UPDATE OF text,kind,scope,source,session,rec_from,rec_to,contract,merged ON items BEGIN SELECT RAISE(ABORT,'memory corrections require a new version'); END;
CREATE TRIGGER IF NOT EXISTS memory_history_no_update BEFORE UPDATE ON memory_history BEGIN SELECT RAISE(ABORT,'memory history is append only'); END;
CREATE TRIGGER IF NOT EXISTS memory_history_no_delete BEFORE DELETE ON memory_history BEGIN SELECT RAISE(ABORT,'memory history is append only'); END;
`

func contractJSON(c Contract) string { b, _ := json.Marshal(c); return string(b) }

func prepare(it *Item) error {
	if it.Version != 0 && it.Version != ContractVersion {
		return fmt.Errorf("memory: unsupported contract version")
	}
	it.Version = ContractVersion
	it.Subject = strings.ToLower(strings.TrimSpace(it.Subject))
	it.Property = strings.ToLower(strings.TrimSpace(it.Property))
	if (it.Subject == "") != (it.Property == "") || (it.Subject != "" && it.Value == "") {
		return fmt.Errorf("memory: subject, property and value must be supplied together")
	}
	if it.Text == "" && it.Subject != "" {
		it.Text = it.Subject + " / " + it.Property + ": " + it.Value
	}
	// Check the entire envelope before embeddings, persistence or event emission.
	raw, _ := json.Marshal(it)
	if secret.Match(raw) || secret.MatchString(it.Property+"="+it.Value) {
		return fmt.Errorf("%w: it looks like it holds a secret", ErrNotUseful)
	}
	if it.Observed.IsZero() {
		it.Observed = time.Now().UTC()
	}
	if it.Status == "" {
		it.Status = "unverified"
	}
	switch it.Status {
	case "unverified", "verified", "uncertain", "completed":
	default:
		return fmt.Errorf("memory: invalid status")
	}
	if it.Status == "verified" && it.Verified.IsZero() {
		return fmt.Errorf("memory: verified status requires a verification timestamp")
	}
	if it.Status == "verified" && (it.Source == "model" || it.Source == "extract" || it.Source == "consolidation" || it.Source == SourceMCP) {
		return fmt.Errorf("memory: untrusted writers cannot self-verify")
	}
	it.Scope = NormScope(it.Scope)
	scope := it.Scope
	if scope != Global && !(strings.HasPrefix(scope, "project:") && len(scope) > 8) && !(strings.HasPrefix(scope, "session:") && len(scope) > 8) {
		return fmt.Errorf("memory: invalid scope")
	}
	return nil
}

func (s *Store) recordObservation(ctx context.Context, id string, it Item) error {
	b, _ := json.Marshal(it)
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_history(id,event,snapshot) VALUES(?, 'observation', ?)`, id, string(b))
	return err
}

// Only different values for the same explicit identity are deterministic conflicts.
// Related prose is not a contradiction; explicit findings use FlagConflict.
func (s *Store) conflictPeers(ctx context.Context, it Item) ([]Item, error) {
	if it.Kind == Summary {
		return nil, nil
	}
	others, err := s.query(ctx, `WHERE invalid=0 AND scope=? AND id<>? AND kind<>?`, it.Scope, it.ID, Summary)
	if err != nil {
		return nil, err
	}
	var peers []Item
	for _, o := range others {
		structured := it.Subject != "" && o.Subject == it.Subject && o.Property == it.Property
		if structured && o.Value != it.Value {
			peers = append(peers, o)
		}
	}
	return peers, nil
}

// FlagConflict records an advisory finding that two or more different items
// disagree. It never retires or selects a source, never blocks anything, and
// refuses cross-scope links. An item cannot conflict with itself: repeated ids
// count once, and fewer than two different items is refused.
func (s *Store) FlagConflict(ctx context.Context, ids []string, reason string) error {
	if strings.TrimSpace(reason) == "" || secret.MatchString(reason) {
		return fmt.Errorf("memory: conflict needs a non-secret reason")
	}
	var distinct []string
	for _, id := range ids {
		if id != "" && !slices.Contains(distinct, id) {
			distinct = append(distinct, id)
		}
	}
	if len(distinct) < 2 {
		return fmt.Errorf("memory: a conflict needs two different items")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []Item
	for _, id := range distinct {
		it, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		if len(items) > 0 && it.Scope != items[0].Scope {
			return fmt.Errorf("memory: conflict cannot cross scopes")
		}
		items = append(items, it)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, it := range items[1:] {
		// The newest finding's reason replaces an older one, so a pair repair
		// closed is not closed again for its old reason.
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_conflicts(a,b,reason) VALUES(?,?,?) ON CONFLICT(a,b) DO UPDATE SET reason=excluded.reason`, items[0].ID, it.ID, reason); err != nil {
			return err
		}
		// A pair closed automatically (repair) opens again on a new finding;
		// a pair a person settled stays settled.
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_resolutions WHERE a=? AND b=? AND json_extract(clarification,'$.auto') IS NOT NULL`, items[0].ID, it.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) validateDependencies(ctx context.Context, it Item) error {
	for _, id := range it.Dependencies {
		dep, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		if dep.Scope != Global && dep.Scope != scopeOr(it.Scope) {
			return fmt.Errorf("memory: dependency outside scope")
		}
		if err := s.guardDependency(ctx, id, map[string]bool{}); err != nil {
			return err
		}
	}
	return nil
}

// decorate fills an item's open conflicts: the valid other items it was
// flagged against with no recorded resolution. A pair of one item with itself
// is never a conflict, and a peer that was invalidated no longer disputes it.
func (s *Store) decorate(ctx context.Context, it *Item) error {
	rows, err := s.db.QueryContext(ctx, `SELECT CASE WHEN c.a=? THEN c.b ELSE c.a END AS peer FROM memory_conflicts c
		WHERE (c.a=? OR c.b=?) AND c.a<>c.b
		AND NOT EXISTS(SELECT 1 FROM memory_resolutions r WHERE r.a=c.a AND r.b=c.b)
		AND EXISTS(SELECT 1 FROM items p WHERE p.id=(CASE WHEN c.a=? THEN c.b ELSE c.a END) AND p.invalid=0)`, it.ID, it.ID, it.ID, it.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	it.Conflicts = nil
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		it.Conflicts = append(it.Conflicts, id)
	}
	if len(it.Conflicts) > 0 {
		it.Status = "conflict"
	}
	return rows.Err()
}

// guardDependency refuses a dependency that is invalid or part of a cycle. A
// dispute on a dependency is shown where it is recalled; it never blocks a
// write.
func (s *Store) guardDependency(ctx context.Context, id string, seen map[string]bool) error {
	if seen[id] {
		return fmt.Errorf("memory: dependency cycle")
	}
	seen[id] = true
	defer delete(seen, id)
	it, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !it.Invalid.IsZero() {
		return fmt.Errorf("memory: dependency %s is no longer valid", id)
	}
	for _, dep := range it.Dependencies {
		if err := s.guardDependency(ctx, dep, seen); err != nil {
			return err
		}
	}
	return nil
}

// RetrievalFeedback accepts evidence of utility only for an item actually shown
// to this session. Its reason describes an effect on the answer/action, not a
// lexical mention. Contradicted/harmful feedback never resolves factual truth.
func (s *Store) RetrievalFeedback(ctx context.Context, session, retrieval, id, outcome, reason string) error {
	switch outcome {
	case "used", "irrelevant", "contradicted", "harmful":
	default:
		return fmt.Errorf("memory: unknown feedback outcome")
	}
	if strings.TrimSpace(reason) == "" || secret.MatchString(reason) {
		return fmt.Errorf("memory: feedback needs a non-secret explanation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT items FROM retrievals WHERE id=? AND session=?`, retrieval, session).Scan(&raw); err != nil {
		return fmt.Errorf("memory: retrieval not available in this session")
	}
	var hits []Hit
	if err := json.Unmarshal([]byte(raw), &hits); err != nil {
		return err
	}
	found := false
	for _, h := range hits {
		if h.Item.ID == id {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("memory: item was not in this retrieval")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_feedback(retrieval,item,outcome,reason) VALUES(?,?,?,?)`, retrieval, id, outcome, reason); err != nil {
		return err
	}
	used := outcome == "used"
	if _, err = tx.ExecContext(ctx, `UPDATE items SET used=used+?,utility=utility*0.8+? WHERE id=?`, b2i(used), 0.2*b2f(used), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveConflict records a human clarification without choosing or deleting a
// source. Callers must obtain the clarification from the user, never a model
// guess. A subsequent explicit Edit/Invalidate records the requested correction.
type Resolution struct {
	Session string `json:"session"`
	Call    string `json:"ask_call"`
	Answer  string `json:"answer"`
}

func (s *Store) ResolveConflict(ctx context.Context, a, b string, proof Resolution) error {
	if s.VerifyResolution == nil {
		return fmt.Errorf("memory: verified user answer required")
	}
	if err := s.VerifyResolution(ctx, a, b, proof); err != nil {
		return err
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	clarification := string(encoded)
	if strings.TrimSpace(clarification) == "" || secret.MatchString(clarification) {
		return fmt.Errorf("memory: a non-secret user clarification is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `INSERT INTO memory_resolutions(a,b,clarification,at) SELECT a,b,?,? FROM memory_conflicts WHERE (a=? AND b=?) OR (a=? AND b=?)`, clarification, time.Now().UTC().Format(time.RFC3339Nano), a, b, b, a)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory: no such conflict")
	}
	return nil
}

// readiness blocks use of expired, completed or dependency-invalid knowledge.
// Stale knowledge remains visible with a verification request, not as certainty.
func (s *Store) readiness(ctx context.Context, it Item, seen map[string]bool) (string, error) {
	if seen[it.ID] {
		return "dependency cycle requires clarification", nil
	}
	seen[it.ID] = true
	defer delete(seen, it.ID)
	// A dispute is shown on the item's own line (Line); it does not make the
	// item or what depends on it unready.
	if !it.Invalid.IsZero() {
		return "invalid: " + it.Reason, nil
	}
	if it.Status == "completed" {
		return "completed; historical only", nil
	}
	if !it.Expires.IsZero() && time.Now().After(it.Expires) {
		return "expired; verify before use", nil
	}
	for _, id := range it.Dependencies {
		dep, err := s.Get(ctx, id)
		if err != nil {
			return "missing dependency; verify before use", nil
		}
		if dep.Scope != Global && dep.Scope != it.Scope {
			return "dependency outside scope; blocked", nil
		}
		why, err := s.readiness(ctx, dep, seen)
		if err != nil || why != "" {
			return "dependency not ready: " + why, err
		}
	}
	at := it.Verified
	if at.IsZero() {
		at = it.Observed
	}
	if at.IsZero() {
		at = it.Created
	}
	if p := PolicyFor(it.Kind); p.ReviewAfter > 0 && time.Since(at) > p.ReviewAfter {
		return "review due; verify before use", nil
	}
	return "", nil
}
