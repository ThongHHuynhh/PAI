package task

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pai/pkg/ultra/internal/atomicfile"
	"pai/pkg/ultra/internal/lockfile"
)

const (
	taskFile   = "task.json"
	eventsFile = "events.jsonl"
	lockName   = ".lock"
	// lockWait bounds waiting for the per-task mutex. A dead holder's lock is
	// taken over at once (its pid is gone); lockStale is the last resort for
	// a holder that hangs while alive. A mutation is a few small writes.
	lockWait  = 10 * time.Second
	lockStale = 10 * time.Minute
	// DefaultLease is how long a claim lasts without renewal. Owners renew
	// at a third of it, so two missed renewals still leave time.
	DefaultLease = 60 * time.Second
	// maxEvent bounds one event line: the snapshot it carries must stay small.
	maxEvent = 8 << 20
	// tailChunk is how much of the log end is read at a time to find the newest event.
	tailChunk = 64 << 10
)

var (
	ErrNotFound   = errors.New("no such task")
	ErrRevision   = errors.New("the task changed since it was read")
	ErrStaleOwner = errors.New("this owner's lease was lost or taken over")
	ErrTransition = errors.New("transition not allowed")
	ErrHeld       = errors.New("the task is held by a live owner")
	ErrDeps       = errors.New("dependencies are not accepted yet")
	ErrAttempts   = errors.New("attempts exhausted")
	ErrUnresolved = errors.New("an operation's outcome is unknown; reconcile it first")
	ErrCorrupt    = errors.New("the task log is corrupt")
)

// tornType marks where a crash tore the log's last write; the line before
// it is that tear, not corruption.
const tornType = "torn_tail"

var tornMarker = `{"type":"` + tornType + `"}`

// Event is one change to a task. It carries the snapshot after the change,
// so the log alone rebuilds the task.
type Event struct {
	Rev   int             `json:"rev"`
	Time  time.Time       `json:"time"`
	Type  string          `json:"type"`
	Actor string          `json:"actor,omitempty"`
	From  State           `json:"from,omitempty"`
	To    State           `json:"to,omitempty"`
	Note  string          `json:"note,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Task  *Task           `json:"task"`
	// amend is set only by AmendCriteria: no other change may alter criteria.
	amend bool
}

// Store keeps tasks under a directory, one subdirectory each.
type Store struct {
	Dir string
	now func() time.Time
}

// NewStore opens the task store in dir.
func NewStore(dir string) *Store {
	return &Store{Dir: dir, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) dir(id string) string { return filepath.Join(s.Dir, id) }

// Guard is what a mutation must match: a revision (for edits made from a
// view of the task), an epoch (for an owner's writes), or neither.
type Guard struct {
	Rev   int // 0: any
	Epoch int // 0: not an owner's write
}

func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\.:`) && len(id) < 128
}

// Create stores a new task. Its criteria are fingerprinted now. The task's
// directory is made exclusively, so two creators of one id cannot both win.
func (s *Store) Create(t *Task) error {
	if t.ID == "" {
		t.ID = NewID("t")
	}
	if !validID(t.ID) {
		return fmt.Errorf("bad task id %q", t.ID)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(s.dir(t.ID), 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("task %s exists", t.ID)
		}
		return err
	}
	now := s.now()
	t.State, t.Rev, t.Created, t.Updated = Pending, 1, now, now
	if t.Title == "" {
		t.Title = Title(t.Objective)
	}
	t.CriteriaHash = HashCriteria(t.Criteria)
	return s.commit(t, Event{Rev: 1, Time: now, Type: "created", To: Pending})
}

// commit appends the event, then replaces the snapshot. A torn tail left by
// an earlier crash is ended with a newline first, so it cannot swallow this
// event.
func (s *Store) commit(t *Task, e Event) error {
	snap := *t
	e.Task = &snap
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > maxEvent {
		return fmt.Errorf("task %s: event of %d bytes is over the %d limit", t.ID, len(b), maxEvent)
	}
	path := filepath.Join(s.dir(t.ID), eventsFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if end > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, end-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			// A crash tore the last write. End it, and say so in the log, so a
			// reader can tell this recovered tear from corruption.
			b = append([]byte("\n"+tornMarker+"\n"), b...)
		}
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return atomicfile.WriteJSON(filepath.Join(s.dir(t.ID), taskFile), t, 0o644)
}

// Get reads a task. When the log is ahead of the snapshot (a crash between
// the two writes) the log wins. Only the log's end is read.
func (s *Store) Get(id string) (*Task, error) {
	if !validID(id) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	var t Task
	err := atomicfile.ReadJSON(filepath.Join(s.dir(id), taskFile), &t)
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(s.dir(id)); serr != nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
	}
	last, lerr := s.lastEvent(id)
	if lerr == nil && last.Task != nil && (err != nil || last.Rev > t.Rev) {
		return last.Task, nil
	}
	if err != nil {
		return nil, errors.Join(err, lerr)
	}
	return &t, nil
}

