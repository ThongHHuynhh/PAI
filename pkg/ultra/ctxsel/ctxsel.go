// Package ctxsel builds the context an ULTRA execution starts with, by rule:
// no model routes or summarizes it. Every item records why it was included
// and its estimated tokens. Mandatory items (the contract, applicable
// mandatory rules, unknown operations) are never cut; optional items fill
// the soft budget in order, and an overrun is reported, not hidden.
package ctxsel

import (
	"fmt"
	"slices"
	"strings"

	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/knowledge"
	"pai/pkg/ultra/project"
	"pai/pkg/ultra/task"
)

// Item kinds.
const (
	KindProtocol    = "protocol"
	KindObjective   = "objective"
	KindCriteria    = "criteria"
	KindConstraint  = "constraint"
	KindInstruction = "instruction"
	KindSkillMeta   = "skill"
	KindSkillFull   = "skill_full"
	KindCheckpoint  = "checkpoint"
	KindKnowledge   = "knowledge"
)

// Loaded statuses: whether text is in the executor's context.
const (
	LoadedYes     = "loaded"          // PAI put it in the prompt
	LoadedNative  = "loaded natively" // the executor reads it itself and reports it
	LoadedUnknown = "unknown"         // the executor reads it itself and does not say
	LoadedNo      = "not loaded"      // excluded, with the reason
	LoadedOnAsk   = "on demand"       // a skill's description was given; its file is read when needed
)

// headingAllowance is the estimated tokens of the section headings render adds.
const headingAllowance = 60

// Item is one candidate piece of context.
type Item struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Reason    string `json:"reason"`
	Mandatory bool   `json:"mandatory"`
	Included  bool   `json:"included"`
	Loaded    string `json:"loaded"`
	Tokens    int    `json:"tokens"` // estimated
	Text      string `json:"-"`
}

// Inputs is what selection draws on.
type Inputs struct {
	Task       *task.Task
	Resolved   project.Resolved
	Selection  project.Selection
	Caps       exec.Capabilities
	Checkpoint *task.Checkpoint
	Drift      *task.Drift
	Knowledge  []knowledge.Hit
	Resume     bool // a resume uses the resume budget
	Budget     int  // the soft budget, in estimated tokens
	ConfigRev  int
}

// Context is a selection's result.
type Context struct {
	Items     []Item   `json:"items"`
	Prompt    string   `json:"-"`
	Tokens    int      `json:"tokens"`    // estimated tokens of the prompt
	Mandatory int      `json:"mandatory"` // estimated tokens that cannot be cut
	Budget    int      `json:"budget"`
	Overrun   bool     `json:"overrun"`
	Warnings  []string `json:"warnings,omitempty"`
	ConfigRev int      `json:"config_revision"`
	// Estimated is always true: these are PAI's byte-based estimates.
	// Provider-reported usage is recorded per run, separately.
	Estimated bool `json:"estimated"`
}

func tokens(s string) int { return project.EstimateTokens(s) }

