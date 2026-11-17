package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"pai/pkg/connector"
	"pai/pkg/harness"
	"pai/pkg/statefile"
)

const (
	// paiVia is what ULTRA writes on the records it causes.
	paiVia = "ultra"
	// paiPoll is one long poll of a session's records.
	paiPoll = "25s"
	// paiCall bounds the calls around the long poll.
	paiCall = 30 * time.Second
)

// PAI runs a task on PAI's own agent loop: a session on the PAI server,
// labelled mode "ultra", with the tools of the workers the person chose.
// It needs the server; a project without one uses a native executor.
type PAI struct {
	Client *connector.Client
	// Base is the session config to start from; nil copies the newest
	// session's model, tools and workers, as the TUI's home does.
	Base *statefile.Conf
}

// Capabilities reports what this integration supports.
func (PAI) Capabilities() Capabilities {
	return Capabilities{
		Name: "pai", NativeSession: true, Resume: true, Cancel: true, Events: true, ToolIntercept: true, ReportsUsage: true,
		Tools: []string{"shell", "files", "git", "network", "browser"}, Remote: true,
		Notes: []string{
			"needs a reachable PAI server and a worker on the machine that holds the workspace",
			"Classic memory recall and extraction run inside that session as in Classic; ULTRA does not turn them off there",
		},
	}
}

// Available checks the server answers.
func (p PAI) Available(ctx context.Context) error {
	if p.Client == nil {
		return errors.New("not signed in to a PAI server: run pai login")
	}
	ctx, cancel := context.WithTimeout(ctx, paiCall)
	defer cancel()
	if _, err := p.Client.Version(ctx); err != nil {
		return fmt.Errorf("PAI server offline: %w", err)
	}
	return nil
}

func (p PAI) base(ctx context.Context) (statefile.Conf, error) {
	if p.Base != nil {
		return *p.Base, nil
	}
	infos, err := p.Client.Sessions(ctx, "", 1)
	if err != nil {
		return statefile.Conf{}, err
	}
	if len(infos) == 0 {
		return statefile.Conf{}, errors.New("no PAI session to copy a model and workers from: start one in Classic first")
	}
	return infos[0].Config, nil
}

type paiRun struct {
	p      PAI
	id     string
	events chan Event
	done   chan struct{}
	res    Result
	cancel context.CancelFunc
	once   sync.Once
}

func (r *paiRun) ID() string           { return r.id }
func (r *paiRun) Events() <-chan Event { return r.events }
func (r *paiRun) Wait() Result         { <-r.done; return r.res }
func (r *paiRun) Cancel() error {
	var err error
	r.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), paiCall)
		defer cancel()
		err = r.p.Client.Stop(ctx, r.id, paiVia)
		r.cancel()
	})
	return err
}

// Start creates (or, to resume, reuses) a session and posts the prompt.
func (p PAI) Start(ctx context.Context, a Assignment) (Run, error) {
	if err := p.Available(ctx); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, paiCall)
	defer cancel()
	id := a.Resume
	if id == "" {
		conf, err := p.base(cctx)
		if err != nil {
			return nil, err
		}
		model := conf.Model
		if a.Model != "" {
			model = a.Model
		}
		id, err = p.Client.Create(cctx, harness.Create{Model: model, Tools: conf.Tools, ContextLimit: conf.ContextLimit, Workers: conf.Workers,
			Input: conf.Input, Params: conf.Params, Name: "ULTRA " + a.TaskID, Mode: "ultra"}, paiVia)
		if err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}
	}
	before, err := p.Client.Info(cctx, id)
	if err != nil {
		return nil, err
	}
	text := a.Prompt
	if a.Workdir != "" {
		text = "Working directory on this worker: " + a.Workdir + "\n\n" + text
	}
	n, err := p.Client.Post(cctx, id, paiVia, text, nil)
	if err != nil {
		// The server may have taken the message and lost the reply.
		return nil, &UncertainError{Ref: "pai session " + id, Err: err}
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rctx, rcancel := context.WithTimeout(ctx, timeout)
	r := &paiRun{p: p, id: id, events: make(chan Event, eventBuffer), done: make(chan struct{}), cancel: rcancel}
	r.res.NativeSession = id
	go r.follow(rctx, n, before.Usage, time.Now())
	return r, nil
}

// follow reads the session's records after the posted message until the
// session is no longer working on it.
func (r *paiRun) follow(ctx context.Context, after int, start statefile.Usage, began time.Time) {
	defer close(r.done)
	defer close(r.events)
	defer r.cancel()
	emit := func(e Event) {
		e.Time = time.Now()
		select {
		case r.events <- e:
		default:
		}
	}
	emit(Event{Kind: "start", Text: "pai session " + r.id})
	var final strings.Builder
	r.res.Status = "unknown"
	for {
		recs, err := r.p.Client.Records(ctx, r.id, after, paiPoll)
		if ctx.Err() != nil {
			// A timeout, a cancel or a lost lease: stop the server's work too.
			r.Cancel()
			r.res.Status, r.res.Err = "cancelled", ctx.Err().Error()
			break
		}
		if err != nil {
			emit(Event{Kind: "error", Text: err.Error()})
			time.Sleep(time.Second)
			continue
		}
		for _, rec := range recs {
			after = max(after, rec.N)
			switch rec.Kind {
			case statefile.Text:
				final.Reset()
				final.Write(rec.Body)
				emit(Event{Kind: "text", Text: string(rec.Body)})
			case statefile.Call:
				var c struct{ Name string }
				_ = json.Unmarshal(rec.Body, &c)
				emit(Event{Kind: "tool", Text: c.Name})
			case statefile.Error:
				emit(Event{Kind: "error", Text: string(rec.Body)})
			}
		}
		ictx, cancel := context.WithTimeout(ctx, paiCall)
		info, err := r.p.Client.Info(ictx, r.id)
		cancel()
		if err != nil {
			continue
		}
		switch info.Status {
		case statefile.StatusClosed, statefile.StatusFailed, statefile.StatusInterrupted:
			u := info.Usage
			r.res.Usage = Usage{Input: u.Input - start.Input + u.CacheWrite - start.CacheWrite, Output: u.Output - start.Output, CacheRead: u.CacheRead - start.CacheRead, Reported: true}
			finish(&r.res, final.String())
			if info.Status != statefile.StatusClosed && r.res.Status == "unknown" {
				r.res.Status, r.res.Err = "failed", "session "+string(info.Status)
			}
			r.res.Duration = time.Since(began)
			emit(Event{Kind: "done", Text: r.res.Status})
			return
		}
	}
	r.res.Duration = time.Since(began)
	emit(Event{Kind: "done", Text: r.res.Status})
}