// lastEvent is the newest whole event, read from the end of the log. A torn
// last line is skipped.
func (s *Store) lastEvent(id string) (Event, error) {
	f, err := os.Open(filepath.Join(s.dir(id), eventsFile))
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return Event{}, err
	}
	var buf []byte
	for pos := end; pos > 0 && len(buf) <= 2*maxEvent; {
		n := min(int64(tailChunk), pos)
		pos -= n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, pos); err != nil {
			return Event{}, err
		}
		buf = append(chunk, buf...)
		lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte{'\n'})
		// The first line may be cut by the chunk boundary unless at the start.
		for i := len(lines) - 1; i >= 0; i-- {
			if i == 0 && pos > 0 {
				break
			}
			var e Event
			if json.Unmarshal(lines[i], &e) == nil && e.Task != nil {
				return e, nil
			}
		}
	}
	return Event{}, fs.ErrNotExist
}

// Events is the task's log, oldest first. A torn last line is skipped; a
// bad line anywhere else is corruption and an error.
func (s *Store) Events(id string) ([]Event, error) {
	f, err := os.Open(filepath.Join(s.dir(id), eventsFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxEvent+1)
	bad := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Event
		if json.Unmarshal(line, &e) != nil {
			bad++
			continue
		}
		if e.Type == tornType {
			bad = 0 // the bad line before was a recovered tear
			continue
		}
		if bad > 0 {
			return out, fmt.Errorf("%w: %s: an unreadable line before event %d", ErrCorrupt, id, e.Rev)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// List is every task, newest first.
func (s *Store) List() ([]*Task, error) {
	es, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Task
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		if t, err := s.Get(e.Name()); err == nil {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *Task) int { return b.Created.Compare(a.Created) })
	return out, nil
}

// runnable states let an executor act; none may be entered while a
// non-idempotent operation's outcome is unknown.
func runnable(s State) bool { return s == Pending || s == Claimed || s == Running }

func unresolved(t *Task) *Op {
	for i := range t.Ops {
		if t.Ops[i].State == OpUnknown && !t.Ops[i].Idempotent {
			return &t.Ops[i]
		}
	}
	return nil
}

// Update applies fn under the task's lock, if g matches. fn returns the
// event to record; returning an error changes nothing. Invariants are
// checked after fn: legal transition, criteria changed only by amendment,
// no runnable state over an unresolved operation.
func (s *Store) Update(id string, g Guard, fn func(t *Task) (Event, error)) (*Task, error) {
	if !validID(id) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	lk, err := lockfile.Take(filepath.Join(s.dir(id), lockName), lockWait, lockStale)
	if err != nil {
		return nil, err
	}
	defer lk.Release()
	t, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if g.Rev != 0 && t.Rev != g.Rev {
		return nil, fmt.Errorf("%w: at rev %d, expected %d", ErrRevision, t.Rev, g.Rev)
	}
	if g.Epoch != 0 && (t.Owner == nil || t.Owner.Epoch != g.Epoch || t.Epoch != g.Epoch) {
		return nil, ErrStaleOwner
	}
	// An expired lease authorizes nothing, even before anyone recovers the
	// task: an owner that was suspended past it must stop.
	if g.Epoch != 0 && s.now().After(t.Owner.Until) {
		return nil, fmt.Errorf("%w: the lease expired at %s", ErrStaleOwner, t.Owner.Until.Format(time.RFC3339))
	}
	before, criteria := t.State, t.CriteriaHash
	e, err := fn(t)
	if err != nil {
		return nil, err
	}
	if t.State != before && !CanMove(before, t.State) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrTransition, before, t.State)
	}
	if !e.amend && (t.CriteriaHash != criteria || HashCriteria(t.Criteria) != criteria) {
		return nil, errors.New("acceptance criteria may change only through an explicit amendment")
	}
	if t.State != before && runnable(t.State) {
		if o := unresolved(t); o != nil {
			return nil, fmt.Errorf("%w: %s (%s)", ErrUnresolved, o.ID, o.Desc)
		}
	}
	t.Rev++
	t.Updated = s.now()
	e.Rev, e.Time = t.Rev, t.Updated
	if t.State != before {
		e.From, e.To = before, t.State
	}
	// A holder that hung past the stale age has lost the lock: it must not write.
	if !lk.Held() {
		return nil, fmt.Errorf("%w: the task lock was taken over", ErrStaleOwner)
	}
	if err := s.commit(t, e); err != nil {
		return nil, err
	}
	return t, nil
}

