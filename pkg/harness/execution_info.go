package harness

import (
	"encoding/json"
	"pai/pkg/statefile"
	"sort"
	"time"
)

// ExecutionInfo is safe lifecycle metadata, never command bodies or results.
// Background remains true until a terminal worker outcome, not tool startup.
type ExecutionInfo struct {
	Call       string    `json:"call"`
	Attempt    string    `json:"attempt"`
	Worker     string    `json:"worker"`
	Stage      string    `json:"stage"`
	Background bool      `json:"background"`
	Active     bool      `json:"active"`
	Started    time.Time `json:"started"`
	Updated    time.Time `json:"updated"`
	Deadline   int64     `json:"deadline,omitempty"`
	PID        int       `json:"pid,omitempty"`
	Log        string    `json:"log,omitempty"`
}

func executionInfo(chain []statefile.Record) []ExecutionInfo {
	byID := map[string]*ExecutionInfo{}
	for _, r := range chain {
		if r.Kind != statefile.Execution {
			continue
		}
		var e executionEvent
		if json.Unmarshal(r.Body, &e) != nil || e.Frame.ID == "" {
			continue
		}
		f := e.Frame
		x := byID[f.ID]
		if x == nil {
			x = &ExecutionInfo{Call: r.For, Attempt: f.ID, Started: r.Time}
			byID[f.ID] = x
		}
		x.Stage, x.Updated = e.State, r.Time
		if e.Worker != "" {
			x.Worker = e.Worker
		}
		if f.Job != "" {
			x.Background = true
		}
		if f.Deadline != 0 {
			x.Deadline = f.Deadline
		}
		if f.Pid != 0 {
			x.PID = f.Pid
		}
		if f.Path != "" && x.Background {
			x.Log = f.Path
		}
		x.Active = e.State != "completed" && e.State != "failed" && e.State != "cancelled" && e.State != "uncertain"
	}
	var out []ExecutionInfo
	for _, x := range byID {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].Updated.After(out[j].Updated)
	})
	// Keep all outstanding work plus only a small terminal history.
	active := 0
	for active < len(out) && out[active].Active {
		active++
	}
	return out[:min(len(out), active+32)]
}
