package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// historyMax is how many sent messages ~/.pai/history.jsonl keeps, newest
// last: far more than anyone steps back through, and the file stays small.
const historyMax = 500

func historyPath() (string, error) {
	h, err := os.UserHomeDir()
	return filepath.Join(h, ".pai", "history.jsonl"), err
}

// History is the messages sent from this machine's TUI, oldest first, for Up
// in the input. It is read once; a missing file is none.
func (a *App) History() []string {
	if a.history != nil {
		return a.history
	}
	a.history = []string{}
	path, err := historyPath()
	if err != nil {
		return a.history
	}
	f, err := os.Open(path)
	if err != nil {
		return a.history
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24) // a line is a message, which a paste can make long
	for sc.Scan() {
		var t string
		if json.Unmarshal(sc.Bytes(), &t) == nil {
			a.history = append(a.history, t)
		}
	}
	return a.history
}

// Remember adds a sent message to the history, unless it repeats the newest,
// and keeps the file to historyMax. The file is this user's only: messages
// can hold what is private.
func (a *App) Remember(text string) {
	h := a.History()
	if strings.TrimSpace(text) == "" || len(h) > 0 && h[len(h)-1] == text {
		return
	}
	a.history = append(h, text)
	if len(a.history) > historyMax {
		a.history = a.history[len(a.history)-historyMax:]
	}
	path, err := historyPath()
	if err != nil {
		return
	}
	var b strings.Builder
	for _, t := range a.history {
		line, _ := json.Marshal(t)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = os.WriteFile(path, []byte(b.String()), 0o600)
	}
}
