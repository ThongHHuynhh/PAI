package exec

import (
	"context"
	"sync"
	"time"
)

// Scripted is an executor that runs a Go function instead of a model. It
// exists for tests and model-free benchmarks of the coordinator; it is never
// offered as a real executor.
type Scripted struct {
	Name string
	Caps Capabilities
	// Do does the work in the workspace and returns the executor's final
	// text (with or without a pai-result block). It sees each call's
	// assignment, so a script can behave differently on retry or resume.
	Do func(ctx context.Context, a Assignment) (final string, usage Usage, err error)
}

// Capabilities reports the script's declared capabilities.
func (s Scripted) Capabilities() Capabilities {
	c := s.Caps
	if c.Name == "" {
		c.Name = s.Name
	}
	return c
}

// Available is always true.
func (Scripted) Available(context.Context) error { return nil }

type scriptedRun struct {
	id     string
	events chan Event
	done   chan struct{}
	res    Result
	cancel context.CancelFunc
	mu     sync.Mutex
	killed bool
}

func (r *scriptedRun) ID() string           { return r.id }
func (r *scriptedRun) Events() <-chan Event { return r.events }
func (r *scriptedRun) Wait() Result         { <-r.done; return r.res }
func (r *scriptedRun) Cancel() error {
	r.mu.Lock()
	r.killed = true
	r.mu.Unlock()
	r.cancel()
	return nil
}

// Start runs Do on its own goroutine.
func (s Scripted) Start(ctx context.Context, a Assignment) (Run, error) {
	cctx, cancel := context.WithCancel(ctx)
	r := &scriptedRun{id: a.TaskID, events: make(chan Event, eventBuffer), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(r.done)
		defer close(r.events)
		defer cancel()
		began := time.Now()
		r.events <- Event{Time: began, Kind: "start", Text: s.Name}
		final, usage, err := s.Do(cctx, a)
		finish(&r.res, final)
		r.res.Usage, r.res.Duration = usage, time.Since(began)
		if a.Resume != "" {
			r.res.NativeSession = a.Resume
		} else {
			r.res.NativeSession = "scripted-" + a.TaskID
		}
		r.mu.Lock()
		killed := r.killed
		r.mu.Unlock()
		switch {
		case killed:
			r.res.Status, r.res.Err = "cancelled", "cancelled"
		case err != nil:
			r.res.Status, r.res.Err = "failed", err.Error()
		}
		r.events <- Event{Time: time.Now(), Kind: "done", Text: r.res.Status}
	}()
	return r, nil
}
