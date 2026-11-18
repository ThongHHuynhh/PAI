package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"
)

// Claude drives Claude Code through its supported print mode:
// `claude -p --output-format stream-json --verbose`, the prompt on stdin.
// It resumes a native session with --resume. Claude Code keeps its own
// tools, permissions and CLAUDE.md loading.
type Claude struct {
	Bin string // "" is claude on PATH
}

func (c Claude) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "claude"
}

// Capabilities reports what this integration supports.
func (Claude) Capabilities() Capabilities {
	return Capabilities{
		Name: "claude", NativeSession: true, Resume: true, Cancel: true, Events: true, ReportsUsage: true,
		Tools:     []string{"shell", "files", "git", "network"},
		AutoLoads: []string{"CLAUDE.md", "CLAUDE.local.md", "~/.claude/CLAUDE.md", "~/.claude/skills", ".claude/skills"},
		Notes: []string{
			"PAI does not intercept Claude Code's tool calls: its own permission settings apply",
			"Claude Code does not report which CLAUDE.md files it loaded: their status is unknown",
			"usage is Claude Code's own report for the run",
		},
	}
}

// Available checks the CLI is installed.
func (c Claude) Available(ctx context.Context) error {
	if _, err := osexec.LookPath(c.bin()); err != nil {
		return fmt.Errorf("claude CLI not found: %w", err)
	}
	return nil
}

// Start runs one execution.
func (c Claude) Start(ctx context.Context, a Assignment) (Run, error) {
	if a.Workdir == "" {
		return nil, errors.New("claude: no working directory")
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Resume != "" {
		args = append(args, "--resume", a.Resume)
	}
	if m := a.Permissions.ClaudePermissionMode; m != "" {
		args = append(args, "--permission-mode", m)
	}
	if len(a.Permissions.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, a.Permissions.AllowedTools...)
	}
	var final string
	parse := func(line []byte, r *Result, emit func(Event)) {
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Session string `json:"session_id"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
			Usage struct {
				Input       int `json:"input_tokens"`
				Output      int `json:"output_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if ev.Session != "" {
			r.NativeSession = ev.Session
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					emit(Event{Kind: "text", Text: c.Text})
				case "tool_use":
					emit(Event{Kind: "tool", Text: c.Name})
				}
			}
		case "result":
			final = ev.Result
			r.Usage = Usage{Input: ev.Usage.Input + ev.Usage.CacheCreate, Output: ev.Usage.Output, CacheRead: ev.Usage.CacheRead, Reported: true}
			emit(Event{Kind: "usage", Text: fmt.Sprintf("%d tokens", r.Usage.Total())})
			finish(r, final)
			if ev.IsError {
				r.Status, r.Err = "failed", firstLine(ev.Result)
			}
		}
	}
	return startProcess(ctx, a.TaskID, a.Workdir, c.bin(), args, a.Prompt, a.Timeout, parse)
}
