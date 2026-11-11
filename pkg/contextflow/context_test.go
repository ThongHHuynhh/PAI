package contextflow

import (
	"slices"
	"testing"
)

func item(id string, l Layer, task string) Item {
	return Item{ID: id, Layer: l, Task: task, Text: "original fact " + id, Sources: []Ref{{"session", 1}}}
}
func TestProtectedSelectionAndHandoff(t *testing.T) {
	xs := []Item{item("intent", Intent, "a"), item("evidence", Evidence, "a"), item("failure", History, "b"), item("left", Memory, "b"), item("right", Project, "b"), item("unrelated", History, "b")}
	xs[2].Failure = true
	xs[3].Key = "target"
	xs[3].Value = "one"
	xs[4].Key = "target"
	xs[4].Value = "two"
	p, err := Select(xs, "a", Budget{Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Overflow || len(p.Items) != 5 || len(p.Conflicts) != 1 || !slices.Contains(p.Omitted, "unrelated") {
		t.Fatalf("unsafe projection: %+v", p)
	}
	if len(p.Recommendations) < 3 {
		t.Fatal("missing handoff/clarification recommendations")
	}
	// No mutation of originals, including both sides of the conflict.
	if xs[3].Value != "one" || xs[4].Value != "two" {
		t.Fatal("mutated originals")
	}
}
func TestSummaryReferences(t *testing.T) {
	parent := item("original", Evidence, "a")
	child := item("summary", History, "a")
	child.Parents = []string{"original"}
	if err := Validate([]Item{parent, child}); err != nil {
		t.Fatal(err)
	}
	child.Sources = []Ref{{"other", 9}}
	if Validate([]Item{parent, child}) == nil {
		t.Fatal("summary lost original reference")
	}
	child.Sources = parent.Sources
	child.Parents = []string{"summary"}
	if Validate([]Item{parent, child}) == nil {
		t.Fatal("summary cycle accepted")
	}
}
func TestLayerBudgetAndDedup(t *testing.T) {
	a := item("a", Memory, "")
	b := a
	b.ID = "b"
	p, err := Select([]Item{a, b}, "", Budget{Total: 100, Layers: map[Layer]int{Memory: 100}})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != "b" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = Select([]Item{a}, "", Budget{Total: 100, Layers: map[Layer]int{Memory: 0}})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("layer budget ignored: %+v", p)
	}
	if _, err := Select([]Item{a, a}, "", Budget{Total: 100}); err == nil {
		t.Fatal("duplicate identity accepted")
	}
}