// Move changes state with a reason.
func (s *Store) Move(id string, g Guard, to State, actor, note string) (*Task, error) {
	return s.Update(id, g, func(t *Task) (Event, error) {
		t.State = to
		return Event{Type: "state", Actor: actor, Note: note}, nil
	})
}

// Claim takes the lease for owner. A task is claimable when pending or
// paused with its dependencies accepted. An owner whose lease ran out is
// recovered first, by the same rules as Recover.
func (s *Store) Claim(id string, o Owner, lease time.Duration) (*Task, error) {
	if t, err := s.Get(id); err == nil && t.State.Held() && t.Owner != nil && s.now().After(t.Owner.Until) {
		if err := s.recoverOne(t); err != nil && !errors.Is(err, ErrRevision) {
			return nil, err
		}
	}
	return s.Update(id, Guard{}, func(t *Task) (Event, error) {
		switch {
		case t.State == Pending || t.State == Paused:
		case t.State.Held():
			return Event{}, fmt.Errorf("%w: %s until %s", ErrHeld, t.Owner.ID, t.Owner.Until.Format(time.RFC3339))
		default:
			return Event{}, fmt.Errorf("%w: %s cannot be claimed", ErrTransition, t.State)
		}
		for _, d := range t.DependsOn {
			dt, err := s.Get(d)
			if err != nil || dt.State != Accepted {
				return Event{}, fmt.Errorf("%w: %s", ErrDeps, d)
			}
		}
		t.Epoch++
		o.Epoch, o.Until = t.Epoch, s.now().Add(lease)
		t.Owner = &o
		t.State = Claimed
		return Event{Type: "claimed", Actor: o.ID}, nil
	})
}

// StartAttempt moves a claimed task to running and counts the attempt. It
// refuses once MaxAttempts attempts were made.
func (s *Store) StartAttempt(id string, epoch int, executor string, configRev int) (*Task, error) {
	return s.Update(id, Guard{Epoch: epoch}, func(t *Task) (Event, error) {
		if t.MaxAttempts > 0 && t.Attempts >= t.MaxAttempts {
			return Event{}, fmt.Errorf("%w: %d of %d", ErrAttempts, t.Attempts, t.MaxAttempts)
		}
		t.Attempts++
		t.Executor, t.ConfigRev = executor, configRev
		t.State = Running
		return Event{Type: "attempt", Actor: t.Owner.ID, Note: fmt.Sprintf("attempt %d with %s, config r%d", t.Attempts, executor, configRev)}, nil
	})
}

// Reopen gives a failed or blocked task more attempts, by a person's decision.
func (s *Store) Reopen(id string, extra int, actor, reason string) (*Task, error) {
	if extra < 1 || strings.TrimSpace(reason) == "" {
		return nil, errors.New("reopen needs at least one attempt and a reason")
	}
	return s.Update(id, Guard{}, func(t *Task) (Event, error) {
		if t.State != Failed && t.State != Blocked {
			return Event{}, fmt.Errorf("%w: reopen a failed or blocked task, not %s", ErrTransition, t.State)
		}
		t.MaxAttempts = t.Attempts + extra
		t.Blockers = nil
		t.State = Pending
		return Event{Type: "reopened", Actor: actor, Note: reason}, nil
	})
}

// Renew extends the lease of the owner holding epoch.
func (s *Store) Renew(id string, epoch int, lease time.Duration) error {
	_, err := s.Update(id, Guard{Epoch: epoch}, func(t *Task) (Event, error) {
		t.Owner.Until = s.now().Add(lease)
		return Event{Type: "renewed", Actor: t.Owner.ID}, nil
	})
	return err
}

// Release gives the lease up, moving the task to state. Operations still in
// progress become unknown: the owner leaving does not finish them.
func (s *Store) Release(id string, epoch int, to State, note string) (*Task, error) {
	return s.Update(id, Guard{Epoch: epoch}, func(t *Task) (Event, error) {
		actor := t.Owner.ID
		markUnknown(t, "released by "+actor+" while in progress")
		t.Owner = nil
		t.State = to
		if to == Paused && unresolved(t) != nil {
			t.State = Blocked
			t.Blockers = append(t.Blockers, "released with an operation in progress; reconcile it first")
		}
		return Event{Type: "released", Actor: actor, Note: note}, nil
	})
}

func markUnknown(t *Task, why string) {
	for i := range t.Ops {
		switch t.Ops[i].State {
		case OpIntent, OpStarted:
			t.Ops[i].State = OpUnknown
			t.Ops[i].Note = why
		}
	}
}

