package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"pai/pkg/memory"
	"pai/pkg/statefile"
)

const (
	gradeTurnBytes = 8000 // of one turn's work: its end, where the answer is
	gradeReason    = "auto: the session's model judged the turn after it ended"
	gradeLookback  = 200 // newest retrievals of a session searched for ungraded ones
)

const gradeSystem = `You judge whether recalled memory helped an agent. For each RETRIEVAL you get the user's message, the memory items recalled for it, and the agent's work after it. An item is "used" when its content shaped the agent's answer, a command, a path, a value or a decision. It is "irrelevant" otherwise. Repeating words the user's message already holds is not use. Reply with only a JSON object in one ` + "```json" + ` fence: {"<retrieval id>": {"<item id>": "used" | "irrelevant"}}, with every item of every retrieval.`

var recalledFor = regexp.MustCompile(`^Memory recalled for this turn \(retrieval (\w+)\)`)

// grade has the session's model say which recalled items each turn used, for
// the retrievals in records from through to that no one gave feedback on, so
// recall learns from every turn and not only from turns where the model
// reported. Feedback the model gave itself is never overwritten.
func (h *Harness) grade(ctx context.Context, conf statefile.Conf, id string, chain []statefile.Record, from, to int) {
	open, err := h.Memory.Retrievals(ctx, id, gradeLookback)
	if err != nil {
		log.Printf("session %s: grade: %v", id, err)
		return
	}
	byID := map[string]memory.Retrieval{}
	for _, r := range open {
		if r.Reward == nil && r.K > 0 {
			byID[r.ID] = r
		}
	}
	type turn struct {
		n, i int // record number and place in chain
		rv   memory.Retrieval
	}
	var turns []turn
	for i, r := range chain {
		if r.N <= from || r.N > to || r.Kind != statefile.System || r.Source != "memory" {
			continue
		}
		if m := recalledFor.FindStringSubmatch(string(r.Body)); m != nil {
			if rv, ok := byID[m[1]]; ok && len(unpinned(rv.Hits)) > 0 {
				turns = append(turns, turn{r.N, i, rv})
			}
		}
	}
	if len(turns) == 0 {
		return
	}
	var b strings.Builder
	for i, t := range turns {
		end := to
		if i+1 < len(turns) {
			end = turns[i+1].n - 1
		}
		fmt.Fprintf(&b, "RETRIEVAL %s\nUSER:\n%s\nRECALLED:\n", t.rv.ID, askedAt(chain, t.i))
		for _, hit := range unpinned(t.rv.Hits) {
			fmt.Fprintf(&b, "- [%s] %s\n", hit.Item.ID, hit.Item.Text)
		}
		fmt.Fprintf(&b, "WORK:\n%s\n\n", transcript(chain, t.n, end, gradeTurnBytes))
	}
	out, err := h.complete(ctx, conf, id, gradeSystem, b.String())
	if err != nil {
		log.Printf("session %s: grade: %v", id, err)
		return
	}
	m := fence.FindStringSubmatch(out)
	if m == nil {
		log.Printf("session %s: grade: no json in the reply", id)
		return
	}
	var verdicts map[string]map[string]string
	if err := json.Unmarshal([]byte(m[1]), &verdicts); err != nil {
		log.Printf("session %s: grade: %v", id, err)
		return
	}
	var graded []string
	for _, t := range turns {
		for _, hit := range unpinned(t.rv.Hits) {
			outcome := verdicts[t.rv.ID][hit.Item.ID]
			if outcome != "used" && outcome != "irrelevant" {
				continue
			}
			if err := h.Memory.RetrievalFeedback(ctx, id, t.rv.ID, hit.Item.ID, outcome, gradeReason); err != nil {
				log.Printf("session %s: grade %s: %v", id, hit.Item.ID, err)
			}
		}
		graded = append(graded, t.rv.ID)
	}
	if err := h.Memory.Rescore(ctx, id, graded); err != nil {
		log.Printf("session %s: grade: %v", id, err)
	}
}

// askedAt is the user's text the recall at chain[at] was for: the messages
// just before it.
func askedAt(chain []statefile.Record, at int) string {
	var parts []string
	for i := at - 1; i >= 0 && chain[i].Kind == statefile.Message; i-- {
		if strings.HasPrefix(chain[i].Type, "text/") {
			parts = append([]string{string(chain[i].Body)}, parts...)
		}
	}
	return strings.Join(parts, "\n")
}

func unpinned(hits []memory.Hit) []memory.Hit {
	var out []memory.Hit
	for _, h := range hits {
		if !h.Pinned {
			out = append(out, h)
		}
	}
	return out
}
