package project

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"pai/pkg/ultra/mode"
)

// Owners say who may change an instruction.
const (
	OwnerProject = "project" // project-owned Markdown: editable, with a diff before apply
	OwnerPackage = "package" // a library's file: read-only; edit by making a project override
	OwnerGlobal  = "global"  // the person's global rules: read-only from a project
)

// minDuplicateLine is the shortest normalized line compared for duplicates:
// shorter lines ("## Rules", "- Yes") repeat by accident.
const minDuplicateLine = 24

// Instruction is one source of instructions, from any origin.
type Instruction struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Source     string     `json:"source"` // override, project, profile, package, global
	Owner      string     `json:"owner"`
	Path       string     `json:"path"`
	Revision   string     `json:"revision"` // content hash, short
	Mandatory  bool       `json:"mandatory"`
	Activation Activation `json:"activation"`
	Paths      []string   `json:"paths,omitempty"`
	Key        string     `json:"key,omitempty"`
	Value      string     `json:"value,omitempty"`
	Text       string     `json:"-"`
	Tokens     int        `json:"tokens"`
	Rank       int        `json:"rank"` // position in the precedence order; lower wins
	// NativeTo is the executors that load this file themselves; ULTRA does
	// not inject it into them, to avoid sending it twice.
	NativeTo   []string `json:"native_to,omitempty"`
	Overrides  string   `json:"overrides,omitempty"`   // the package instruction this replaces
	ShadowedBy string   `json:"shadowed_by,omitempty"` // the override that replaces this one
}

// Editable is whether ULTRA may write this instruction's file in place.
func (i Instruction) Editable() bool { return i.Owner == OwnerProject }

// nativeFile is an instruction file an executor reads on its own.
type nativeFile struct {
	rel      string
	nativeTo []string
}

var projectNative = []nativeFile{
	{"AGENTS.md", []string{"codex"}},
	{"CLAUDE.md", []string{"claude"}},
	{"CLAUDE.local.md", []string{"claude"}},
}

// globalNative is relative to the home directory.
var globalNative = []nativeFile{
	{".claude/CLAUDE.md", []string{"claude"}},
	{".codex/AGENTS.md", []string{"codex"}},
}

