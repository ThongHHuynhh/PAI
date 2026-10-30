package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// prefs is what this person chose in the TUI, kept in ~/.pai/tui.json on
// this machine: the theme's id, or none to follow the terminal.
type prefs struct {
	Theme string `json:"theme,omitempty"`
}

func prefsPath() (string, error) {
	h, err := os.UserHomeDir()
	return filepath.Join(h, ".pai", "tui.json"), err
}

// loadPrefs is the saved prefs; a missing or unreadable file is none.
func loadPrefs() prefs {
	var p prefs
	path, err := prefsPath()
	if err != nil {
		return p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	_ = json.Unmarshal(b, &p)
	return p
}

func savePrefs(p prefs) error {
	path, err := prefsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
