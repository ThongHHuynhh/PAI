// Package project is a PAI ULTRA project's configuration: the manifest a
// person edits (.pai/project.yaml), the lockfile that pins exact content
// (.pai/lock.json), the libraries skills and rules come from, and the
// instructions that apply to a task.
//
// Three things are kept apart:
//   - installed: a package exists in a library on this machine;
//   - enabled: the manifest selects it for this project (directly or by profile);
//   - selected/loaded: decided per execution (see Applicable and pkg/ultra/ctxsel).
package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	// Dir is the project's committed ULTRA directory.
	Dir          = ".pai"
	ManifestFile = "project.yaml"
	LockFile     = "lock.json"
	// ManifestVersion and LockVersion are the schema versions this build writes.
	ManifestVersion = 1
	LockVersion     = 1
	// Default soft context budgets, in estimated tokens. Mandatory items may
	// exceed them; the overrun is reported, never truncated.
	DefaultBootstrapTokens = 1000
	DefaultResumeTokens    = 2000
	DefaultMaxAttempts     = 3
)

// Activation says when a package is selected for an execution.
type Activation string

const (
	// Auto: a skill's name and description are offered; the executor reads
	// the full SKILL.md only when it needs it. A rule with Auto applies when
	// its path scope matches.
	Auto Activation = "auto"
	// Always: the full text is included in every execution.
	Always Activation = "always"
	// Manual: only when a task asks for it by name.
	Manual Activation = "manual"
)

func (a Activation) valid() bool { return a == "" || a == Auto || a == Always || a == Manual }

// Entry selects one package from a library.
type Entry struct {
	Name       string     `yaml:"name"`
	Source     string     `yaml:"source,omitempty"`  // a library's name; required when two libraries have the name
	Version    string     `yaml:"version,omitempty"` // a version the locked content must declare, when set
	Activation Activation `yaml:"activation,omitempty"`
	Paths      []string   `yaml:"paths,omitempty"`     // path scope, as slash globs relative to the root; ** spans directories
	Mandatory  *bool      `yaml:"mandatory,omitempty"` // rules only: overrides the rule's own default
	Disabled   bool       `yaml:"disabled,omitempty"`  // switches off an entry a profile brought in
}

// Library is a directory of packages: skills/<name>/SKILL.md, rules/<name>.md
// and profiles/<name>.yaml.
type Library struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"` // absolute, ~/..., or relative to the project root
}

// ContextConf holds the soft budgets for selected context, in estimated tokens.
type ContextConf struct {
	BootstrapTokens int `yaml:"bootstrap_tokens,omitempty"`
	ResumeTokens    int `yaml:"resume_tokens,omitempty"`
}

// MemoryConf switches ULTRA's background memory work. All of it is off by
// default: the lightweight profile makes no model calls of its own.
type MemoryConf struct {
	Extraction    bool `yaml:"extraction,omitempty"`
	Consolidation bool `yaml:"consolidation,omitempty"`
	ModelReview   bool `yaml:"model_review,omitempty"` // allow a model to judge "review" criteria
}

// ExecutorConf picks the default executor and holds per-adapter settings a
// person chose. Skills and rules never set these: they cannot widen what an
// executor may do.
type ExecutorConf struct {
	Default     string            `yaml:"default,omitempty"` // pai, claude or codex
	Model       string            `yaml:"model,omitempty"`
	MaxAttempts int               `yaml:"max_attempts,omitempty"`
	Settings    map[string]string `yaml:"settings,omitempty"` // e.g. claude.permission_mode, codex.sandbox
}

// Criterion is a default acceptance criterion added to every new task.
type Criterion struct {
	ID      string `yaml:"id" json:"id"`
	Text    string `yaml:"text,omitempty" json:"text,omitempty"`
	Command string `yaml:"command,omitempty" json:"command,omitempty"`
}

// Manifest is .pai/project.yaml.
type Manifest struct {
	Version    int          `yaml:"version"`
	Profiles   []string     `yaml:"profiles,omitempty"`
	Libraries  []Library    `yaml:"libraries,omitempty"`
	Skills     []Entry      `yaml:"skills,omitempty"`
	Rules      []Entry      `yaml:"rules,omitempty"`
	Precedence []string     `yaml:"precedence,omitempty"`
	Context    ContextConf  `yaml:"context,omitempty"`
	Memory     MemoryConf   `yaml:"memory,omitempty"`
	Executor   ExecutorConf `yaml:"executor,omitempty"`
	Criteria   []Criterion  `yaml:"criteria,omitempty"`
}

// DefaultPrecedence orders instruction sources, highest first.
var DefaultPrecedence = []string{SourceOverride, SourceProject, SourceProfile, SourcePackage, SourceGlobal}

// Instruction sources.
const (
	SourceOverride = "override" // .pai/overrides: a project's edit of a package rule
	SourceProject  = "project"  // .pai/rules and the project's AGENTS.md, CLAUDE.md
	SourceProfile  = "profile"  // a rule a profile brought in
	SourcePackage  = "package"  // a rule enabled from a library
	SourceGlobal   = "global"   // ~/.claude/CLAUDE.md, ~/.codex/AGENTS.md, ~/.pai/rules
)

