package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// stuckAfter is how long a call may go with no lifecycle event from its
// worker before diagnose calls it stuck: well over a run's normal reply time,
// well under the ten minute run bound, so a hang shows before it ends.
const stuckAfter = 3 * time.Minute

// Diagnosis is what a person or an agent needs to tell a working session from
// a stuck one: where it waits, on what, since when, and why it may not move.
type Diagnosis struct {
	ID      string           `json:"id"`
	Status  statefile.Status `json:"status"`
	Verdict string           `json:"verdict"`
	Last    string           `json:"last"` // the newest record: n, kind, age
	Idle    string           `json:"idle"` // since the newest record
	Calls   []WaitingCall    `json:"calls,omitempty"`
	Error   string           `json:"error,omitempty"` // the newest error record, when it is the tip
	Workers map[string]bool  `json:"workers"`         // the session's workers: connected or not
	Context string           `json:"context"`         // used of limit
}

// WaitingCall is a call with no result yet.
type WaitingCall struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Worker string `json:"worker,omitempty"`
	Stage  string `json:"stage,omitempty"` // the newest lifecycle event: dispatched, acknowledged, started...
	Age    string `json:"age"`
	Quiet  string `json:"quiet"` // since the newest lifecycle event
}

func ago(t, now time.Time) string { return now.Sub(t).Round(time.Second).String() }

// Diagnose reads one session's file and the worker pool, and judges.
func (h *Harness) Diagnose(id string) (Diagnosis, error) {
	chain, err := h.Sessions.Load(id)
	if err != nil {
		return Diagnosis{}, err
	}
	conf, err := config(chain)
	if err != nil {
		return Diagnosis{}, err
	}
	now, tip := time.Now(), chain[len(chain)-1]
	d := Diagnosis{ID: id, Status: h.status(id, chain), Last: fmt.Sprintf("%d %s %s ago", tip.N, tip.Kind, ago(tip.Time, now)), Idle: ago(tip.Time, now), Workers: map[string]bool{}, Context: fmt.Sprintf("%d of %d", statefile.ContextUsed(chain), conf.ContextLimit)}
	for _, w := range conf.Workers {
		_, ok := h.Workers.Get(w.Name)
		d.Workers[w.Name] = ok
	}
	if tip.Kind == statefile.Error {
		d.Error = string(tip.Body)
	}
	events := map[string]ExecutionInfo{}
	for _, e := range executionInfo(chain) {
		if _, seen := events[e.Call]; !seen {
			events[e.Call] = e
		}
	}
	for _, c := range statefile.Waiting(chain) {
		var call provider.Call
		_ = json.Unmarshal(c.Body, &call)
		var a struct{ Worker string }
		_ = json.Unmarshal(call.Arguments, &a)
		w := WaitingCall{ID: c.ID, Tool: call.Name, Worker: a.Worker, Age: ago(c.Time, now), Quiet: ago(c.Time, now)}
		if e, ok := events[c.ID]; ok {
			w.Stage, w.Quiet = e.Stage, ago(e.Updated, now)
		}
		d.Calls = append(d.Calls, w)
	}
	d.Verdict = verdict(d, chain, now)
	return d, nil
}

// verdict is one line: the most likely reason the session does or does not move.
func verdict(d Diagnosis, chain []statefile.Record, now time.Time) string {
	for _, c := range d.Calls {
		if c.Worker != "" && !d.Workers[c.Worker] {
			return fmt.Sprintf("stuck: %s waits on worker %s, which is not connected", c.Tool, c.Worker)
		}
	}
	for _, c := range d.Calls {
		if c.Tool == "ask" {
			return "waiting for the person to answer an ask"
		}
		if q, _ := time.ParseDuration(c.Quiet); q > stuckAfter {
			return fmt.Sprintf("likely stuck: %s on %s has had no event from its worker for %s (stage %q); stop the session, or kill the process on the worker", c.Tool, c.Worker, c.Quiet, c.Stage)
		}
	}
	switch {
	case len(d.Calls) > 0:
		return fmt.Sprintf("working: %d call(s) running", len(d.Calls))
	case d.Status == statefile.StatusStepping:
		return "working: waiting for the model's reply, since " + d.Idle
	case d.Status == statefile.StatusFailed:
		return "failed: " + firstLine(d.Error) + "; retry after the cause is fixed"
	case d.Status == statefile.StatusInterrupted:
		return "interrupted: a call lost its result; the next step reports it as uncertain"
	}
	return string(d.Status) + ": nothing is running"
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' || i >= 300 {
			return s[:i]
		}
	}
	return s
}

var diagnoseTool = provider.Tool{Name: "diagnose", Description: "Diagnose a PAI session by id: whether it is working, waiting on a person, a model or a worker, or stuck, with its calls waiting for results, their workers and how long they have been quiet", Schema: schema(`{"type":"object","properties":{"session":{"type":"string"}},"required":["session"]}`)}

func (h *Harness) diagnoseCall(_ context.Context, call statefile.Record, c provider.Call) statefile.Record {
	var a struct{ Session string }
	if err := json.Unmarshal(c.Arguments, &a); err != nil || !statefile.ValidID(a.Session) {
		return result(call.ID, fmt.Sprintf("error: bad session id %q", a.Session))
	}
	d, err := h.Diagnose(a.Session)
	if err != nil {
		return result(call.ID, "error: "+err.Error())
	}
	b, _ := json.MarshalIndent(d, "", " ")
	return result(call.ID, string(b))
}
