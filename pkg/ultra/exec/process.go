package exec

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	osexec "os/exec"
	"strings"
	"sync"
	"time"

	"pai/pkg/ultra/internal/proc"
)

const (
	// eventBuffer is how many progress events wait for a slow reader before
	// older ones are dropped: progress must never stall the executor.
	eventBuffer = 256
	// maxLine bounds one JSON line from an executor.
	maxLine = 16 << 20
	// stderrKeep is how much of an executor's stderr a failed result keeps.
	stderrKeep = 4000
	// DefaultTimeout bounds one execution unless the assignment says otherwise.
	DefaultTimeout = 2 * time.Hour
)

// lineParser turns one stdout line into events and updates the result.
type lineParser func(line []byte, r *Result, emit func(Event))

// procRun is an execution of a local CLI.
type procRun struct {
	id     string
	cmd    *osexec.Cmd
	cancel context.CancelFunc
	events chan Event
	done   chan struct{}
	res    Result
	mu     sync.Mutex
	killed bool
}

func (p *procRun) ID() string           { return p.id }
func (p *procRun) Events() <-chan Event { return p.events }
func (p *procRun) Wait() Result         { <-p.done; return p.res }
func (p *procRun) Cancel() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	proc.Kill(p.cmd)
	p.cancel()
	return nil
}

// startProcess runs name args in dir with stdin, parsing stdout line by line.
func startProcess(ctx context.Context, id, dir, name string, args []string, stdin string, timeout time.Duration, parse lineParser) (*procRun, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	cmd := proc.Command(cctx, name, args...) // a cancel or timeout stops the whole tree
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &limited{w: &stderr, n: 1 << 20}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	p := &procRun{id: id, cmd: cmd, cancel: cancel, events: make(chan Event, eventBuffer), done: make(chan struct{})}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	emit := func(e Event) {
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		select {
		case p.events <- e:
		default: // a slow reader loses progress lines, never the result
		}
	}
	go func() {
		defer close(p.done)
		defer close(p.events)
		defer cancel()
		p.res.Status = "unknown"
		emit(Event{Kind: "start", Text: name})
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64<<10), maxLine)
		for sc.Scan() {
			parse(sc.Bytes(), &p.res, emit)
		}
		werr := cmd.Wait()
		p.res.Duration = time.Since(started)
		p.mu.Lock()
		killed := p.killed
		p.mu.Unlock()
		switch {
		case killed:
			p.res.Status, p.res.Err = "cancelled", "cancelled"
		case cctx.Err() == context.DeadlineExceeded:
			p.res.Status, p.res.Err = "failed", fmt.Sprintf("timed out after %s", timeout)
		case werr != nil && p.res.Status == "unknown":
			p.res.Status = "failed"
			p.res.Err = strings.TrimSpace(werr.Error() + ": " + tail(stderr.String(), stderrKeep))
		}
		emit(Event{Kind: "done", Text: p.res.Status})
	}()
	return p, nil
}

// limited writes at most n bytes, then discards.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(b []byte) (int, error) {
	if l.n > 0 {
		k := min(len(b), l.n)
		l.w.Write(b[:k])
		l.n -= k
	}
	return len(b), nil
}

// finish reads the claim from the final text into r, keeping usage and session.
func finish(r *Result, final string) {
	claim := ParseResult(final)
	usage, native, dur := r.Usage, r.NativeSession, r.Duration
	*r = claim
	r.Usage, r.NativeSession, r.Duration = usage, native, dur
}
