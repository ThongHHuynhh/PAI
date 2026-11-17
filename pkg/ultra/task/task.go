// Package task is PAI ULTRA's durable task state: the task contract, its
// states and the transitions between them, ownership leases with fencing
// epochs, an operations journal for side effects, and portable checkpoints.
//
// Every change is an event appended to the task's log before its snapshot
// is replaced, so a crash at any point loses at most the change in progress.
package task

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// State is where a task is in its life.
type State string

const (
	Pending   State = "pending"    // ready to be claimed once its dependencies are accepted
	Claimed   State = "claimed"    // an owner holds the lease and has not started
	Running   State = "running"    // an executor is working on it
	Reported  State = "reported"   // the executor says it finished; not yet validated
	Accepted  State = "accepted"   // the coordinator validated it: done
	Paused    State = "paused"     // checkpointed and released; resumable
	Blocked   State = "blocked"    // needs a person: a review, a decision, an uncertain operation
	Failed    State = "failed"     // attempts exhausted; a person may reopen it
	Cancelled State = "cancelled"  // stopped for good
	HandedOff State = "handed_off" // its work continues in another session or mode
)

// transitions is every allowed move. Anything else is refused.
var transitions = map[State][]State{
	Pending:   {Claimed, Blocked, Cancelled, HandedOff},
	Claimed:   {Running, Pending, Paused, Blocked, Cancelled},
	Running:   {Reported, Paused, Blocked, Pending, Cancelled},
	Reported:  {Accepted, Pending, Blocked, Failed},
	Paused:    {Pending, Claimed, Blocked, Cancelled, HandedOff},
	Blocked:   {Pending, Paused, Accepted, Failed, Cancelled, HandedOff},
	Failed:    {Pending},
	Accepted:  nil,
	Cancelled: nil,
	HandedOff: nil,
}

// CanMove is whether from -> to is allowed.
func CanMove(from, to State) bool { return slices.Contains(transitions[from], to) }

// Terminal is whether nothing more happens to a task in s.
func (s State) Terminal() bool { return len(transitions[s]) == 0 }

// Active is whether an owner is (or was) working on it.
func (s State) Active() bool { return s == Claimed || s == Running }

// Criterion kinds.
const (
	CriterionCommand = "command" // run Command in the workspace; exit 0 passes
	CriterionFile    = "file"    // Path exists, and holds Contains when set
	CriterionReview  = "review"  // a person (or, when enabled, a model) judges Text
)

// Criterion is one acceptance criterion. The executor never changes these;
// an amendment is an explicit, recorded event.
type Criterion struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Text     string `json:"text,omitempty"`
	Command  string `json:"command,omitempty"`
	Path     string `json:"path,omitempty"`
	Contains string `json:"contains,omitempty"`
	Timeout  int    `json:"timeout_s,omitempty"`
}

// Describe is a criterion in one line.
func (c Criterion) Describe() string {
	switch c.Kind {
	case CriterionCommand:
		return "`" + c.Command + "` exits 0"
	case CriterionFile:
		if c.Contains != "" {
			return c.Path + " contains " + fmt.Sprintf("%q", c.Contains)
		}
		return c.Path + " exists"
	}
	return c.Text
}

// HashCriteria fingerprints the criteria so a silent change is detectable.
func HashCriteria(cs []Criterion) string {
	b, _ := json.Marshal(cs)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Owner holds a task's lease. Epoch is the fencing token: each claim gets a
// new one, and a write from an older epoch is refused.
type Owner struct {
	ID      string    `json:"id"`
	Host    string    `json:"host"`
	PID     int       `json:"pid"`
	Adapter string    `json:"adapter,omitempty"`
	Epoch   int       `json:"epoch"`
	Until   time.Time `json:"until"`
}

// Op states.
const (
	OpIntent  = "intent"  // recorded before starting: it may not have started
	OpStarted = "started" // it began; its effects may be partial
	OpDone    = "done"
	OpFailed  = "failed"
	OpUnknown = "unknown" // its owner died: whether it finished is not known
)

// Op is one external operation with effects: an executor run, a command.
// Recovery never replays one whose outcome is unknown unless Idempotent.
type Op struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Desc       string    `json:"desc"`
	State      string    `json:"state"`
	Idempotent bool      `json:"idempotent,omitempty"`
	Ref        string    `json:"ref,omitempty"` // e.g. the executor's native session id
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended,omitzero"`
	Note       string    `json:"note,omitempty"`
}

