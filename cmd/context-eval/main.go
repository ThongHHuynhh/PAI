// context-eval compares deterministic context selection policies locally.
// It performs no model calls and measures retention, not answer quality.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"pai/pkg/contextflow"
)

type fixture struct {
	Name     string             `json:"name"`
	Task     string             `json:"task"`
	Budget   int                `json:"budget"`
	Items    []contextflow.Item `json:"items"`
	Required []string           `json:"required"`
}
type score struct {
	Fixture               string `json:"fixture"`
	Policy                string `json:"policy"`
	RequiredRetained      int    `json:"required_retained"`
	RequiredTotal         int    `json:"required_total"`
	ConflictSidesRetained int    `json:"conflict_sides_retained"`
	ConflictSidesTotal    int    `json:"conflict_sides_total"`
	Tokens                int    `json:"estimated_tokens"`
	Overflow              bool   `json:"overflow"`
}

func main() {
	input := flag.String("input", "", "optional JSON array of fixtures with items and required IDs")
	flag.Parse()
	fixtures := defaults()
	if *input != "" {
		b, err := os.ReadFile(*input)
		check(err)
		check(json.Unmarshal(b, &fixtures))
	}
	var scores []score
	for _, f := range fixtures {
		check(contextflow.Validate(f.Items))
		if f.Budget < 0 {
			check(fmt.Errorf("negative fixture budget"))
		}
		for _, policy := range []string{"full", "recency", "typed"} {
			var selected []contextflow.Item
			tokens := 0
			if policy == "typed" {
				p, err := contextflow.Select(f.Items, f.Task, contextflow.Budget{Total: f.Budget})
				check(err)
				selected = p.Items
				tokens = p.Tokens
			} else {
				for i := len(f.Items) - 1; i >= 0; i-- {
					x := f.Items[i]
					n := (len(x.Text) + contextflow.BytesPerToken - 1) / contextflow.BytesPerToken
					if policy == "full" || tokens+n <= f.Budget {
						selected = append(selected, x)
						tokens += n
					}
				}
			}
			ids := map[string]bool{}
			for _, x := range selected {
				ids[x.ID] = true
			}
			s := score{Fixture: f.Name, Policy: policy, RequiredTotal: len(f.Required), Tokens: tokens, Overflow: tokens > f.Budget}
			for _, id := range f.Required {
				if ids[id] {
					s.RequiredRetained++
				}
			}
			for _, c := range contextflow.Conflicts(f.Items) {
				for _, id := range c.Items {
					s.ConflictSidesTotal++
					if ids[id] {
						s.ConflictSidesRetained++
					}
				}
			}
			scores = append(scores, s)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	check(enc.Encode(scores))
}
func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func defaults() []fixture {
	makeItem := func(id string, l contextflow.Layer, text string) contextflow.Item {
		return contextflow.Item{ID: id, Layer: l, Task: "active", Text: text, Sources: []contextflow.Ref{{Session: "fixture", Record: 1}}}
	}
	intent := makeItem("intent", contextflow.Intent, "Do not deploy or change credentials.")
	a := makeItem("a", contextflow.Evidence, "Original source says target alpha.")
	a.Key = "target"
	a.Value = "alpha"
	b := makeItem("b", contextflow.Evidence, "New source says target beta.")
	b.Key = "target"
	b.Value = "beta"
	failure := makeItem("failure", contextflow.History, "Previous attempt failed: stale file hash.")
	failure.Failure = true
	noise := makeItem("noise", contextflow.History, "Unrelated task notes fill the newest history.")
	noise.Task = "other"
	return []fixture{
		{Name: "contradiction-pressure", Task: "active", Budget: 25, Items: []contextflow.Item{intent, a, b, failure, noise}, Required: []string{"intent", "a", "b", "failure"}},
		{Name: "ample-budget", Task: "active", Budget: 200, Items: []contextflow.Item{intent, a, b, failure, noise}, Required: []string{"intent", "a", "b", "failure"}},
	}
}