// Build selects context. It is deterministic: the same inputs give the same prompt.
func Build(in Inputs) Context {
	c := Context{Budget: in.Budget, ConfigRev: in.ConfigRev, Estimated: true}
	add := func(it Item) {
		it.Tokens = tokens(it.Text)
		if it.Loaded == "" {
			it.Loaded = LoadedNo
			if it.Included {
				it.Loaded = LoadedYes
			}
		}
		c.Items = append(c.Items, it)
	}
	t := in.Task
	// 1. The contract: always whole.
	add(Item{ID: "protocol", Kind: KindProtocol, Source: "pai", Reason: "how to report a result", Mandatory: true, Included: true, Text: exec.ResultProtocol})
	if t != nil {
		obj := fmt.Sprintf("Task %s: %s", t.ID, t.Objective)
		add(Item{ID: "objective", Kind: KindObjective, Source: "task", Reason: "the objective", Mandatory: true, Included: true, Text: obj})
		if len(t.Criteria) > 0 {
			var b strings.Builder
			b.WriteString("Acceptance criteria (PAI checks these; they are not yours to change):\n")
			for _, cr := range t.Criteria {
				fmt.Fprintf(&b, "- [%s] %s\n", cr.ID, cr.Describe())
			}
			add(Item{ID: "criteria", Kind: KindCriteria, Source: "task", Reason: "acceptance criteria", Mandatory: true, Included: true, Text: b.String()})
		}
		for i, x := range t.Constraints {
			add(Item{ID: fmt.Sprintf("constraint-%d", i+1), Kind: KindConstraint, Source: "task", Reason: "task constraint", Mandatory: true, Included: true, Text: "Constraint: " + x})
		}
		for i, x := range t.Decisions {
			add(Item{ID: fmt.Sprintf("decision-%d", i+1), Kind: KindConstraint, Source: "task", Reason: "recorded decision", Mandatory: true, Included: true, Text: "Decision: " + x})
		}
		if len(t.Paths) > 0 {
			add(Item{ID: "scope", Kind: KindConstraint, Source: "task", Reason: "declared path scope", Mandatory: true, Included: true, Text: "Work within: " + strings.Join(t.Paths, ", ")})
		}
	}
	// 2. Instructions, by the project's precedence. A file the executor
	// loads on its own is "unknown" until a run's report confirms it.
	for _, a := range in.Selection.Applied {
		it := Item{ID: a.ID, Kind: KindInstruction, Source: a.Source + " " + a.Path, Reason: a.Reason, Mandatory: a.Include && a.Mandatory, Included: a.Include}
		switch {
		case a.Include:
			it.Text = fmt.Sprintf("Instructions from %s (%s, rev %s):\n%s", a.Name, a.Source, a.Revision, strings.TrimSpace(a.Text))
		case a.Native:
			it.Loaded = LoadedUnknown
			it.Reason += "; not confirmed by the executor"
		}
		add(it)
	}
	// 3. Skills: description first, the file on demand; "always" in full.
	var paths []string
	if t != nil && len(t.Paths) > 0 {
		paths = t.Paths
	}
	for _, en := range in.Resolved.Enabled {
		if en.Kind != project.KindSkill {
			continue
		}
		if en.Status == project.StatusMissing {
			c.Warnings = append(c.Warnings, fmt.Sprintf("skill %s is enabled but its content is missing", en.Entry.Name))
			continue
		}
		name := en.Pkg.Name
		act := en.ActivationOf()
		it := Item{ID: "skill:" + name, Kind: KindSkillMeta, Source: en.Pkg.Library + " " + project.Short(en.Pkg.Hash)}
		if nativeSkill(en.Pkg.Library, in.Caps) {
			it.Loaded, it.Reason = LoadedUnknown, in.Caps.Name+" loads this folder's skills itself; not sent again"
			add(it)
			continue
		}
		switch {
		case act == project.Manual && (t == nil || !slices.Contains(t.Skills, name)):
			it.Reason = "manual: not asked for by the task"
		case len(en.PathsOf()) > 0 && paths != nil && !project.InScope(en.PathsOf(), paths):
			it.Reason = "path scope does not match the task"
		case act == project.Always:
			it.Kind, it.Included, it.Mandatory, it.Reason = KindSkillFull, true, true, "activation always: full instructions"
			it.Text = fmt.Sprintf("Skill %s:\n%s", name, strings.TrimSpace(en.Text()))
		default:
			it.Included, it.Reason, it.Loaded = true, "enabled; description only, file read when needed", LoadedOnAsk
			it.Text = fmt.Sprintf("- %s: %s (instructions: %s)", name, oneLine(en.Pkg.Meta.Description), en.Pkg.Path)
		}
		if it.Included {
			for _, req := range en.Pkg.Meta.Requires() {
				if !in.Caps.Has(req) {
					c.Warnings = append(c.Warnings, fmt.Sprintf("skill %s needs %q, which %s does not offer", name, req, in.Caps.Name))
				}
			}
		}
		add(it)
	}
	// 4. The checkpoint, on resume: its mandatory part whole (without the
	// contract the task items already state), then its history to budget.
	if cp := in.Checkpoint; cp != nil {
		must, history := cp.Parts(in.Drift, t == nil)
		add(Item{ID: "checkpoint:" + cp.ID, Kind: KindCheckpoint, Source: "checkpoint", Reason: "resume: state at the newest checkpoint", Mandatory: true, Included: true, Text: must})
		for i, h := range history {
			add(Item{ID: fmt.Sprintf("checkpoint:%s#%d", cp.ID, i+1), Kind: KindCheckpoint, Source: "checkpoint", Reason: "resume: history, newest first", Included: true, Text: h})
		}
	}
	// 5. Knowledge, verified before candidates, while it fits. Superseded and
	// invalid items never come. A person's verified decision or preference is
	// mandatory like the task's own.
	ks := slices.DeleteFunc(slices.Clone(in.Knowledge), func(h knowledge.Hit) bool { return !h.Live() })
	slices.SortStableFunc(ks, func(a, b knowledge.Hit) int { return rankStatus(a.Effective()) - rankStatus(b.Effective()) })
	for _, h := range ks {
		must := h.Effective() == knowledge.Verified && (h.Kind == knowledge.Decision || h.Kind == knowledge.Preference)
		add(Item{ID: "knowledge:" + h.ID, Kind: KindKnowledge, Source: h.Origin, Reason: fmt.Sprintf("%s %s, %s", h.Effective(), h.Kind, h.Why), Mandatory: must, Included: must, Text: knowledge.Line(h)})
	}
	// Fill: mandatory always, then optional in order while the budget lasts.
	// Section headings are counted up front.
	used := headingAllowance
	for i := range c.Items {
		if c.Items[i].Mandatory && c.Items[i].Included {
			used += c.Items[i].Tokens
		}
	}
	c.Mandatory = used
	for i := range c.Items {
		it := &c.Items[i]
		if it.Mandatory || it.Text == "" {
			continue
		}
		if it.Kind == KindKnowledge || it.Included {
			if used+it.Tokens <= in.Budget {
				it.Included, it.Loaded = true, loadedFor(it)
				used += it.Tokens
			} else {
				it.Included, it.Loaded = false, LoadedNo
				it.Reason += "; left out: over the budget"
			}
		}
	}
	c.Overrun = c.Mandatory > in.Budget
	if c.Overrun {
		c.Warnings = append(c.Warnings, fmt.Sprintf("mandatory context is ~%d tokens, over the %d budget; nothing mandatory was cut", c.Mandatory, in.Budget))
	}
	for _, x := range in.Selection.Unresolved() {
		c.Warnings = append(c.Warnings, fmt.Sprintf("unresolved instruction conflict on %q between %s", x.Key, strings.Join(x.IDs, ", ")))
	}
	for _, x := range in.Selection.Conflicts {
		if x.Resolved {
			c.Warnings = append(c.Warnings, fmt.Sprintf("instructions disagree on %q; %s wins by precedence", x.Key, x.Winner))
		}
	}
	if n := len(in.Selection.Duplicates); n > 0 {
		c.Warnings = append(c.Warnings, fmt.Sprintf("%d requirement lines appear in more than one instruction source", n))
	}
	c.Prompt = render(c.Items)
	c.Tokens = tokens(c.Prompt)
	if !c.Overrun && c.Tokens > in.Budget {
		c.Overrun = true
		c.Warnings = append(c.Warnings, fmt.Sprintf("the prompt is ~%d tokens, over the %d budget", c.Tokens, in.Budget))
	}
	return c
}

