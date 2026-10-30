// bench measures how PAI carries knowledge from session to session against
// the way Claude Code and Codex do, with no model in the loop, so every run
// gives the same numbers:
//
//   - PAI: items in the memory store, recalled per turn by hybrid search,
//     the learnt policy choosing how many.
//   - Claude Code style: every remembered line in CLAUDE.md, which loads whole
//     into every session.
//   - Codex style: the same lines in AGENTS.md, which Codex reads up to
//     project_doc_max_bytes, 32 KiB by default.
//   - No memory: a fresh session knows nothing of earlier ones.
//
// The corpus is synthetic personal knowledge: machines, projects, databases,
// preferences, decisions, procedures, with repeats and later updates. Each
// question names one fact; recall is whether that fact, in its newest
// version, reaches the model's context.
//
//	go run ./bench > bench/results.json
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"pai/pkg/memory"
)

// codexDocMax is Codex's default project_doc_max_bytes: what it reads of
// AGENTS.md. A token is about four bytes of English.
const (
	codexDocMax   = 32 << 10
	bytesPerToken = 4
	recallK       = 6
)

var sizes = []int{50, 200, 800, 2000}

type fact struct {
	id, text, question, answer string
	stale                      string // the text this one replaced, if an update
}

var (
	machines = []string{"atlas", "nova", "forge", "ember", "orion", "vega", "lyra", "zephyr", "quartz", "cobalt", "aurora", "nimbus"}
	oses     = []string{"Ubuntu 26.04", "macOS 26", "Windows 11", "Debian 13", "Fedora 43"}
	services = []string{"Grafana", "the Postgres primary", "the build cache", "Home Assistant", "the Ollama models", "the backup vault", "the CI runners", "the media server"}
	projects = []string{"orbit", "ledger", "harbor", "pulse", "beacon", "atlas-web", "inkwell", "meridian", "summit", "relay"}
	dbs      = []string{"Postgres 17", "MySQL 9", "SQLite", "ClickHouse", "Redis 8"}
	tools    = []string{"GitHub Actions to Fly.io", "Kamal to a Hetzner VPS", "Nomad", "plain systemd units", "Docker Compose on the NAS"}
	reasons  = []string{"it is cheapest", "it rolls back in seconds", "the team knows it", "it needs no Kubernetes"}
	people   = []string{"the user", "Mai", "Linh", "Quan", "Tuan", "Hoa"}
	langs    = []string{"Go", "TypeScript", "Rust", "Python", "Zig", "Elixir"}
	areas    = []string{"backend services", "web front ends", "data scripts", "CLI tools", "daemons"}
)

// corpus is n facts in the order they were learnt, with repeats said again
// in other words and some facts updated later.
func corpus(n int, rng *rand.Rand) (learnt []fact, gold []fact) {
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	for i := 0; len(gold) < n; i++ {
		var f fact
		f.id = fmt.Sprint(i)
		switch i % 5 {
		case 0:
			m, o, s := machines[i%len(machines)]+fmt.Sprint(i/len(machines)), pick(oses), pick(services)
			f.text = fmt.Sprintf("The machine %s runs %s and hosts %s.", m, o, s)
			f.question, f.answer = fmt.Sprintf("Which operating system is on machine %s, and what does it host?", m), m
		case 1:
			p, d, port := projects[i%len(projects)]+fmt.Sprint(i/len(projects)), pick(dbs), 5000+rng.IntN(4000)
			f.text = fmt.Sprintf("Project %s stores its data in %s on port %d.", p, d, port)
			f.question, f.answer = fmt.Sprintf("What port does the %s database for project %s listen on?", d, p), fmt.Sprint(port)
		case 2:
			who, l, a := pick(people)+fmt.Sprint(i), pick(langs), pick(areas)
			f.text = fmt.Sprintf("%s prefers %s for %s.", who, l, a)
			f.question, f.answer = fmt.Sprintf("Which language does %s prefer for %s?", who, a), l
		case 3:
			p, t, r := projects[i%len(projects)]+fmt.Sprint(i/len(projects)), pick(tools), pick(reasons)
			f.text = fmt.Sprintf("We decided to deploy project %s with %s because %s.", p, t, r)
			f.question, f.answer = fmt.Sprintf("How do we deploy project %s, and why?", p), t
		case 4:
			p, m := projects[i%len(projects)]+fmt.Sprint(i/len(projects)), pick(machines)
			cmd := fmt.Sprintf("./rotate-%d.sh", i)
			f.text = fmt.Sprintf("To rotate the secrets of project %s, run %s on machine %s.", p, cmd, m)
			f.question, f.answer = fmt.Sprintf("How do I rotate the secrets for project %s?", p), cmd
		}
		learnt = append(learnt, f)
		gold = append(gold, f)
		// a tenth are said again later, in other words
		if rng.IntN(10) == 0 {
			learnt = append(learnt, fact{id: f.id, text: "As noted before: " + strings.TrimSuffix(f.text, ".") + "."})
		}
		// a seventh of the database facts change later: a new port
		if i%5 == 1 && rng.IntN(7) == 0 {
			old := f
			port := 9000 + rng.IntN(900)
			u := fact{id: f.id, text: strings.Replace(old.text, old.answer, fmt.Sprint(port), 1) + " It moved from port " + old.answer + ".", question: old.question, answer: fmt.Sprint(port), stale: old.text}
			learnt = append(learnt, u)
			gold[len(gold)-1] = u
		}
	}
	return learnt, gold
}

