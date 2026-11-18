// Package exec is PAI ULTRA's execution boundary: one interface over the
// existing PAI agent loop and native coding agents (Claude Code, Codex).
//
// PAI owns the task contract; an executor keeps its own reasoning, tools and
// session format. An adapter says what it can and cannot do through
// Capabilities, and an unsupported capability is reported, never faked.
// Native session ids are optional metadata: PAI's portable checkpoint works
// without them.
package exec

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Capabilities is what an adapter supports. Notes say, in plain words, what
// it cannot do.
type Capabilities struct {
	Name          string   `json:"name"`
	NativeSession bool     `json:"native_session"` // has its own session id
	Resume        bool     `json:"resume"`         // can continue a native session
	Cancel        bool     `json:"cancel"`
	Events        bool     `json:"events"` // streams progress
	ToolIntercept bool     `json:"tool_intercept"`
	ReportsUsage  bool     `json:"reports_usage"`  // provider-reported tokens
	ReportsLoaded bool     `json:"reports_loaded"` // can say which instruction files it loaded
	Tools         []string `json:"tools"`          // shell, files, git, network, browser
	AutoLoads     []string `json:"auto_loads"`     // instruction files it reads on its own
	Remote        bool     `json:"remote"`         // needs a PAI server
	Notes         []string `json:"notes,omitempty"`
}

// Has is whether the adapter offers tool t.
func (c Capabilities) Has(t string) bool {
	for _, x := range c.Tools {
		if x == t {
			return true
		}
	}
	return false
}

// Permissions are what a person allowed for this project's executors. Only
// the project manifest's executor section sets them; skills, rules, memory
// and handoffs cannot.
type Permissions struct {
	ClaudePermissionMode string // passed as --permission-mode when set
	CodexSandbox         string // passed as --sandbox when set
	AllowedTools         []string
}

// Assignment is one bounded piece of work for an executor.
type Assignment struct {
	TaskID      string
	Workdir     string
	Prompt      string // the composed context: contract, rules, skills, checkpoint
	Model       string
	Resume      string // a native session to continue, when supported
	Permissions Permissions
	Timeout     time.Duration
}

// Usage is token use. Reported says the provider reported it; otherwise it
// is PAI's estimate.
type Usage struct {
	Input     int  `json:"input"`
	Output    int  `json:"output"`
	CacheRead int  `json:"cache_read,omitempty"`
	Reported  bool `json:"reported"`
}

// Total is all tokens.
func (u Usage) Total() int { return u.Input + u.Output + u.CacheRead }

// Event is progress from a running executor.
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // start, text, tool, usage, error, done
	Text string    `json:"text,omitempty"`
}

// Result is the executor's outcome. Status is what the executor claims; the
// coordinator decides acceptance separately.
type Result struct {
	Status        string        `json:"status"` // done, blocked, failed, cancelled, unknown
	Summary       string        `json:"summary,omitempty"`
	Changes       []string      `json:"changes,omitempty"`
	Evidence      []string      `json:"evidence,omitempty"`
	Open          []string      `json:"open,omitempty"`
	NativeSession string        `json:"native_session,omitempty"`
	Usage         Usage         `json:"usage"`
	Duration      time.Duration `json:"duration_ns"`
	Final         string        `json:"final,omitempty"` // the executor's last text, bounded
	Err           string        `json:"error,omitempty"`
}

// Run is one started execution.
type Run interface {
	ID() string
	Events() <-chan Event // closed when the run ends
	Wait() Result
	Cancel() error
}

// Adapter starts executions.
type Adapter interface {
	Capabilities() Capabilities
	// Available says whether the adapter can run here now, and why not.
	Available(ctx context.Context) error
	Start(ctx context.Context, a Assignment) (Run, error)
}

// ErrUnsupported marks an operation an adapter cannot do.
var ErrUnsupported = errors.New("not supported by this executor")

// ResultProtocol is appended to every prompt: how to end a run so PAI can
// read the claim. The claim is checked, not trusted.
const ResultProtocol = "When you stop, end your reply with one fenced ```pai-result block holding JSON: " +
	`{"status":"done|blocked|failed","summary":"...","changes":["files"],"evidence":["commands run and results"],"open":["unresolved issues"]}. ` +
	"PAI checks the acceptance criteria itself; do not change them."

var resultBlock = regexp.MustCompile("(?s)```pai-result\\s*\\n(.*?)\\n```")

// maxFinal bounds the executor text a result keeps.
const maxFinal = 4000

// ParseResult reads the executor's pai-result block from its final text. A
// missing or malformed block is status "unknown": validation still runs.
func ParseResult(final string) Result {
	r := Result{Status: "unknown", Final: tail(final, maxFinal)}
	ms := resultBlock.FindAllStringSubmatch(final, -1)
	if len(ms) == 0 {
		return r
	}
	var claim struct {
		Status   string   `json:"status"`
		Summary  string   `json:"summary"`
		Changes  []string `json:"changes"`
		Evidence []string `json:"evidence"`
		Open     []string `json:"open"`
	}
	if json.Unmarshal([]byte(ms[len(ms)-1][1]), &claim) != nil {
		return r
	}
	switch claim.Status {
	case "done", "blocked", "failed":
		r.Status = claim.Status
	}
	r.Summary, r.Changes, r.Evidence, r.Open = claim.Summary, claim.Changes, claim.Evidence, claim.Open
	return r
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++ // do not cut a character in half
	}
	return "…" + s[i:]
}

// firstLine is the first line of s, trimmed.
func firstLine(s string) string { return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0]) }

// UncertainError is a start that may or may not have reached the executor:
// a request whose reply was lost. Ref names what to inspect. The
// coordinator records the operation as unknown rather than retrying it.
type UncertainError struct {
	Ref string
	Err error
}

func (e *UncertainError) Error() string { return "uncertain start (" + e.Ref + "): " + e.Err.Error() }
func (e *UncertainError) Unwrap() error { return e.Err }

// StripResult removes pai-result blocks from text shown to a person: the
// block is PAI's to read, and its claim is shown as checked results instead.
func StripResult(text string) string {
	return strings.TrimSpace(resultBlock.ReplaceAllString(text, ""))
}
