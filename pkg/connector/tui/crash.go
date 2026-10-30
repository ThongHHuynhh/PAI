package tui

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	xterm "golang.org/x/term"

	"pai/pkg/connector/tui/term"
	"pai/pkg/version"
)

// childEnv marks the process that is the TUI under Supervise.
const childEnv = "PAI_TUI_CHILD"

// crashed finds the start of a Go crash in stderr, or the hang that
// Terminal.hung writes in the same shape.
var crashed = regexp.MustCompile(`(?m)^(panic|fatal error|hang): `)

// released is the line each child writes first to stderr. After an update the
// TUI that crashes is a newer release than the parent; the last line names it.
var released = regexp.MustCompile(`(?m)^pai-tui-version: (.*)\n`)

// Supervise runs this binary again with the same arguments as the TUI, and
// waits. Only a parent survives every way the TUI can die, a fatal error and
// a hang included: when the child crashes, the parent restores the terminal,
// saves the report to ~/.pai/crash, and asks to send it to
// bugs.pai.ai. It returns only in the child.
func Supervise() {
	if os.Getenv(childEnv) != "" {
		debug.SetTraceback("all") // a crash shows every goroutine, the loop's too
		fmt.Fprintf(os.Stderr, "pai-tui-version: %s\n", version.V)
		return
	}
	restore, err := term.Guard()
	if err != nil {
		log.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	var stderr bytes.Buffer // held until the end: a write would land on the TUI's screen
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, &stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt) // the child gets Ctrl+C too, and decides
	err = cmd.Run()
	signal.Stop(sig)
	if cmd.ProcessState == nil {
		log.Fatal(err)
	}
	code, out, v := cmd.ProcessState.ExitCode(), stderr.String(), version.V
	if m := released.FindAllStringSubmatch(out, -1); m != nil {
		v = m[len(m)-1][1]
	}
	out = released.ReplaceAllString(out, "")
	if code != 0 && code != 1 { // 0 and 1 are the child's own exits, after it gave the terminal back
		restore()
	}
	i := crashed.FindStringIndex(out)
	if code != 2 || i == nil {
		fmt.Fprint(os.Stderr, out)
		os.Exit(code)
	}
	fmt.Fprint(os.Stderr, out[:i[0]])
	offer(v, crashReport(v, out[i[0]:]))
	os.Exit(2)
}

// crashReport is what a crash report holds: the release, the platform, the
// terminal, and the trace. Nothing of the session.
func crashReport(v, trace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PAI TUI crash\nversion: %s\nos: %s/%s\n", v, runtime.GOOS, runtime.GOARCH)
	if w, h, err := xterm.GetSize(int(os.Stdout.Fd())); err == nil {
		fmt.Fprintf(&b, "size: %dx%d\n", w, h)
	}
	for _, k := range []string{"TERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "COLORTERM"} {
		if v := os.Getenv(k); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	for _, k := range []string{"WT_SESSION", "TMUX", "SSH_TTY"} {
		if os.Getenv(k) != "" {
			fmt.Fprintf(&b, "%s: set\n", k)
		}
	}
	b.WriteString("\n" + trace)
	return b.String()
}

// offer saves the report and shows its start. Nothing leaves the machine.
func offer(_, r string) {
	lines := strings.SplitAfter(r, "\n")
	fmt.Fprint(os.Stderr, strings.Join(lines[:min(len(lines), 30)], ""))
	path, err := saveCrash(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nThe PAI TUI crashed. Saving the report failed: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "\nThe PAI TUI crashed. The whole report is in %s\n", path)
}

func saveCrash(r string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(h, ".pai", "crash")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.txt", time.Now().UTC().Format("2006-01-02T150405Z"), os.Getpid()))
	return path, os.WriteFile(path, []byte(r), 0o600)
}