// Report is what an executor says about its attempt. It is a claim, not the
// task's status: only validation accepts a task.
type Report struct {
	Status   string    `json:"status"` // done, blocked, failed, or unknown when it gave none
	Summary  string    `json:"summary,omitempty"`
	Changes  []string  `json:"changes,omitempty"`
	Evidence []string  `json:"evidence,omitempty"`
	Open     []string  `json:"open,omitempty"`
	Time     time.Time `json:"time"`
	Native   string    `json:"native_session,omitempty"`
}

// CheckResult is one criterion's result in one validation.
type CheckResult struct {
	Criterion string        `json:"criterion"`
	Status    string        `json:"status"` // pass, fail, needs_review, error
	Detail    string        `json:"detail,omitempty"`
	Output    string        `json:"output,omitempty"` // the tail
	Exit      int           `json:"exit,omitempty"`
	Duration  time.Duration `json:"duration_ns,omitempty"`
	Env       string        `json:"env,omitempty"` // environment fingerprint
	Time      time.Time     `json:"time"`
}

// Validation is one validation of the task against its criteria.
type Validation struct {
	Passed       bool          `json:"passed"`
	NeedsReview  bool          `json:"needs_review"`
	Results      []CheckResult `json:"results"`
	CriteriaHash string        `json:"criteria_sha256"`
	Time         time.Time     `json:"time"`
}

// Task is the contract and its state.
type Task struct {
	ID           string      `json:"id"`
	Title        string      `json:"title"`
	Objective    string      `json:"objective"`
	Criteria     []Criterion `json:"criteria"`
	CriteriaHash string      `json:"criteria_sha256"`
	Constraints  []string    `json:"constraints,omitempty"`
	Decisions    []string    `json:"decisions,omitempty"`
	Paths        []string    `json:"paths,omitempty"` // declared scope; nil is undeclared
	Skills       []string    `json:"skills,omitempty"`
	DependsOn    []string    `json:"depends_on,omitempty"`
	Parent       string      `json:"parent,omitempty"`
	Workspace    string      `json:"workspace"`
	Worktree     string      `json:"worktree,omitempty"` // an isolated workspace, for delegated work

	State       State             `json:"state"`
	Rev         int               `json:"rev"`
	Epoch       int               `json:"epoch"`
	Owner       *Owner            `json:"owner,omitempty"`
	Attempts    int               `json:"attempts"`
	MaxAttempts int               `json:"max_attempts"`
	Executor    string            `json:"executor,omitempty"`
	ConfigRev   int               `json:"config_revision"`
	Native      map[string]string `json:"native,omitempty"` // adapter -> native session id; optional
	Mode        string            `json:"mode"`

	Report         *Report     `json:"report,omitempty"`
	Validation     *Validation `json:"validation,omitempty"`
	Completed      []string    `json:"completed,omitempty"`
	Hypotheses     []string    `json:"hypotheses,omitempty"`
	Blockers       []string    `json:"blockers,omitempty"`
	FailedAttempts []string    `json:"failed_attempts,omitempty"`
	NextAction     string      `json:"next_action,omitempty"`
	Ops            []Op        `json:"ops,omitempty"`
	Checkpoints    []string    `json:"checkpoints,omitempty"`
	Links          []string    `json:"links,omitempty"`
	Created        time.Time   `json:"created"`
	Updated        time.Time   `json:"updated"`
}

// Unknown is the ops whose outcome is not known.
func (t *Task) Unknown() []Op {
	var out []Op
	for _, o := range t.Ops {
		if o.State == OpUnknown || o.State == OpIntent || o.State == OpStarted {
			out = append(out, o)
		}
	}
	return out
}

// Op returns the op with id.
func (t *Task) Op(id string) *Op {
	for i := range t.Ops {
		if t.Ops[i].ID == id {
			return &t.Ops[i]
		}
	}
	return nil
}

// NewID is a sortable, unique id with a prefix.
func NewID(prefix string) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// Title is a short title from an objective.
func Title(objective string) string {
	t := strings.TrimSpace(strings.SplitN(strings.TrimSpace(objective), "\n", 2)[0])
	if r := []rune(t); len(r) > 72 {
		t = string(r[:71]) + "…"
	}
	return t
}