type row struct {
	Strategy     string  `json:"strategy"`
	Facts        int     `json:"facts"`
	Recall       float64 `json:"recall"`        // newest version of the asked fact in context
	StaleExposed float64 `json:"stale_exposed"` // an outdated version is in context too, for updated facts
	Tokens       float64 `json:"tokens"`        // memory tokens in context per turn
	LatencyMS    float64 `json:"latency_ms"`    // to build the turn's memory
	Note         string  `json:"note,omitempty"`
}

func main() {
	ctx := context.Background()
	var rows []row
	for _, n := range sizes {
		rng := rand.New(rand.NewPCG(42, uint64(n)))
		learnt, gold := corpus(n, rng)
		var file strings.Builder
		file.WriteString("# Memory\n\n")
		for _, f := range learnt {
			file.WriteString("- " + f.text + "\n")
		}
		doc := file.String()

		// PAI, twice: as the store keeps it with no model, and with updates
		// applied, as consolidation or the model's forget does.
		for _, updates := range []bool{false, true} {
			dir, _ := os.MkdirTemp("", "paibench")
			st, err := memory.Open(dir, memory.Hash{})
			if err != nil {
				panic(err)
			}
			ids := map[string]string{}
			for _, f := range learnt {
				it, _, err := st.Remember(ctx, memory.Item{Text: f.text, Source: "bench"})
				if err != nil {
					panic(err)
				}
				if updates && f.stale != "" {
					if old, ok := ids[f.id]; ok && old != it.ID {
						if _, err := st.Invalidate(ctx, old, "updated"); err != nil {
							panic(err)
						}
					}
				}
				if f.question != "" || ids[f.id] == "" {
					ids[f.id] = it.ID
				}
			}
			var hit, stale, updated int
			var toks, ms float64
			for _, g := range gold {
				t0 := time.Now()
				hits, err := st.Search(ctx, g.question, "", recallK)
				if err != nil {
					panic(err)
				}
				ms += float64(time.Since(t0).Microseconds()) / 1000
				var c strings.Builder
				for _, h := range hits {
					c.WriteString("- " + h.Item.Text + "\n")
				}
				got := c.String()
				toks += float64(len(got)) / bytesPerToken
				if strings.Contains(got, g.text) {
					hit++
				}
				if g.stale != "" {
					updated++
					if strings.Contains(got, g.stale) {
						stale++
					}
				}
			}
			name, note := "PAI (store only, no model)", "hybrid recall, 6 items, nothing consolidated"
			if updates {
				name, note = "PAI (updates applied)", "the old version retired when it changed, as forget or consolidation does"
			}
			rows = append(rows, row{name, n, frac(hit, len(gold)), frac(stale, updated), toks / float64(len(gold)), ms / float64(len(gold)), note})
			_ = st.Close()
			_ = os.RemoveAll(dir)
		}

		// Claude Code: CLAUDE.md, whole, every session.
		var stale, updated int
		for _, g := range gold {
			if g.stale != "" {
				updated++
				if strings.Contains(doc, g.stale) {
					stale++
				}
			}
		}
		rows = append(rows, row{"Claude Code style (CLAUDE.md)", n, recallIn(doc, gold), frac(stale, updated), float64(len(doc)) / bytesPerToken, 0, "the whole file in every session"})

		// Codex: AGENTS.md up to 32 KiB.
		cut := doc[:min(len(doc), codexDocMax)]
		stale, updated = 0, 0
		for _, g := range gold {
			if g.stale != "" {
				updated++
				if strings.Contains(cut, g.stale) {
					stale++
				}
			}
		}
		rows = append(rows, row{"Codex style (AGENTS.md)", n, recallIn(cut, gold), frac(stale, updated), float64(len(cut)) / bytesPerToken, 0, "read up to project_doc_max_bytes, 32 KiB"})

		rows = append(rows, row{"No memory", n, 0, 0, 0, 0, "a fresh session"})
	}
	out := map[string]any{"rows": rows, "policy": policy(ctx), "facts_note": "synthetic personal knowledge; a tenth said twice, a seventh of the database facts updated", "k": recallK}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		panic(err)
	}
}