// Recover pauses every task whose owner's lease has run out, marking its
// unfinished operations unknown, and blocks those with an unknown
// non-idempotent operation so a person reconciles it. It returns the ids.
func (s *Store) Recover() ([]string, error) {
	ts, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []string
	now := s.now()
	for _, t := range ts {
		if !t.State.Held() || t.Owner == nil || !now.After(t.Owner.Until) {
			continue
		}
		err := s.recoverOne(t)
		if err == nil {
			out = append(out, t.ID)
		} else if !errors.Is(err, ErrRevision) {
			return out, err
		}
	}
	return out, nil
}

// recoverOne pauses (or blocks) one task whose owner's lease ran out, as of
// the revision read.
func (s *Store) recoverOne(seen *Task) error {
	_, err := s.Update(seen.ID, Guard{Rev: seen.Rev}, func(t *Task) (Event, error) {
		if t.Owner == nil || !s.now().After(t.Owner.Until) {
			return Event{}, ErrRevision
		}
		owner := t.Owner.ID
		markUnknown(t, "owner "+owner+" stopped renewing its lease")
		t.Owner = nil
		t.State = Paused
		if t.State == Paused && seen.State == Reported {
			// The executor finished; only the decision was lost. Validation is
			// repeatable, so the task goes back to be validated by a new attempt.
			t.State = Pending
		}
		if o := unresolved(t); o != nil {
			t.State = Blocked
			for _, o := range t.Ops {
				if o.State == OpUnknown && !o.Idempotent {
					t.Blockers = append(t.Blockers, fmt.Sprintf("operation %s (%s) may or may not have completed; inspect the workspace before resuming", o.ID, o.Desc))
				}
			}
		}
		return Event{Type: "recovered", Note: "lease of " + owner + " expired"}, nil
	})
	return err
}

// BeginOp records an op's intent; the owner then marks it started.
func (s *Store) BeginOp(id string, epoch int, o Op) (Op, error) {
	if o.ID == "" {
		o.ID = NewID("op")
	}
	o.State, o.Started = OpIntent, s.now()
	_, err := s.Update(id, Guard{Epoch: epoch}, func(t *Task) (Event, error) {
		t.Ops = append(t.Ops, o)
		return Event{Type: "op", Note: o.ID + " " + OpIntent}, nil
	})
	return o, err
}

// SetOp updates an op's state and reference.
func (s *Store) SetOp(id string, epoch int, opID, state, ref, note string) error {
	_, err := s.Update(id, Guard{Epoch: epoch}, func(t *Task) (Event, error) {
		o := t.Op(opID)
		if o == nil {
			return Event{}, fmt.Errorf("no op %s", opID)
		}
		if o.State == OpUnknown {
			return Event{}, fmt.Errorf("op %s is unknown: a person reconciles it", opID)
		}
		o.State = state
		if ref != "" {
			o.Ref = ref
		}
		if note != "" {
			o.Note = note
		}
		if state == OpDone || state == OpFailed {
			o.Ended = s.now()
		}
		return Event{Type: "op", Note: opID + " " + state}, nil
	})
	return err
}

// ResolveOp records a person's reconciliation of an unknown op.
func (s *Store) ResolveOp(id, opID, state, note string) (*Task, error) {
	return s.Update(id, Guard{}, func(t *Task) (Event, error) {
		o := t.Op(opID)
		if o == nil || o.State != OpUnknown {
			return Event{}, fmt.Errorf("op %s is not unknown", opID)
		}
		if state != OpDone && state != OpFailed {
			return Event{}, errors.New("resolve an op as done or failed")
		}
		o.State, o.Note, o.Ended = state, note, s.now()
		t.Blockers = slices.DeleteFunc(t.Blockers, func(b string) bool { return strings.Contains(b, opID) })
		if t.State == Blocked && unresolved(t) == nil && len(t.Blockers) == 0 {
			t.State = Paused
		}
		return Event{Type: "op_resolved", Actor: "person", Note: opID + " " + state + ": " + note}, nil
	})
}

// AmendCriteria replaces the criteria explicitly, recording who and why and
// keeping the old criteria in the event. It is the only way criteria change.
func (s *Store) AmendCriteria(id string, g Guard, cs []Criterion, actor, reason string) (*Task, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, errors.New("an amendment needs a reason")
	}
	return s.Update(id, g, func(t *Task) (Event, error) {
		if t.State == Accepted || t.State.Terminal() || t.State.Held() {
			return Event{}, fmt.Errorf("%w: criteria of a %s task cannot change; reopen it or make a new task", ErrTransition, t.State)
		}
		old, _ := json.Marshal(t.Criteria)
		t.Validation = nil // a check of other criteria proves nothing about these
		t.Criteria = cs
		t.CriteriaHash = HashCriteria(cs)
		t.Decisions = append(t.Decisions, "criteria amended by "+actor+": "+reason)
		return Event{Type: "criteria_amended", Actor: actor, Note: reason, Data: old, amend: true}, nil
	})
}