// Budgets returns the context budgets with defaults filled.
func (m Manifest) Budgets() (bootstrap, resume int) {
	bootstrap, resume = m.Context.BootstrapTokens, m.Context.ResumeTokens
	if bootstrap <= 0 {
		bootstrap = DefaultBootstrapTokens
	}
	if resume <= 0 {
		resume = DefaultResumeTokens
	}
	return
}

// Attempts is the bounded number of executor attempts per task.
func (m Manifest) Attempts() int {
	if m.Executor.MaxAttempts > 0 {
		return m.Executor.MaxAttempts
	}
	return DefaultMaxAttempts
}

// Order is the precedence in effect.
func (m Manifest) Order() []string {
	if len(m.Precedence) > 0 {
		return m.Precedence
	}
	return DefaultPrecedence
}

// ParseManifest reads a manifest strictly: unknown keys are errors, so a typo
// cannot silently drop a requirement.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) { // empty is the zero manifest
		return Manifest{}, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if m.Version == 0 {
		m.Version = ManifestVersion
	}
	if m.Version > ManifestVersion {
		return Manifest{}, fmt.Errorf("%s: version %d is newer than this PAI understands (%d)", ManifestFile, m.Version, ManifestVersion)
	}
	return m, nil
}

const manifestHeader = "# PAI ULTRA project manifest. Edited by `pai ultra` and the ULTRA TUI;\n# comments are not kept when PAI rewrites it. See docs/ultra.md.\n"

// Marshal writes the manifest as YAML.
func (m Manifest) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(manifestHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// Validate checks what can be checked without libraries.
func (m Manifest) Validate() []Problem {
	var ps []Problem
	check := func(kind string, es []Entry) {
		seen := map[string]bool{}
		for _, e := range es {
			key := e.Source + "/" + e.Name
			if e.Name == "" {
				ps = append(ps, errorf(kind, "", "an entry has no name"))
				continue
			}
			if seen[key] {
				ps = append(ps, errorf(kind, e.Name, "listed twice"))
			}
			seen[key] = true
			if !e.Activation.valid() {
				ps = append(ps, errorf(kind, e.Name, "activation %q is not auto, always or manual", e.Activation))
			}
			for _, g := range e.Paths {
				if err := checkGlob(g); err != nil {
					ps = append(ps, errorf(kind, e.Name, "path %q: %v", g, err))
				}
			}
		}
	}
	check(KindSkill, m.Skills)
	check(KindRule, m.Rules)
	for _, p := range m.Precedence {
		if !slices.Contains(DefaultPrecedence, p) {
			ps = append(ps, errorf("manifest", "precedence", "unknown source %q", p))
		}
	}
	if len(m.Precedence) > 0 && len(m.Precedence) != len(DefaultPrecedence) {
		ps = append(ps, errorf("manifest", "precedence", "must list all of %s", strings.Join(DefaultPrecedence, ", ")))
	}
	switch m.Executor.Default {
	case "", "pai", "claude", "codex":
	default:
		ps = append(ps, errorf("manifest", "executor", "unknown executor %q", m.Executor.Default))
	}
	for _, l := range m.Libraries {
		if l.Name == "" || l.Path == "" {
			ps = append(ps, errorf("manifest", "libraries", "a library needs a name and a path"))
		}
	}
	return ps
}

// LockedFile is one file of a locked package.
type LockedFile struct {
	Path string `json:"path"` // slash path inside the package
	Hash string `json:"sha256"`
	Size int64  `json:"size"`
}

// Locked pins one enabled package to exact content.
type Locked struct {
	Kind     string       `json:"kind"`
	Name     string       `json:"name"`
	Source   string       `json:"source"`
	Version  string       `json:"version,omitempty"` // what the content declares; display only
	Hash     string       `json:"sha256"`
	Override string       `json:"override_sha256,omitempty"` // the project override in force, when any
	Files    []LockedFile `json:"files"`
}

// Lock is .pai/lock.json. Revision counts applied changes: an execution pins
// it, and a later apply makes a newer one.
type Lock struct {
	Version      int       `json:"version"`
	Revision     int       `json:"revision"`
	ManifestHash string    `json:"manifest_sha256"`
	Generated    time.Time `json:"generated"`
	Packages     []Locked  `json:"packages"`
}

// Find is the locked entry for kind/name.
func (l Lock) Find(kind, name string) (Locked, bool) {
	for _, p := range l.Packages {
		if p.Kind == kind && p.Name == name {
			return p, true
		}
	}
	return Locked{}, false
}

// RevisionLabel is how a revision is shown: r0 before any apply.
func RevisionLabel(n int) string { return fmt.Sprintf("r%d", n) }

func hashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// expandPath resolves ~ and project-relative paths.
func expandPath(root, p string) string {
	if rest, ok := strings.CutPrefix(p, "~"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, filepath.FromSlash(p))
}
