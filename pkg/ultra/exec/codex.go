package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"
)

// Codex drives the Codex CLI through its supported non-interactive mode:
// `codex exec --json`, the prompt on stdin (`-`). It resumes a native
// session with `codex exec resume <id>`. Codex keeps its own sandbox,
// approvals and AGENTS.md loading.
type Codex struct {
	Bin string // "" is codex on PATH
}

func (c Codex) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "codex"
}

// Capabilities reports what this integration supports.
func (Codex) Capabilities() Capabilities {
	return Capabilities{
		Name: "codex", NativeSession: true, Resume: true, Cancel: true, Events: true, ReportsUsage: true,
		Tools:     []string{"shell", "files", "git"},
		AutoLoads: []string{"AGENTS.md", "~/.codex/AGENTS.md"},
		Notes: []string{
			"PAI does not intercept Codex's tool calls: its sandbox and approval settings apply",
			"network access depends on the Codex sandbox chosen in the project manifest",
			"Codex does not report which AGENTS.md files it loaded: their status is unknown",
		},
	}
}

// Available checks the CLI is installed.
func (c Codex) Available(ctx context.Context) error {
	if _, err := osexec.LookPath(c.bin()); err != nil {
		return fmt.Errorf("codex CLI not found: %w", err)
	}
	return nil
}

// Start runs one execution.
func (c Codex) Start(ctx context.Context, a Assignment) (Run, error) {
	if a.Workdir == "" {
		return nil, errors.New("codex: no working directory")
	}
	args := []string{"exec"}
	if a.Resume != "" {
		args = append(args, "resume", a.Resume)
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if a.Resume == "" {
		args = append(args, "-C", a.Workdir) // a resumed session keeps its own directory
	}
	if a.Model != "" {
		args = append(args, "-m", a.Model)
	}
	if s := a.Permissions.CodexSandbox; s != "" {
		if a.Resume != "" {
			args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", s)) // resume takes no -s
		} else {
			args = append(args, "-s", s)
		}
	}
	args = append(args, "-")
	var final string
	parse := func(line []byte, r *Result, emit func(Event)) {
		var ev struct {
			Type   string `json:"type"`
			Thread string `json:"thread_id"`
			Item   struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Command string `json:"command"`
			} `json:"item"`
			Usage struct {
				Input  int `json:"input_tokens"`
				Cached int `json:"cached_input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case "thread.started":
			r.NativeSession = ev.Thread
		case "item.completed":
			switch ev.Item.Type {
			case "agent_message":
				final = ev.Item.Text
				emit(Event{Kind: "text", Text: ev.Item.Text})
			case "command_execution":
				emit(Event{Kind: "tool", Text: ev.Item.Command})
			}
		case "turn.completed":
			// Codex reports input including cached tokens; split them out.
			r.Usage.Input += ev.Usage.Input - ev.Usage.Cached
			r.Usage.CacheRead += ev.Usage.Cached
			r.Usage.Output += ev.Usage.Output
			r.Usage.Reported = true
			emit(Event{Kind: "usage", Text: fmt.Sprintf("%d tokens", r.Usage.Total())})
			finish(r, final)
		case "turn.failed", "error":
			msg := ev.Error.Message
			if msg == "" {
				msg = ev.Message
			}
			r.Status, r.Err = "failed", msg
			emit(Event{Kind: "error", Text: msg})
		}
	}
	return startProcess(ctx, a.TaskID, a.Workdir, c.bin(), args, a.Prompt, a.Timeout, parse)
}