// Inventory lists every instruction source the project can see, with its
// precedence. It reads files only; it never writes or calls a model.
func (p *Project) Inventory(r Resolved) []Instruction {
	order := p.Manifest.Order()
	rank := func(src string) int { return slices.Index(order, src) }
	var out []Instruction
	add := func(in Instruction) {
		in.Rank = rank(in.Source)
		in.Tokens = EstimateTokens(in.Text)
		out = append(out, in)
	}
	readFile := func(id, name, src, owner, path string, native []string) {
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		_, body, _ := splitFrontMatter(b)
		add(Instruction{ID: id, Name: name, Source: src, Owner: owner, Path: path, Revision: short(hashBytes(b)), Mandatory: true, Activation: Always, Text: body, NativeTo: native})
	}
	if h, err := os.UserHomeDir(); err == nil {
		for _, f := range globalNative {
			readFile("global:~/"+f.rel, "~/"+f.rel, SourceGlobal, OwnerGlobal, filepath.Join(h, filepath.FromSlash(f.rel)), f.nativeTo)
		}
	}
	if home, err := mode.Home(); err == nil {
		for _, f := range mdFiles(filepath.Join(filepath.Dir(home), "rules")) {
			p.addRuleFile(&out, add, "global:", SourceGlobal, OwnerGlobal, f)
		}
	}
	for _, f := range projectNative {
		readFile("project:"+f.rel, f.rel, SourceProject, OwnerProject, filepath.Join(p.Root, f.rel), f.nativeTo)
	}
	for _, f := range mdFiles(p.Path(rulesDir)) {
		p.addRuleFile(&out, add, "project:", SourceProject, OwnerProject, f)
	}
	for _, e := range r.Enabled {
		if e.Kind != KindRule || e.Status == StatusMissing {
			continue
		}
		src := SourcePackage
		if strings.HasPrefix(e.Via, "profile:") {
			src = SourceProfile
		}
		in := Instruction{ID: src + ":" + e.Pkg.Library + "/" + e.Pkg.Name, Name: e.Pkg.Name, Source: src, Owner: OwnerPackage, Path: e.Pkg.Path,
			Revision: short(e.Pkg.Hash), Mandatory: e.Mandatory(), Activation: e.ActivationOf(), Paths: e.PathsOf(), Key: e.Pkg.Meta.Key, Value: e.Pkg.Meta.Value, Text: e.Pkg.Body}
		if e.Override != nil {
			// The override replaces the package's text; the package entry stays
			// listed so its origin and revision remain visible.
			pkgID := in.ID
			in.ShadowedBy = "override:" + e.Pkg.Name
			add(in)
			ov := Instruction{ID: "override:" + e.Pkg.Name, Name: e.Pkg.Name, Source: SourceOverride, Owner: OwnerProject, Path: e.Override.Path,
				Revision: short(e.Override.Hash), Mandatory: e.Mandatory(), Activation: e.ActivationOf(), Paths: e.PathsOf(), Text: e.Override.Body, Overrides: pkgID}
			ov.Key, ov.Value = firstNonEmpty(e.Override.Meta.Key, e.Pkg.Meta.Key), firstNonEmpty(e.Override.Meta.Value, e.Pkg.Meta.Value)
			add(ov)
			continue
		}
		add(in)
	}
	slices.SortStableFunc(out, func(a, b Instruction) int { return a.Rank - b.Rank })
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (p *Project) addRuleFile(out *[]Instruction, add func(Instruction), prefix, src, owner, f string) {
	pk, _ := parsePackage(KindRule, src, f)
	if pk.Name == "" {
		return
	}
	mand := true
	if pk.Meta.Mandatory != nil {
		mand = *pk.Meta.Mandatory
	}
	act := pk.Meta.Activation
	if act == "" {
		act = Auto
	}
	add(Instruction{ID: prefix + pk.Name, Name: pk.Name, Source: src, Owner: owner, Path: f, Revision: short(pk.Hash), Mandatory: mand,
		Activation: act, Paths: pk.Meta.Paths, Key: pk.Meta.Key, Value: pk.Meta.Value, Text: pk.Body})
}

func mdFiles(dir string) []string {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// Applied is an instruction's selection for one execution.
type Applied struct {
	Instruction
	Include bool   `json:"include"` // ULTRA puts its text in the executor's context
	Native  bool   `json:"native"`  // the executor loads it itself; not injected
	Reason  string `json:"reason"`
}

// Conflict is two or more applicable instructions declaring different values
// for the same key. It is a configured check: front matter declares keys.
// ULTRA does not detect contradictions in free text.
type Conflict struct {
	Key      string   `json:"key"`
	IDs      []string `json:"ids"`
	Values   []string `json:"values"`
	Resolved bool     `json:"resolved"`         // one source outranks the rest by precedence
	Winner   string   `json:"winner,omitempty"` // that source
}

// Duplicate is a requirement line found word for word in several sources.
type Duplicate struct {
	Line string   `json:"line"`
	IDs  []string `json:"ids"`
}

// Selection is what applies to one execution.
type Selection struct {
	Applied    []Applied   `json:"applied"`
	Conflicts  []Conflict  `json:"conflicts,omitempty"`
	Duplicates []Duplicate `json:"duplicates,omitempty"`
}

// Unresolved is the conflicts precedence does not settle.
func (s Selection) Unresolved() []Conflict {
	var out []Conflict
	for _, c := range s.Conflicts {
		if !c.Resolved {
			out = append(out, c)
		}
	}
	return out
}

// Apply selects instructions for an execution by executor, task paths (nil:
// not declared) and manual names asked for.
func Apply(ins []Instruction, executor string, paths []string, manual []string) Selection {
	var s Selection
	for _, in := range ins {
		a := Applied{Instruction: in}
		switch {
		case in.ShadowedBy != "":
			a.Reason = "replaced by " + in.ShadowedBy
		case in.Activation == Manual && !slices.Contains(manual, in.Name):
			a.Reason = "manual: not asked for"
		case len(in.Paths) > 0 && paths != nil && !InScope(in.Paths, paths):
			a.Reason = fmt.Sprintf("path scope %s does not match the task", strings.Join(in.Paths, ", "))
		case slices.Contains(in.NativeTo, executor):
			a.Native, a.Reason = true, "loaded natively by "+executor
		case len(in.NativeTo) > 0 && in.Source == SourceGlobal:
			a.Reason = "global file of " + strings.Join(in.NativeTo, ", ") + ": not imported into other executors"
		case !in.Mandatory && in.Activation != Always:
			a.Reason = "optional: listed by name only"
		default:
			a.Include = true
			switch {
			case in.Activation == Manual:
				a.Reason = "asked for by the task"
			case len(in.Paths) > 0 && paths == nil:
				a.Reason = "path-scoped, task paths undeclared: included to be safe"
			case len(in.Paths) > 0:
				a.Reason = "path scope matches the task"
			default:
				a.Reason = "mandatory, project-wide"
			}
		}
		s.Applied = append(s.Applied, a)
	}
	s.Conflicts = conflicts(s.Applied)
	s.Duplicates = duplicates(s.Applied)
	return s
}

func conflicts(as []Applied) []Conflict {
	byKey := map[string][]Applied{}
	var keys []string
	for _, a := range as {
		if (a.Include || a.Native) && a.Key != "" {
			if _, ok := byKey[a.Key]; !ok {
				keys = append(keys, a.Key)
			}
			byKey[a.Key] = append(byKey[a.Key], a)
		}
	}
	var out []Conflict
	for _, k := range keys {
		g := byKey[k]
		vals := map[string]bool{}
		for _, a := range g {
			vals[strings.TrimSpace(strings.ToLower(a.Value))] = true
		}
		if len(vals) < 2 {
			continue
		}
		c := Conflict{Key: k}
		best, tie := g[0], false
		for _, a := range g {
			c.IDs = append(c.IDs, a.ID)
			c.Values = append(c.Values, a.Value)
			if a.Rank < best.Rank {
				best, tie = a, false
			} else if a.Rank == best.Rank && a.ID != best.ID && !strings.EqualFold(a.Value, best.Value) {
				tie = true
			}
		}
		if !tie {
			c.Resolved, c.Winner = true, best.ID
		}
		out = append(out, c)
	}
	return out
}

func normLine(l string) string {
	l = strings.TrimSpace(l)
	l = strings.TrimLeft(l, "-*+>#0123456789. \t")
	return strings.Join(strings.Fields(strings.ToLower(l)), " ")
}

func duplicates(as []Applied) []Duplicate {
	seen := map[string][]string{}
	var order []string
	for _, a := range as {
		if !a.Include && !a.Native {
			continue
		}
		mine := map[string]bool{}
		for l := range strings.SplitSeq(a.Text, "\n") {
			n := normLine(l)
			if len(n) < minDuplicateLine || mine[n] {
				continue
			}
			mine[n] = true
			if _, ok := seen[n]; !ok {
				order = append(order, n)
			}
			seen[n] = append(seen[n], a.ID)
		}
	}
	var out []Duplicate
	for _, n := range order {
		if len(seen[n]) > 1 {
			out = append(out, Duplicate{Line: n, IDs: seen[n]})
		}
	}
	return out
}