func frac(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func recallIn(doc string, gold []fact) float64 {
	hit := 0
	for _, g := range gold {
		if strings.Contains(doc, g.text) {
			hit++
		}
	}
	return frac(hit, len(gold))
}

// window is the learnt policy over a stretch of turns.
type window struct {
	Turns     int     `json:"turns"`
	Tokens    float64 `json:"tokens"`     // memory tokens per turn
	AskRecall float64 `json:"ask_recall"` // questions whose fact reached the context
	SkipChat  float64 `json:"skip_chat"`  // chatter turns with no recall
}

// policy runs PAI's own recall and settle over a workload of questions and
// chatter, against recalling a fixed 6 every turn. The model is simulated:
// it uses a recalled fact when the question asks for it, and nothing in
// reply to chatter.
func policy(ctx context.Context) map[string]any {
	rng := rand.New(rand.NewPCG(7, 7))
	_, gold := corpus(300, rng)
	run := func(learn bool) []window {
		dir, _ := os.MkdirTemp("", "paibench")
		defer func() { _ = os.RemoveAll(dir) }()
		st, err := memory.Open(dir, memory.Hash{})
		if err != nil {
			panic(err)
		}
		defer func() { _ = st.Close() }()
		for _, g := range gold {
			if _, _, err := st.Remember(ctx, memory.Item{Text: g.text, Source: "bench"}); err != nil {
				panic(err)
			}
		}
		chatter := []string{"ok thanks", "great", "sounds good", "nice, go on", "cool", "thank you!", "yes please", "perfect"}
		var ws []window
		var w window
		var asks, askHit, chats, chatSkip int
		for turn := 1; turn <= 600; turn++ {
			ask := rng.IntN(2) == 0
			var g fact
			q := chatter[rng.IntN(len(chatter))]
			if ask {
				g = gold[rng.IntN(len(gold))]
				q = g.question
			}
			var hits []memory.Hit
			if learn {
				r, err := st.Recall(ctx, memory.Turn{Session: "bench", Query: q, Fill: 0.3})
				if err != nil {
					panic(err)
				}
				hits = r.Hits
			} else {
				hits, _ = st.Search(ctx, q, "", recallK)
			}
			var c strings.Builder
			for _, h := range hits {
				c.WriteString("- " + h.Item.Text + "\n")
			}
			w.Tokens += float64(c.Len()) / bytesPerToken
			reply := "Sure."
			if ask {
				asks++
				if strings.Contains(c.String(), g.text) {
					askHit++
					reply = "From memory: " + g.text
				} else {
					reply = "I do not know that yet."
				}
			} else {
				chats++
				if len(hits) == 0 {
					chatSkip++
				}
			}
			if learn {
				if err := st.Settle(ctx, "bench", reply, false); err != nil {
					panic(err)
				}
			}
			if turn%100 == 0 {
				ws = append(ws, window{turn, w.Tokens / 100, frac(askHit, asks), frac(chatSkip, chats)})
				w, asks, askHit, chats, chatSkip = window{}, 0, 0, 0, 0
			}
		}
		return ws
	}
	return map[string]any{"learnt": run(true), "fixed": run(false)}
}
