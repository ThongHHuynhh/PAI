// context-eval compares context selection policies locally. It measures
// retention, which evidence each policy keeps under a budget, and answer
// quality: each fixture's questions are answered from only the selected
// context and graded by the facts the answer must and must not hold. With
// -answer, a command answers, such as a model's CLI reading the prompt on
// stdin; without it, an extractive answerer that quotes the selected items
// gives a model-free upper bound.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"pai/pkg/contextflow"
)

// question is asked of the selected context. The answer is correct when it
// holds every expect and no forbid, case-insensitively.
type question struct {
	Ask    string   `json:"ask"`
	Expect []string `json:"expect"`
	Forbid []string `json:"forbid,omitempty"`
}

type fixture struct {
	Name      string             `json:"name"`
	Task      string             `json:"task"`
	Budget    int                `json:"budget"`
	Items     []contextflow.Item `json:"items"`
	Required  []string           `json:"required"`
	Questions []question         `json:"questions,omitempty"`
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
	AnswersCorrect        int    `json:"answers_correct"`
	AnswersTotal          int    `json:"answers_total"`
	ForbiddenStated       int    `json:"forbidden_stated"` // answers that said something they must not
}

// answer answers q from ctx: by the -answer command, or by quoting it.
func answer(cmd, ctx, q string) string {
	if cmd == "" {
		return ctx
	}
	prompt := "Answer the question using only this context. When sources conflict, say so.\n\nContext:\n" + ctx + "\nQuestion: " + q + "\n"
	c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sh, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		sh, flag = "cmd.exe", "/c"
	}
	x := exec.CommandContext(c, sh, flag, cmd)
	x.Stdin, x.Stderr = strings.NewReader(prompt), os.Stderr
	out, err := x.Output()
	check(err)
	return string(bytes.TrimSpace(out))
}

func grade(a string, q question) (ok, forbidden bool) {
	a = strings.ToLower(a)
	ok = true
	for _, e := range q.Expect {
		ok = ok && strings.Contains(a, strings.ToLower(e))
	}
	for _, x := range q.Forbid {
		forbidden = forbidden || strings.Contains(a, strings.ToLower(x))
	}
	return ok && !forbidden, forbidden
}

func main() {
	input := flag.String("input", "", "optional JSON array of fixtures with items, required IDs and questions")
	answerCmd := flag.String("answer", "", "shell command that answers a prompt on stdin, such as a model CLI; empty quotes the context")
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
			var ctx strings.Builder
			for i := len(selected) - 1; i >= 0; i-- { // oldest first, as a context reads
				fmt.Fprintf(&ctx, "- %s\n", selected[i].Text)
			}
			for _, q := range f.Questions {
				ok, bad := grade(answer(*answerCmd, ctx.String(), q.Ask), q)
				s.AnswersTotal++
				if ok {
					s.AnswersCorrect++
				}
				if bad {
					s.ForbiddenStated++
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
	questions := []question{
		{Ask: "May I deploy now?", Expect: []string{"do not deploy"}},
		{Ask: "Which target is it? Name every value the sources give.", Expect: []string{"alpha", "beta"}},
		{Ask: "Why did the previous attempt fail?", Expect: []string{"stale file hash"}},
	}
	return []fixture{
		{Name: "contradiction-pressure", Task: "active", Budget: 25, Items: []contextflow.Item{intent, a, b, failure, noise}, Required: []string{"intent", "a", "b", "failure"}, Questions: questions},
		{Name: "ample-budget", Task: "active", Budget: 200, Items: []contextflow.Item{intent, a, b, failure, noise}, Required: []string{"intent", "a", "b", "failure"}, Questions: questions},
	}
}
