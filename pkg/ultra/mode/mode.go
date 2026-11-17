// Package mode is the boundary between PAI Classic and PAI ULTRA.
//
// Classic is the default: a session or project with no mode recorded is
// Classic, and any failure to read ULTRA's state resolves to Classic, so
// Classic never depends on ULTRA working. A session is pinned to the mode it
// was created in; changing a project's default affects new sessions only.
//
// This package depends only on the standard library and statefile, so the
// Classic TUI can import it without pulling in ULTRA.
package mode

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"pai/pkg/statefile"
	"pai/pkg/ultra/internal/atomicfile"
)

// Mode is how a session runs.
type Mode string

const (
	Classic Mode = "classic"
	Ultra   Mode = "ultra"
)

// Label is the name shown to people.
func (m Mode) Label() string {
	if m == Ultra {
		return "PAI ULTRA"
	}
	return "PAI Classic"
}

// Parse reads a mode a person typed. Unknown text is an error, not Classic:
// a typo must not silently pick a mode.
func Parse(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "classic", "c":
		return Classic, nil
	case "ultra", "u":
		return Ultra, nil
	}
	return "", fmt.Errorf("unknown mode %q: use classic or ultra", s)
}

// Resolve is the mode recorded in a field. Empty (every session made before
// modes existed) and unknown values are Classic.
func Resolve(s string) Mode {
	if Mode(s) == Ultra {
		return Ultra
	}
	return Classic
}

// Of is a session's pinned mode, from its config.
func Of(c statefile.Conf) Mode { return Resolve(c.Mode) }

// Project is a working directory's project: its root (the git top level,
// else the directory itself) and where ULTRA keeps this project's local,
// uncommitted state.
type Project struct {
	Root  string // absolute
	ID    string // stable per root on this machine
	State string // ~/.pai/ultra/projects/<ID>
}

// homeEnv overrides the ULTRA home, for tests and portable installs.
const homeEnv = "PAI_ULTRA_HOME"

// Home is where ULTRA keeps its machine-local state: ~/.pai/ultra.
func Home() (string, error) {
	if h := os.Getenv(homeEnv); h != "" {
		return filepath.Abs(h)
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".pai", "ultra"), nil
}

var unsafeID = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Find is the project of dir.
func Find(dir string) (Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Project{}, err
	}
	root := abs
	if top, err := gitTop(abs); err == nil && top != "" {
		root = top
	}
	home, err := Home()
	if err != nil {
		return Project{}, err
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(root))))
	id := unsafeID.ReplaceAllString(filepath.Base(root), "-") + "-" + hex.EncodeToString(sum[:])[:10]
	return Project{Root: root, ID: id, State: filepath.Join(home, "projects", id)}, nil
}

func gitTop(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.FromSlash(strings.TrimSpace(string(out))))
}

// Link joins a session to the session its work was carried to in the other
// mode. Both stay; neither is changed.
type Link struct {
	From    string    `json:"from"` // "classic:<session>" or "ultra:<task>"
	To      string    `json:"to"`
	Reason  string    `json:"reason,omitempty"`
	Created time.Time `json:"created"`
}

// Prefs is a project's mode state on this machine.
type Prefs struct {
	Version int    `json:"version"`
	Default Mode   `json:"default_mode,omitempty"` // empty is Classic
	Links   []Link `json:"links,omitempty"`
}

const prefsVersion = 1

func (p Project) prefsPath() string { return filepath.Join(p.State, "mode.json") }

// LoadPrefs reads the project's prefs. A missing file is the zero prefs.
func (p Project) LoadPrefs() (Prefs, error) {
	var pr Prefs
	err := atomicfile.ReadJSON(p.prefsPath(), &pr)
	if errors.Is(err, os.ErrNotExist) {
		return Prefs{Version: prefsVersion}, nil
	}
	if err != nil {
		return Prefs{Version: prefsVersion}, fmt.Errorf("read %s: %w", p.prefsPath(), err)
	}
	return pr, nil
}

// Default is the mode new sessions in dir start in. Every failure is Classic,
// with the error for the caller to show.
func Default(dir string) (Mode, error) {
	p, err := Find(dir)
	if err != nil {
		return Classic, err
	}
	pr, err := p.LoadPrefs()
	if err != nil {
		return Classic, err
	}
	return Resolve(string(pr.Default)), nil
}

// SetDefault records m as the project's default for new sessions. Running
// sessions keep the mode they were made in.
func (p Project) SetDefault(m Mode) error {
	pr, err := p.LoadPrefs()
	if err != nil {
		return err
	}
	pr.Version, pr.Default = prefsVersion, m
	return atomicfile.WriteJSON(p.prefsPath(), pr, 0o600)
}

// AddLink records that from's work continues in to.
func (p Project) AddLink(l Link) error {
	pr, err := p.LoadPrefs()
	if err != nil {
		return err
	}
	if l.Created.IsZero() {
		l.Created = time.Now().UTC()
	}
	pr.Version = prefsVersion
	pr.Links = append(pr.Links, l)
	return atomicfile.WriteJSON(p.prefsPath(), pr, 0o600)
}

// LinksOf is every link that names ref at either end.
func (pr Prefs) LinksOf(ref string) []Link {
	var out []Link
	for _, l := range pr.Links {
		if l.From == ref || l.To == ref {
			out = append(out, l)
		}
	}
	return out
}
