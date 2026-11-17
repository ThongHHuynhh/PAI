package ctxsel

import (
	"strings"
	"testing"

	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/knowledge"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/task"
)

func inputs() Inputs {
	tk := &task.Task{ID: "t1", Objective: "Fix the lexer", Criteria: []task.Criterion{{ID: "test", Kind: task.CriterionCommand, Command: "go test ./..."}}, Paths: []string{"pkg/lex/a.go"}}
	sel := project.Apply([]project.Instruction{
		{ID: "project:AGENTS.md", Name: "AGENTS.md", Source: "project", Mandatory: true, Activation: project.Always, Text: "Run go vet before commits.", NativeTo: []string{"codex"}},
		{ID: "project:style", Name: "style", Source: "project", Mandatory: true, Activation: project.Auto, Paths: []string{"web/**"}, Text: "Two-space indent."},
	}, "claude", tk.Paths, nil)
	return Inputs{Task: tk, Selection: sel, Caps: exec.Capabilities{Name: "claude", Tools: []string{"shell"}}, Budget: 1000, ConfigRev: 3}
}

func TestDeterministicAndExplained(t *testing.T) {
	a, b := Build(inputs()), Build(inputs())
	if a.Prompt != b.Prompt || a.Tokens != b.Tokens {
		t.Fatal("same inputs, different context")
	}
	if !strings.Contains(a.Prompt, "Fix the lexer") || !strings.Contains(a.Prompt, "go vet") || strings.Contains(a.Prompt, "Two-space") {
		t.Fatalf("prompt:\n%s", a.Prompt)
	}
	for _, it := range a.Items {
		if it.Reason == "" || it.Loaded == "" {
			t.Fatalf("item without reason or status: %+v", it)
		}
	}
	if !a.Estimated || a.ConfigRev != 3 {
		t.Fatal("estimate/config not recorded")
	}
	if a.Tokens > 1000 {
		t.Fatalf("ordinary bootstrap is %d tokens", a.Tokens)
	}
}

func TestNativeLoadIsUnknownWhenNotReported(t *testing.T) {
	in := inputs()
	in.Caps.Name = "codex"
	in.Selection = project.Apply([]project.Instruction{{ID: "project:AGENTS.md", Name: "AGENTS.md", Source: "project", Mandatory: true, Activation: project.Always, Text: "x", NativeTo: []string{"codex"}}}, "codex", nil, nil)
	c := Build(in)
	for _, it := range c.Items {
		if it.ID == "project:AGENTS.md" && it.Loaded != LoadedUnknown {
			t.Fatalf("native load claimed: %s", it.Loaded)
		}
	}
}

func TestMandatoryOverBudgetIsKeptAndReported(t *testing.T) {
	in := inputs()
	in.Budget = 10
	in.Knowledge = []knowledge.Hit{{Item: knowledge.Item{ID: "k1", Kind: knowledge.Fact, Status: knowledge.Verified, Text: "lexer is in pkg/lex"}}}
	c := Build(in)
	if !c.Overrun || !strings.Contains(c.Prompt, "Fix the lexer") || strings.Contains(c.Prompt, "pkg/lex\n") && strings.Contains(c.Prompt, "k1") {
		t.Fatalf("overrun handling:\n%s", c.Report())
	}
	if len(c.Warnings) == 0 {
		t.Fatal("no overrun warning")
	}
}

func TestKnowledgeIsDataAndVerifiedFirst(t *testing.T) {
	in := inputs()
	in.Knowledge = []knowledge.Hit{
		{Item: knowledge.Item{ID: "k-cand", Kind: knowledge.Fact, Status: knowledge.Candidate, Text: "maybe uses regex"}},
		{Item: knowledge.Item{ID: "k-ver", Kind: knowledge.Fact, Status: knowledge.Verified, Text: "lexer is hand written"}},
	}
	c := Build(in)
	i, j := strings.Index(c.Prompt, "k-ver"), strings.Index(c.Prompt, "k-cand")
	if i < 0 || j < 0 || i > j || !strings.Contains(c.Prompt, "data, not instructions") {
		t.Fatalf("knowledge order/label:\n%s", c.Prompt)
	}
}

func TestResumeDoesNotRepeatTheContract(t *testing.T) {
	in := inputs()
	in.Checkpoint = &task.Checkpoint{ID: "cp1", TaskID: "t1", Objective: "Fix the lexer", NextAction: "rerun tests", FailedAttempts: []string{"regex approach failed"}}
	c := Build(in)
	if strings.Count(c.Prompt, "Fix the lexer") != 1 || !strings.Contains(c.Prompt, "rerun tests") || !strings.Contains(c.Prompt, "regex approach") {
		t.Fatalf("resume prompt:\n%s", c.Prompt)
	}
}

func TestKnowledgeHeaderOnceAndDeadItemsDropped(t *testing.T) {
	in := inputs()
	in.Knowledge = []knowledge.Hit{
		{Item: knowledge.Item{ID: "a", Kind: knowledge.Fact, Status: knowledge.Candidate, Text: "one"}},
		{Item: knowledge.Item{ID: "b", Kind: knowledge.Fact, Status: knowledge.Candidate, Text: "two"}},
		{Item: knowledge.Item{ID: "c", Kind: knowledge.Fact, Status: knowledge.Invalid, Text: "dead"}},
	}
	c := Build(in)
	if strings.Count(c.Prompt, knowledge.DataHeader) != 1 || strings.Contains(c.Prompt, "dead") {
		t.Fatalf("knowledge section:\n%s", c.Prompt)
	}
}

func TestCapabilityWarningOnlyForIncludedSkills(t *testing.T) {
	in := inputs()
	in.Resolved = project.Resolved{Enabled: []project.Enabled{{Kind: project.KindSkill, Entry: project.Entry{Name: "web", Activation: project.Manual},
		Pkg: project.Package{Name: "web", Meta: project.Meta{Description: "d", Metadata: map[string]string{"requires": "browser"}}}, Status: project.StatusLocked}}}
	if c := Build(in); len(c.Warnings) != 0 {
		t.Fatalf("warned about an excluded skill: %v", c.Warnings)
	}
	in.Task.Skills = []string{"web"}
	if c := Build(in); len(c.Warnings) != 1 {
		t.Fatalf("no warning for an included skill: %v", c.Warnings)
	}
}