func loadedFor(it *Item) string {
	if it.Kind == KindSkillMeta {
		return LoadedOnAsk
	}
	return LoadedYes
}

func rankStatus(s string) int {
	switch s {
	case knowledge.Verified:
		return 0
	case knowledge.Candidate:
		return 1
	case knowledge.Stale:
		return 2
	}
	return 3
}

// render composes the prompt: contract and policy first, then data.
func render(items []Item) string {
	var b strings.Builder
	section := func(kinds []string, head string) {
		first := true
		for _, it := range items {
			if !it.Included || !slices.Contains(kinds, it.Kind) {
				continue
			}
			if first && head != "" {
				b.WriteString(head + "\n")
			}
			first = false
			b.WriteString(strings.TrimRight(it.Text, "\n") + "\n")
		}
		if !first {
			b.WriteString("\n")
		}
	}
	section([]string{KindObjective, KindCriteria, KindConstraint}, "# Task (PAI ULTRA)")
	section([]string{KindInstruction}, "# Instructions")
	section([]string{KindSkillFull}, "# Skills in full")
	section([]string{KindSkillMeta}, "# Skills available (read a skill's file before using it)")
	section([]string{KindCheckpoint}, "# Resume")
	section([]string{KindKnowledge}, "# Retrieved data\n"+knowledge.DataHeader)
	section([]string{KindProtocol}, "# Reporting")
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Report renders a context's items as a table for people: what, why, cost.
func (c Context) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Effective context: ~%d tokens (estimate), budget %d, mandatory ~%d, config r%d\n", c.Tokens, c.Budget, c.Mandatory, c.ConfigRev)
	for _, it := range c.Items {
		mark := "-"
		if it.Included {
			mark = "+"
		}
		must := ""
		if it.Mandatory {
			must = " mandatory"
		}
		fmt.Fprintf(&b, "%s %-28s %-12s %5d  %s%s; %s\n", mark, cut(it.ID, 28), it.Kind, it.Tokens, it.Loaded, must, it.Reason)
	}
	for _, w := range c.Warnings {
		fmt.Fprintf(&b, "! %s\n", w)
	}
	return b.String()
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// nativeSkill is whether the executor reads this library's skills on its
// own (Claude Code reads ~/.claude/skills and the project's .claude/skills),
// so sending them again would only repeat them.
func nativeSkill(library string, caps exec.Capabilities) bool {
	switch library {
	case project.ClaudeLibrary:
		return slices.Contains(caps.AutoLoads, "~/.claude/skills")
	case project.ProjectClaudeLibrary:
		return slices.Contains(caps.AutoLoads, ".claude/skills")
	}
	return false
}
