package knowledge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ClassicItem is a PAI Classic memory item as the server's GET /memory
// returns it. Only the fields an import needs are read.
type ClassicItem struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	Scope   string    `json:"scope"`
	Topic   string    `json:"topic,omitempty"`
	Session string    `json:"session,omitempty"`
	Source  string    `json:"source"`
	Created time.Time `json:"created"`
	Invalid time.Time `json:"invalid,omitzero"`
	Reason  string    `json:"reason,omitempty"`
}

// Skipped is a Classic item an import leaves out, and why.
type Skipped struct {
	ID  string `json:"id"`
	Why string `json:"why"`
}

// ImportPlan is what an import would add. Nothing is written until Import.
type ImportPlan struct {
	Add  []Item    `json:"add"`
	Skip []Skipped `json:"skip"`
}

// PreviewImport maps Classic items to ULTRA candidates, with provenance. It
// never verifies one: Classic memory was mostly extracted by a model, and an
// import is a claim to check, not a fact. Invalid items, items imported
// before, and exact duplicates are skipped.
func (s *Store) PreviewImport(ctx context.Context, items []ClassicItem) (ImportPlan, error) {
	have, err := s.List(ctx, Filter{Statuses: AllStatuses})
	if err != nil {
		return ImportPlan{}, err
	}
	origins, texts := map[string]bool{}, map[string]bool{}
	for _, it := range have {
		origins[it.Origin] = true
		texts[norm(it.Text)] = true
	}
	var p ImportPlan
	seen := map[string]bool{}
	for _, c := range items {
		origin := "classic:" + c.ID
		if seen[c.ID] {
			p.Skip = append(p.Skip, Skipped{c.ID, "listed twice"})
			continue
		}
		seen[c.ID] = true
		switch {
		case c.ID == "" || strings.TrimSpace(c.Text) == "":
			p.Skip = append(p.Skip, Skipped{c.ID, "empty"})
		case !c.Invalid.IsZero():
			p.Skip = append(p.Skip, Skipped{c.ID, "invalid in Classic: " + c.Reason})
		case origins[origin]:
			p.Skip = append(p.Skip, Skipped{c.ID, "imported before"})
		case texts[norm(c.Text)]:
			p.Skip = append(p.Skip, Skipped{c.ID, "same text already known"})
		default:
			texts[norm(c.Text)] = true
			kind := Fact
			switch strings.ToLower(c.Kind) {
			case "preference":
				kind = Preference
			case "decision":
				kind = Decision
			}
			src := []Source{{Kind: "classic-memory", Ref: c.ID, Note: fmt.Sprintf("source %s, scope %s, topic %s", c.Source, c.Scope, c.Topic)}}
			if c.Session != "" {
				src = append(src, Source{Kind: "session", Ref: c.Session})
			}
			p.Add = append(p.Add, Item{Kind: kind, Status: Candidate, Text: c.Text, Scope: "project", Origin: origin, Sources: src, Observed: c.Created})
		}
	}
	return p, nil
}

// Import adds a previewed plan's items, as candidates. A plan is checked
// again: every item must carry a classic: origin, so an edited plan cannot
// slip in an item that would be stored as verified.
func (s *Store) Import(ctx context.Context, p ImportPlan) (int, error) {
	for _, it := range p.Add {
		if !strings.HasPrefix(it.Origin, "classic:") || len(it.Origin) == len("classic:") {
			return 0, fmt.Errorf("import item %q lacks a classic: origin", it.Text)
		}
	}
	n := 0
	for _, it := range p.Add {
		it.Status, it.Verification = Candidate, nil
		if _, err := s.Add(ctx, it); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func norm(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
