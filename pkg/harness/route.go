package harness

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"pai/pkg/statefile"
)

// These are the prototype's routing thresholds. Keep the cache window below
// its five-minute lifetime; large contexts cost more to resume when cold.
const (
	cacheTTL      = 270 * time.Second
	maxFill       = 0.6            // fuller contexts reason worse and approach compaction
	baseThreshold = 0.3            // relevance needed to reuse a warm child
	coldPenalty   = 0.4            // extra relevance per unit of fill when cold
	handoffMin    = 0.15           // weaker finished work adds noise to a fresh brief
	wordWeight    = 0.6            // words carry 60%, file paths the remaining 40%
	candidateTTL  = 24 * time.Hour // older children are unlikely to retain useful context
)

var (
	routePaths = regexp.MustCompile(`(?:[A-Za-z]:[\\/][^\s"'<>|*?]+|(?:[\w.-]+[\\/])+[\w.-]+\.\w{1,6})`)
	routeWords = regexp.MustCompile(`[a-z_][a-z0-9_]{2,}`)
	routeStop  = strings.Fields("the a an and or to of in on for with is are be it this that from by as at we you i not do does")
)

type routeCandidate struct {
	id, text string
	status   statefile.Status
	fill     float64
	age      time.Duration
	score    float64
}

type routeDecision struct {
	action, id, handoff, why string
}

func paths(text string) map[string]bool {
	out := map[string]bool{}
	for _, p := range routePaths.FindAllString(text, -1) {
		out[strings.ReplaceAll(strings.ToLower(p), `\`, "/")] = true
	}
	return out
}

func words(text string) map[string]float64 {
	out := map[string]float64{}
	for _, w := range routeWords.FindAllString(strings.ToLower(text), -1) {
		stop := false
		for _, s := range routeStop {
			stop = stop || w == s
		}
		if !stop {
			out[w]++
		}
	}
	return out
}

func cosine(a, b, idf map[string]float64) float64 {
	var dot, na, nb float64
	for w, x := range a {
		v := x * idf[w]
		na += v * v
		dot += v * b[w] * idf[w]
	}
	for w, x := range b {
		v := x * idf[w]
		nb += v * v
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func jaccard(a, b map[string]bool) float64 {
	intersection := 0
	for p := range a {
		if b[p] {
			intersection++
		}
	}
	if union := len(a) + len(b) - intersection; union > 0 {
		return float64(intersection) / float64(union)
	}
	return 0
}

// relevance fits IDF to this task and its candidates, just as the prototype
// does. No model call or persistent index is needed.
func relevance(task string, candidates []routeCandidate) []routeCandidate {
	docs := []map[string]float64{words(task)}
	for _, c := range candidates {
		docs = append(docs, words(c.text))
	}
	idf := map[string]float64{}
	for _, d := range docs {
		for w := range d {
			idf[w]++
		}
	}
	for w, n := range idf {
		idf[w] = math.Log(1 + float64(len(docs))/n)
	}
	tp := paths(task)
	scored := append([]routeCandidate(nil), candidates...)
	for i := range scored {
		scored[i].score = wordWeight*cosine(docs[i+1], docs[0], idf) + (1-wordWeight)*jaccard(paths(scored[i].text), tp)
	}
	return scored
}

// decide is pure. Failed and full children cannot be reused, but a closed
// child's report can still be handed to a fresh child.
func decide(scored []routeCandidate) routeDecision {
	var best, prior *routeCandidate
	for i := range scored {
		c := &scored[i]
		if c.status != statefile.StatusFailed && c.fill < maxFill && (best == nil || c.score > best.score) {
			best = c
		}
		if c.status == statefile.StatusClosed && (prior == nil || c.score > prior.score) {
			prior = c
		}
	}
	if best != nil {
		warm := best.status == statefile.StatusStepping || best.age < cacheTTL
		need, temperature := baseThreshold, "warm"
		if !warm {
			need += coldPenalty * best.fill
			temperature = "cold"
		}
		if best.score >= need {
			action := "resume"
			if best.status == statefile.StatusStepping {
				action = "queue"
			}
			return routeDecision{action: action, id: best.id, why: fmt.Sprintf("relevance %.2f >= %.2f, fill %.2f, %s", best.score, need, best.fill, temperature)}
		}
	}
	d := routeDecision{action: "spawn", why: "no candidate"}
	if best != nil {
		d.why = fmt.Sprintf("best relevance %.2f too low or context too full", best.score)
	}
	if prior != nil && prior.score >= handoffMin {
		d.handoff = prior.id
	}
	return d
}
