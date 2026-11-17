package project

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"pai/pkg/contextflow"
)

// Package kinds.
const (
	KindSkill   = "skill"
	KindRule    = "rule"
	KindProfile = "profile"
)

const (
	skillFile = "SKILL.md"
	// maxPackageBytes bounds what one package may hold: skills are meant to
	// be small, and the cache copies them.
	maxPackageBytes = 4 << 20
	// skillNameMax and skillDescMax are the Agent Skills format's limits.
	skillNameMax = 64
	skillDescMax = 1024
)

// Problem is something wrong with configuration. Errors block an apply;
// warnings are shown and allowed.
type Problem struct {
	Severity string `json:"severity"` // error or warning
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Message  string `json:"message"`
}

func (p Problem) String() string {
	return fmt.Sprintf("%s: %s %s: %s", p.Severity, p.Kind, p.Name, p.Message)
}

func errorf(kind, name, format string, a ...any) Problem {
	return Problem{"error", kind, name, fmt.Sprintf(format, a...)}
}
func warnf(kind, name, format string, a ...any) Problem {
	return Problem{"warning", kind, name, fmt.Sprintf(format, a...)}
}

// Blocking is whether any problem is an error.
func Blocking(ps []Problem) bool {
	return slices.ContainsFunc(ps, func(p Problem) bool { return p.Severity == "error" })
}

// Meta is a package's front matter. Skills follow the Agent Skills SKILL.md
// format (name, description, license, compatibility, metadata,
// allowed-tools); PAI reads version, tags and requires from metadata. Rules
// use PAI's own fields.
type Meta struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
	AllowedTools  string            `yaml:"allowed-tools,omitempty"` // recorded, never used to widen permissions
	// Rule fields.
	Version    string     `yaml:"version,omitempty"`
	Tags       []string   `yaml:"tags,omitempty"`
	Mandatory  *bool      `yaml:"mandatory,omitempty"`
	Activation Activation `yaml:"activation,omitempty"`
	Paths      []string   `yaml:"paths,omitempty"`
	Key        string     `yaml:"key,omitempty"` // declared conflicts: same key, different value
	Value      string     `yaml:"value,omitempty"`
	// Profile fields.
	Skills []Entry `yaml:"skills,omitempty"`
	Rules  []Entry `yaml:"rules,omitempty"`
}

// VersionOf is the declared version: a rule's version, else a skill's metadata.version.
func (m Meta) VersionOf() string {
	if m.Version != "" {
		return m.Version
	}
	return m.Metadata["version"]
}

// TagsOf is the declared tags.
func (m Meta) TagsOf() []string {
	if len(m.Tags) > 0 {
		return m.Tags
	}
	return splitList(m.Metadata["tags"])
}

// Requires is the capabilities a skill declares it needs (metadata.requires).
func (m Meta) Requires() []string { return splitList(m.Metadata["requires"]) }

func splitList(s string) []string {
	var out []string
	for f := range strings.FieldsFuncSeq(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, f)
	}
	return out
}

// Package is one installed skill, rule or profile.
type Package struct {
	Kind    string
	Name    string
	Library string // the library's name
	Path    string // the SKILL.md, rule or profile file
	Dir     string // the skill's directory; the file's directory otherwise
	Meta    Meta
	Body    string // the Markdown after the front matter
	Raw     []byte // the whole file
	Hash    string // over every file of the package
	Files   []LockedFile
	Tokens  int // estimated tokens of the full file
}

// Resources is the files of a skill besides SKILL.md.
func (p Package) Resources() []string {
	var out []string
	for _, f := range p.Files {
		if f.Path != skillFile {
			out = append(out, f.Path)
		}
	}
	return out
}

// EstimateTokens is a planning estimate, not provider accounting.
func EstimateTokens(s string) int {
	return (len(s) + contextflow.BytesPerToken - 1) / contextflow.BytesPerToken
}

// splitFrontMatter returns the YAML between leading --- lines, and the rest.
func splitFrontMatter(b []byte) ([]byte, string, bool) {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, s, false
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, s, false
	}
	body := rest[end+4:]
	body = strings.TrimPrefix(body, "\n")
	return []byte(rest[:end]), body, true
}

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// parsePackage reads one package file.
func parsePackage(kind, lib, file string) (Package, []Problem) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Package{}, []Problem{errorf(kind, file, "%v", err)}
	}
	return parseRaw(kind, lib, file, raw)
}

