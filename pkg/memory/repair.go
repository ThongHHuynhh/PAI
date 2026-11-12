package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// legacyConsolidationFlag is the reason v0.4.x gave every update or retire a
// consolidation pass proposed. Those flags were proposals, not contradictions
// found in the facts, so they are closed when the store opens.
const legacyConsolidationFlag = "Automatic consolidation proposed changing these sources.%"

// repair fixes what earlier releases left in a store when it opens. It only
// adds rows: closed conflicts get a resolution that says why, and an item in
// a scope spelt with capitals gets a new version in the lower case scope
// that names it, with the old one invalid as merged into it. Each step is
// idempotent, so opening twice changes nothing more.
func (s *Store) repair(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	why, _ := json.Marshal(map[string]string{"auto": "closed on open: an item cannot conflict with itself, and v0.4.x consolidation proposals were not contradictions"})
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO memory_resolutions(a,b,clarification,at)
		SELECT a,b,?,? FROM memory_conflicts WHERE a=b OR reason LIKE ?`, string(why), now, legacyConsolidationFlag); err != nil {
		return fmt.Errorf("memory: repair conflicts: %w", err)
	}
	return s.normalizeScopes(ctx)
}

// normalizeScopes moves valid items whose scope is not in its NormScope form
// into that form: as a new version naming the old, or, when the target scope
// already holds the same statement, by retiring the old as merged into it.
func (s *Store) normalizeScopes(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, text, scope FROM items WHERE invalid=0 AND scope<>lower(trim(scope))`)
	if err != nil {
		return err
	}
	type old struct{ id, kind, text, scope string }
	var olds []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.kind, &o.text, &o.scope); err != nil {
			_ = rows.Close()
			return err
		}
		olds = append(olds, o)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range olds {
		if err := s.moveScope(ctx, o.id, o.kind, o.text, NormScope(o.scope)); err != nil {
			return fmt.Errorf("memory: normalize scope of %s: %w", o.id, err)
		}
	}
	return nil
}

func (s *Store) moveScope(ctx context.Context, id, kind, text, scope string) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	target := ""
	same, err := tx.QueryContext(ctx, `SELECT id, text FROM items WHERE invalid=0 AND scope=? AND kind=?`, scope, kind)
	if err != nil {
		return err
	}
	for same.Next() {
		var oid, otext string
		if err := same.Scan(&oid, &otext); err != nil {
			_ = same.Close()
			return err
		}
		if target == "" && sameText(otext, text) {
			target = oid
		}
	}
	_ = same.Close()
	if target == "" {
		target = newID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO items (id,kind,text,scope,topic,merged,session,rec_from,rec_to,source,created,updated,pinned,utility,shown,used,embedder,vec,contract)
			SELECT ?,kind,text,?,topic,?,session,rec_from,rec_to,source,created,?,pinned,utility,shown,used,embedder,vec,contract FROM items WHERE id=?`, target, scope, id, now, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE items SET invalid=?, reason=?, updated=? WHERE id=?`, now, MergedInto+target, now, id); err != nil {
		return err
	}
	return tx.Commit()
}
