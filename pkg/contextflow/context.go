// Package contextflow provides deterministic, loss-aware context projections.
// The caller owns append-only storage; projections never replace originals.
package contextflow

import (
	"fmt"
	"slices"
	"sort"
)

// BytesPerToken is a conservative planning estimate, not provider accounting.
const BytesPerToken = 3

type Layer string

const (
	Policy   Layer = "policy"
	Intent   Layer = "current_intent"
	Task     Layer = "task_state"
	Evidence Layer = "evidence"
	Project  Layer = "project_knowledge"
	Memory   Layer = "memory"
	History  Layer = "history_refs"
)

var layers = []Layer{Policy, Intent, Task, Evidence, Project, Memory, History}

type Ref struct {
	Session string `json:"session"`
	Record  int    `json:"record"`
}
type Item struct {
	ID      string `json:"id"`
	Layer   Layer  `json:"layer"`
	Task    string `json:"task,omitempty"`
	Text    string `json:"text"`
	Sources []Ref  `json:"sources"`
	// Key and Value declare competing facts without guessing semantic equivalence.
	Key        string `json:"key,omitempty"`
	Value      string `json:"value,omitempty"`
	Constraint bool   `json:"constraint,omitempty"`
	Failure    bool   `json:"failure,omitempty"`
	// Parents link a summary to earlier items; sources still link original records.
	Parents []string `json:"parents,omitempty"`
}
type Conflict struct {
	Key   string   `json:"key"`
	Items []string `json:"items"`
}
type Budget struct {
	Total  int           `json:"total"`
	Layers map[Layer]int `json:"layers,omitempty"`
}
type Projection struct {
	Items           []Item     `json:"items"`
	Omitted         []string   `json:"omitted,omitempty"`
	Conflicts       []Conflict `json:"conflicts,omitempty"`
	Tokens          int        `json:"estimated_tokens"`
	Overflow        bool       `json:"overflow"`
	Recommendations []string   `json:"recommendations,omitempty"`
}

func Validate(items []Item) error {
	seen := map[string]Item{}
	for _, x := range items {
		if x.ID == "" || x.Text == "" || !slices.Contains(layers, x.Layer) || len(x.Sources) == 0 {
			return fmt.Errorf("item needs id, known layer, text and original sources")
		}
		if _, ok := seen[x.ID]; ok {
			return fmt.Errorf("duplicate immutable item %s", x.ID)
		}
		if (x.Key == "") != (x.Value == "") {
			return fmt.Errorf("key and value must occur together")
		}
		for _, r := range x.Sources {
			if r.Session == "" || r.Record < 1 {
				return fmt.Errorf("invalid original reference")
			}
		}
		for _, p := range x.Parents {
			parent, ok := seen[p]
			if !ok {
				return fmt.Errorf("summary parent %s must precede child", p)
			}
			for _, ref := range parent.Sources {
				if !slices.Contains(x.Sources, ref) {
					return fmt.Errorf("summary must retain original reference %+v", ref)
				}
			}
		}
		seen[x.ID] = x
	}
	return nil
}
func Conflicts(items []Item) []Conflict {
	keys := map[string][]Item{}
	for _, x := range items {
		if x.Key != "" {
			keys[x.Key] = append(keys[x.Key], x)
		}
	}
	var out []Conflict
	for key, xs := range keys {
		values := map[string]bool{}
		var ids []string
		for _, x := range xs {
			values[x.Value] = true
			ids = append(ids, x.ID)
		}
		if len(values) > 1 {
			out = append(out, Conflict{key, ids})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Select retains all relevant intent/evidence, constraints, failures and both
// sides of every conflict even if that exceeds the budget. Overflow is explicit:
// callers must expand capacity or ask the user, never silently discard them.
func Select(items []Item, task string, b Budget) (Projection, error) {
	if err := Validate(items); err != nil {
		return Projection{}, err
	}
	if b.Total < 0 {
		return Projection{}, fmt.Errorf("negative budget")
	}
	for _, n := range b.Layers {
		if n < 0 {
			return Projection{}, fmt.Errorf("negative layer budget")
		}
	}
	p := Projection{Conflicts: Conflicts(items)}
	contested := map[string]bool{}
	for _, c := range p.Conflicts {
		for _, id := range c.Items {
			contested[id] = true
		}
	}
	used := map[Layer]int{}
	selected := map[string]bool{}
	cost := func(x Item) int { return (len(x.Text) + BytesPerToken - 1) / BytesPerToken }
	protected := func(x Item) bool {
		relevant := x.Task == "" || task == "" || x.Task == task
		return x.Layer == Policy || x.Constraint || x.Failure || contested[x.ID] || relevant && (x.Layer == Intent || x.Layer == Evidence || x.Layer == Task)
	}
	add := func(x Item) { selected[x.ID] = true; p.Tokens += cost(x); used[x.Layer] += cost(x) }
	for _, x := range items {
		if protected(x) {
			add(x)
		}
	}
	duplicates := map[string]bool{}
	unrelated := 0
	for i := len(items) - 1; i >= 0; i-- {
		x := items[i]
		if selected[x.ID] {
			continue
		}
		if x.Task != "" && task != "" && x.Task != task {
			unrelated++
			continue
		}
		fingerprint := string(x.Layer) + "\x00" + x.Text
		if duplicates[fingerprint] {
			continue
		}
		duplicates[fingerprint] = true
		cap, limited := b.Layers[x.Layer]
		if p.Tokens+cost(x) <= b.Total && (!limited || used[x.Layer]+cost(x) <= cap) {
			add(x)
		}
	}
	for _, x := range items {
		if selected[x.ID] {
			p.Items = append(p.Items, x)
		} else {
			p.Omitted = append(p.Omitted, x.ID)
		}
	}
	p.Overflow = p.Tokens > b.Total
	for l, n := range used {
		if cap, ok := b.Layers[l]; ok && n > cap {
			p.Overflow = true
		}
	}
	if len(p.Conflicts) > 0 {
		p.Recommendations = append(p.Recommendations, "ask user to clarify competing facts before dependent actions")
	}
	if p.Overflow {
		p.Recommendations = append(p.Recommendations, "protected context exceeds budget; ask for scope clarification or increase capacity")
	}
	if unrelated > 0 {
		p.Recommendations = append(p.Recommendations, "recommend new session with selected relevant context, original references and unresolved conflicts")
	}
	if len(p.Omitted) > unrelated {
		p.Recommendations = append(p.Recommendations, "redundant or lower priority context omitted from projection; originals remain available")
	}
	return p, nil
}