// parseRaw parses a package file's content; skills also hash their directory.
func parseRaw(kind, lib, file string, raw []byte) (Package, []Problem) {
	p := Package{Kind: kind, Library: lib, Path: file, Dir: filepath.Dir(file), Raw: raw}
	var ps []Problem
	switch kind {
	case KindProfile:
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&p.Meta); err != nil {
			return p, []Problem{errorf(kind, file, "%v", err)}
		}
		p.Name = p.Meta.Name
		if p.Name == "" {
			p.Name = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		}
	default:
		fm, body, ok := splitFrontMatter(raw)
		p.Body = body
		if ok {
			if err := yaml.Unmarshal(fm, &p.Meta); err != nil {
				ps = append(ps, errorf(kind, file, "front matter: %v", err))
			}
		} else if kind == KindSkill {
			ps = append(ps, errorf(kind, file, "SKILL.md needs YAML front matter with name and description"))
		}
		p.Name = p.Meta.Name
		if kind == KindRule && p.Name == "" {
			p.Name = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		}
	}
	if kind == KindSkill {
		dir := filepath.Base(p.Dir)
		switch {
		case p.Name == "":
			ps = append(ps, errorf(kind, dir, "name is required"))
		case len(p.Name) > skillNameMax || !skillName.MatchString(p.Name):
			ps = append(ps, errorf(kind, p.Name, "name must be lowercase letters, digits and single hyphens, at most %d", skillNameMax))
		case p.Name != dir:
			ps = append(ps, warnf(kind, p.Name, "name differs from its directory %q", dir))
		}
		if p.Meta.Description == "" {
			ps = append(ps, errorf(kind, p.Name, "description is required"))
		} else if len(p.Meta.Description) > skillDescMax {
			ps = append(ps, warnf(kind, p.Name, "description is longer than %d characters", skillDescMax))
		}
		if p.Meta.AllowedTools != "" {
			ps = append(ps, warnf(kind, p.Name, "allowed-tools is recorded but never widens executor permissions"))
		}
	}
	if kind == KindRule && !p.Meta.Activation.valid() {
		ps = append(ps, errorf(kind, p.Name, "activation %q is not auto, always or manual", p.Meta.Activation))
	}
	for _, g := range p.Meta.Paths {
		if err := checkGlob(g); err != nil {
			ps = append(ps, errorf(kind, p.Name, "path %q: %v", g, err))
		}
	}
	p.Tokens = EstimateTokens(string(raw))
	var herr error
	if kind == KindSkill {
		p.Files, p.Hash, herr = hashDir(p.Dir)
	} else {
		p.Files = []LockedFile{{Path: filepath.Base(file), Hash: hashBytes(raw), Size: int64(len(raw))}}
		p.Hash = hashFiles(p.Files)
	}
	if herr != nil {
		ps = append(ps, errorf(kind, p.Name, "%v", herr))
	}
	return p, ps
}

// hashDir hashes every regular file under dir, skipping dot files.
func hashDir(dir string) ([]LockedFile, string, error) {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real // walk a linked skill directory, not the link
	}
	var files []LockedFile
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		total += int64(len(b))
		if total > maxPackageBytes {
			return fmt.Errorf("package is larger than %d bytes", maxPackageBytes)
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, LockedFile{Path: filepath.ToSlash(rel), Hash: hashBytes(b), Size: int64(len(b))})
		return nil
	})
	slices.SortFunc(files, func(a, b LockedFile) int { return strings.Compare(a.Path, b.Path) })
	return files, hashFiles(files), err
}

// hashFiles is one hash over names and contents.
func hashFiles(fs []LockedFile) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Path)
		b.WriteByte(0)
		b.WriteString(f.Hash)
		b.WriteByte('\n')
	}
	return hashBytes([]byte(b.String()))
}

// Scan reads every package in a library. A missing library is empty.
func Scan(lib Library, root string) ([]Package, []Problem) {
	dir := expandPath(root, lib.Path)
	var pkgs []Package
	var ps []Problem
	add := func(p Package, more []Problem) {
		ps = append(ps, more...)
		if p.Name != "" {
			pkgs = append(pkgs, p)
		}
	}
	if es, err := os.ReadDir(filepath.Join(dir, "skills")); err == nil {
		for _, e := range es {
			f := filepath.Join(dir, "skills", e.Name(), skillFile)
			if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() { // a linked skill directory counts
				add(parsePackage(KindSkill, lib.Name, f))
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		ps = append(ps, warnf("library", lib.Name, "%v", err))
	}
	for _, sub := range []struct{ kind, dir, ext string }{{KindRule, "rules", ".md"}, {KindProfile, "profiles", ".yaml"}} {
		es, err := os.ReadDir(filepath.Join(dir, sub.dir))
		if err != nil {
			continue
		}
		for _, e := range es {
			if !e.IsDir() && strings.HasSuffix(e.Name(), sub.ext) {
				add(parsePackage(sub.kind, lib.Name, filepath.Join(dir, sub.dir, e.Name())))
			}
		}
	}
	return pkgs, ps
}

// checkGlob rejects patterns path.Match cannot read and absolute or parent paths.
func checkGlob(g string) error {
	if g == "" || strings.HasPrefix(g, "/") || strings.Contains(g, "..") || strings.Contains(g, `\`) {
		return errors.New("use a slash path relative to the project root")
	}
	for seg := range strings.SplitSeq(g, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// MatchGlob reports whether the slash path p matches g, where ** spans any
// number of directories.
func MatchGlob(g, p string) bool {
	return matchSegs(strings.Split(g, "/"), strings.Split(strings.Trim(p, "/"), "/"))
}

func matchSegs(g, p []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			for i := 0; i <= len(p); i++ {
				if matchSegs(g[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, _ := path.Match(g[0], p[0]); !ok {
			return false
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0
}

// InScope is whether any of paths falls under any of globs. No globs is the
// whole project.
func InScope(globs, paths []string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, p := range paths {
		for _, g := range globs {
			if MatchGlob(g, filepath.ToSlash(p)) {
				return true
			}
		}
	}
	return false
}
