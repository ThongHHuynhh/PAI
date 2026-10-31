package harness

import (
	"math"
	"testing"
	"time"

	"pai/pkg/statefile"
)

func TestRelevance(t *testing.T) {
	scored := relevance("repair router pkg/harness/route.go", []routeCandidate{
		{id: "same", text: "repair router pkg/harness/route.go"},
		{id: "other", text: "animate kittens web/kitten.css"},
	})
	if math.Abs(scored[0].score-1) > 1e-12 || scored[1].score != 0 {
		t.Fatalf("identical and unrelated scores: %v", scored)
	}
	scored = relevance("repair router", []routeCandidate{{text: "repair router"}})
	if math.Abs(scored[0].score-wordWeight) > 1e-12 {
		t.Fatalf("word-only weight: %v", scored[0].score)
	}
	scored = relevance("the and for", []routeCandidate{{text: "the and for"}, {text: ""}})
	if scored[0].score != 0 || scored[1].score != 0 {
		t.Fatalf("empty or stop-word documents: %v", scored)
	}
	a := paths(`Edit C:\Repo\pkg\file.go and pkg/harness/route.go`)
	b := paths(`c:/repo/pkg/file.go pkg/harness/route.go web/app.ts`)
	if got := jaccard(a, b); math.Abs(got-2.0/3) > 1e-12 {
		t.Fatalf("normalized path overlap: %v; paths %v / %v", got, a, b)
	}
	// Shared words have lower IDF than words that distinguish the task.
	scored = relevance("router parser", []routeCandidate{{text: "router parser"}, {text: "router router"}, {text: "router other"}})
	if scored[0].score <= scored[1].score || scored[1].score <= scored[2].score {
		t.Fatalf("TF-IDF ordering: %v", scored)
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name                string
		candidates          []routeCandidate
		action, id, handoff string
	}{
		{name: "none", action: "spawn"},
		{name: "warm closed", candidates: []routeCandidate{{id: "one", status: statefile.StatusClosed, score: .3}}, action: "resume", id: "one"},
		{name: "stepping stays warm", candidates: []routeCandidate{{id: "one", status: statefile.StatusStepping, score: .3, age: time.Hour, fill: .5}}, action: "queue", id: "one"},
		{name: "cold weak", candidates: []routeCandidate{{id: "one", status: statefile.StatusClosed, score: .49, age: cacheTTL, fill: .5}}, action: "spawn", handoff: "one"},
		{name: "cold strong", candidates: []routeCandidate{{id: "one", status: statefile.StatusClosed, score: .5, age: cacheTTL, fill: .5}}, action: "resume", id: "one"},
		{name: "full", candidates: []routeCandidate{{id: "one", status: statefile.StatusClosed, score: 1, fill: maxFill}}, action: "spawn", handoff: "one"},
		{name: "failed", candidates: []routeCandidate{{id: "one", status: statefile.StatusFailed, score: 1}}, action: "spawn"},
		{name: "weak handoff", candidates: []routeCandidate{{id: "one", status: statefile.StatusClosed, score: handoffMin - .01}}, action: "spawn"},
		{name: "handoff boundary", candidates: []routeCandidate{{id: "one", status: statefile.StatusClosed, score: handoffMin}}, action: "spawn", handoff: "one"},
		{name: "best eligible", candidates: []routeCandidate{
			{id: "failed", status: statefile.StatusFailed, score: 1},
			{id: "full", status: statefile.StatusClosed, score: 1, fill: .7},
			{id: "related", status: statefile.StatusClosed, score: .4},
			{id: "weak", status: statefile.StatusClosed, score: .2},
		}, action: "resume", id: "related"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := decide(tt.candidates)
			if d.action != tt.action || d.id != tt.id || d.handoff != tt.handoff || d.why == "" {
				t.Fatalf("got %+v; want %s %s, handoff %s", d, tt.action, tt.id, tt.handoff)
			}
		})
	}
}
