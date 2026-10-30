//go:build !windows

package term

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
)

// clipboardTools are the commands that print the clipboard, tried in order:
// macOS's own, then Wayland's, then X11's two.
func clipboardTools() [][]string {
	if runtime.GOOS == "darwin" {
		return [][]string{{"pbpaste"}}
	}
	var tools [][]string
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		tools = append(tools, []string{"wl-paste", "--no-newline"})
	}
	return append(tools, []string{"xclip", "-selection", "clipboard", "-o"}, []string{"xsel", "--clipboard", "--output"})
}

// Clipboard is the text on the system clipboard. A terminal pastes on its own
// with Cmd+V or Ctrl+Shift+V; this serves Ctrl+V, which it passes through.
func Clipboard() (string, error) {
	for _, t := range clipboardTools() {
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		out, err := exec.Command(t[0], t[1:]...).Output()
		return string(out), err
	}
	return "", errors.New("no clipboard tool found: paste with your terminal, or install wl-clipboard or xclip")
}
