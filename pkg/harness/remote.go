package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"pai/pkg/statefile"
	"pai/pkg/version"
	"pai/pkg/worker"
)

// runTimeout bounds a run that names no timeout: long enough for a build or an
// install, and the most a hung command costs before its environment is reset.
const runTimeout = 10 * time.Minute

// maxTimeout bounds the timeout a run may name: a day is past any build, and
// work longer than that is a background job.
const maxTimeout = 24 * time.Hour

// updateTimeout bounds a worker's answer to an update, which it gives after
// three downloads of up to five minutes each.
const updateTimeout = 20 * time.Minute

// writeStall bounds a frame write that makes no progress. A worker reads every
// frame at once, so a stall is a peer that stopped: asleep, frozen or cut off.
// Unbounded, its write holds wmu, and with it the worker's calls and every
// Pool.Resend, until TCP gives up.
const writeStall = 30 * time.Second

// writeChunk is the most written under one deadline, so a large frame on a
// slow link is bounded by its progress and not its size.
const writeChunk = 64 << 10

// replyQueue bounds memory per call; a stalled consumer terminates the
// connection rather than blocking cancellation and unrelated replies.
const replyQueue = 64

// requestTimeout bounds file/control operations and dispatch queueing.
const requestTimeout = 30 * time.Second

// Remote is a worker on the far end of a connection. Its hello header says
// what it is. Requests are multiplexed by id; a call waits for its terminal
// frame, the connection's end, or ctx: the worker's timeout bounds a run, and
// a stop cuts it short.
type Remote struct {
	worker.Header
	conn      net.Conn
	writeOnce sync.Once
	writer    chan struct{}
	cmu       sync.Mutex
	calls     map[string]chan worker.Frame
	left      map[string]chan struct{} // closed by a call that stopped listening
	done      chan struct{}            // closed when the connection ends
	onLate    func(string, worker.Frame) error
}

// Connect reads the worker's hello, checks the connection with admit, whose
// error is why it is refused and whose name is the worker's, and starts
// routing its replies. A worker from another release is
// told to update to this one and refused: it comes back as this release, and
// the error says whether the update began or why it failed. That worker is
// returned with the error, so the pool can list it.
func Connect(conn net.Conn, admit func() (string, error)) (*Remote, error) {
	return connect(conn, admit, nil)
}

func connect(conn net.Conn, admit func() (string, error), late func(string, worker.Frame) error) (*Remote, error) {
	br := bufio.NewReader(conn)
	hello, err := worker.Decode(br)
	if err != nil {
		return nil, err
	}
	if hello.Kind != worker.Hello {
		return nil, fmt.Errorf("first frame is %q, want hello", hello.Kind)
	}
	name, err := admit()
	if err != nil {
		return nil, err
	}
	r := &Remote{Header: hello.Header, conn: conn, calls: map[string]chan worker.Frame{}, left: map[string]chan struct{}{}, done: make(chan struct{})}
	r.Name = name
	r.onLate = late
	if !version.Match(hello.Version) {
		return r, fmt.Errorf("worker is version %s, harness is %s: %w", hello.Version, version.V, update(conn, br))
	}
	go r.recv(br)
	return r, nil
}

// send writes f under wmu, which the caller holds. A failed write leaves part
// of a frame on the connection, so it ends the connection, and recv fails
// every call on it.
func (r *Remote) send(f worker.Frame) error {
	err := worker.Encode(stallWriter{r.conn}, f)
	if err != nil {
		_ = r.conn.Close()
	}
	return err
}

// stallWriter writes in chunks, each under a fresh writeStall deadline.
type stallWriter struct{ net.Conn }

func (w stallWriter) Write(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if err := w.SetWriteDeadline(time.Now().Add(writeStall)); err != nil {
			return n, err
		}
		m, err := w.Conn.Write(p[n:min(len(p), n+writeChunk)])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// update tells a worker to become this release and waits for its answer. The
// worker downloads before it answers, so the wait is the download's.
//
// The request has an id of its own, and only a terminal frame with that id is
// the answer. A worker spools requests by id and replays unacknowledged
// results the moment it connects: with no id, every update after the first
// was refused as a repeat, and a replayed result read as "updated", so the
// harness hung up before the worker could start, and the worker looped on
// the old release for good.
func update(conn net.Conn, br *bufio.Reader) error {
	if err := conn.SetDeadline(time.Now().Add(updateTimeout)); err != nil {
		return err
	}
	id := "update-" + version.V + "-" + statefile.NewID()
	if err := worker.Encode(conn, worker.Frame{Header: worker.Header{ID: id, Kind: worker.Update, Version: version.V}}); err != nil {
		return err
	}
	for {
		f, err := worker.Decode(br)
		if err != nil {
			return fmt.Errorf("update: %w", err)
		}
		switch {
		case f.ID != id && !(f.ID == "" && f.Kind == worker.Error):
			continue // a replayed result or another call's frame
		case f.Kind == worker.Acknowledged:
			continue
		case f.Kind != worker.Done:
			return fmt.Errorf("update failed: %s", f.Body)
		}
		return errors.New("updated, restarting")
	}
}

func (r *Remote) recv(br *bufio.Reader) {
	late := make(chan worker.Frame, replyQueue)
	defer close(late)
	go func() {
		for f := range late {
			if r.onLate != nil && r.onLate(r.Name, f) == nil {
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				_ = r.sendContext(ctx, worker.Frame{Header: worker.Header{ID: f.ID, Kind: worker.Receipt}})
				cancel()
			}
		}
	}()
	for {
		f, err := worker.Decode(br)
		r.cmu.Lock()
		if err != nil {
			for _, ch := range r.calls {
				close(ch)
			}
			r.calls, r.left = nil, nil
			r.cmu.Unlock()
			_ = r.conn.Close()
			close(r.done)
			return
		}
		ch, ok := r.calls[f.ID]
		if ok {
			select {
			case ch <- f:
			case <-r.gone(f.ID):
			default:
				// Never hold cmu while waiting for a consumer. Closing the
				// connection makes an overflow explicit, never silent loss.
				_ = r.conn.Close()
			}
		}
		r.cmu.Unlock()
		if !ok && terminalFrame(f.Kind) && r.onLate != nil {
			select {
			case late <- f:
			default:
				_ = r.conn.Close()
			}
		}
	}
}

// gone is the channel a call closes when it stops listening, so a reply to a
// stopped call is dropped instead of blocking the connection.
func (r *Remote) gone(id string) <-chan struct{} { return r.left[id] }

// call sends one request and feeds every reply to on until the terminal one,
// or until ctx ends.
func (r *Remote) call(ctx context.Context, h worker.Header, body []byte, on func(worker.Frame)) (callErr error) {
	id := statefile.NewID()
	ch, left := make(chan worker.Frame, replyQueue), make(chan struct{})
	r.cmu.Lock()
	if r.calls == nil {
		r.cmu.Unlock()
		return errors.New("worker connection lost")
	}
	r.calls[id], r.left[id] = ch, left
	r.cmu.Unlock()
	defer func() {
		close(left)
		r.cmu.Lock()
		delete(r.calls, id)
		delete(r.left, id)
		r.cmu.Unlock()
	}()
	h.ID = id
	if deadline, ok := ctx.Deadline(); ok {
		h.Deadline = deadline.UnixMilli()
	}
	if t, ok := ctx.Value(executionKey{}).(executionTrace); ok {
		h.Session, h.Origin = t.session, t.origin
	}
	request := worker.Frame{Header: h, Body: body}
	if err := trace(ctx, r.Name, "created", request); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = trace(ctx, r.Name, "cancelled", request)
		return err
	}
	if err := trace(ctx, r.Name, "dispatched", request); err != nil {
		return err
	}
	terminal := false
	defer func() {
		if callErr != nil && !terminal {
			_ = trace(ctx, r.Name, "uncertain", worker.Frame{Header: h, Body: []byte(callErr.Error())})
		}
	}()
	err := r.sendContext(ctx, worker.Frame{Header: h, Body: body})
	if err != nil {
		return fmt.Errorf("dispatch failed; outcome uncertain, do not blindly replay: %w", err)
	}
	ack := time.NewTimer(requestTimeout)
	defer ack.Stop()
	var ackDeadline <-chan time.Time
	if r.Reliable || h.Job != "" {
		ackDeadline = ack.C
	}
	for {
		select {
		case <-ackDeadline:
			return errors.New("worker acknowledgement deadline exceeded; outcome uncertain, do not replay")
		case f, ok := <-ch:
			if !ok {
				return errors.New("worker connection lost; outcome uncertain, do not blindly replay")
			}
			if f.Kind == worker.Acknowledged {
				ackDeadline = nil
				if h.Job != "" {
					ack.Reset(requestTimeout)
					ackDeadline = ack.C
				}
				if err := trace(ctx, r.Name, "acknowledged", f); err != nil {
					return err
				}
				continue
			}
			if f.Kind == worker.Started {
				ackDeadline = nil
				if err := trace(ctx, r.Name, "started", f); err != nil {
					return err
				}
			}
			if terminalFrame(f.Kind) {
				state := "completed"
				if f.Kind == worker.Error {
					state = "failed"
				}
				if f.Kind == worker.Uncertain {
					state = "uncertain"
				}
				if err := trace(ctx, r.Name, state, f); err != nil {
					return err
				}
				terminal = true
				if _, ok := ctx.Value(executionKey{}).(executionTrace); ok && r.Reliable {
					if err := r.sendContext(ctx, worker.Frame{Header: worker.Header{ID: id, Kind: worker.Receipt}}); err != nil {
						// Result is already durable; reconnect can repeat it safely.
					}
				}
			}
			if f.Kind == worker.Error || f.Kind == worker.Uncertain {
				return errors.New(string(f.Body))
			}
			on(f)
			if f.Kind != worker.Output && f.Kind != worker.Started {
				return nil
			}
		case <-ctx.Done():
			if r.Reliable {
				cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = r.sendContext(cancelCtx, worker.Frame{Header: worker.Header{ID: id, Kind: worker.Cancel}})
				cancel()
			}
			return fmt.Errorf("outcome uncertain; cancellation is not confirmed, do not blindly replay: %w", ctx.Err())
		}
	}
}

// sendContext makes both writer admission and the write itself cancellable.
// A cancelled partial write poisons framing, so send closes the connection.
func (r *Remote) sendContext(ctx context.Context, f worker.Frame) error {
	r.writeOnce.Do(func() { r.writer = make(chan struct{}, 1) })
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	select {
	case r.writer <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return errors.New("worker connection lost")
	}
	defer func() { <-r.writer }()
	if err := ctx.Err(); err != nil {
		return err
	}
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = r.conn.Close()
		close(finished)
	})
	err := r.send(f)
	if !stop() {
		<-finished
	}
	return err
}

// Job starts code as a background job named job and returns its pid and log
// once the worker runs it. The call stays open until the job ends: done gets
// the log's tail and exit, or the error that ended the call, such as a lost
// connection. A job a kill ended answers done, and done is not called: a stop
// ended it on purpose.
func (r *Remote) Job(ctx context.Context, session, interpreter, code, job string, done func(Output, error)) (int, string, error) {
	started, ended := make(chan worker.Frame, 1), make(chan error, 1)
	var o Output
	stopped := false
	go func() {
		ended <- r.call(ctx, worker.Header{Kind: worker.Run, Session: session, Interp: interpreter, Job: job}, []byte(code), func(f worker.Frame) {
			switch f.Kind {
			case worker.Started:
				started <- f
			case worker.Result:
				o.Body, o.Exit, o.Full = f.Body, f.Exit, f.Path
			case worker.Done:
				stopped = true
			}
		})
	}()
	select {
	case f := <-started:
		go func() {
			if err := <-ended; err != nil || !stopped {
				done(o, err)
			}
		}()
		return f.Pid, f.Path, nil
	case err := <-ended:
		select {
		case f := <-started: // it started and ended before this select looked
			if err != nil || !stopped {
				go done(o, err)
			}
			return f.Pid, f.Path, nil
		default:
		}
		if err == nil {
			err = fmt.Errorf("worker %s answered without starting the job: it may be from before background runs", r.Name)
		}
		return 0, "", err
	}
}

// Run streams the output and keeps a tail within the result limits. Past twice
// the byte limit the front is dropped at a line start and the lines counted,
// a line cut in the middle counting as one; the worker has the whole in the
// file the result names. timeout is in seconds, and zero is runTimeout.
func (r *Remote) Run(ctx context.Context, session, interpreter, code string, timeout int) (Output, error) {
	if timeout == 0 {
		timeout = int(runTimeout.Seconds())
	}
	if timeout < 0 || timeout > int(maxTimeout/time.Second) {
		return Output{}, errors.New("invalid run timeout")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	var o Output
	mid := false // the tail starts inside a line already counted
	err := r.call(ctx, worker.Header{Kind: worker.Run, Session: session, Interp: interpreter, Timeout: timeout}, []byte(code), func(f worker.Frame) {
		if f.Kind == worker.Result {
			o.Exit, o.Full = f.Exit, f.Full
			return
		}
		o.Body = append(o.Body, f.Body...)
		if len(o.Body) > 2*worker.MaxBytes {
			drop := len(o.Body) - worker.MaxBytes
			if i := bytes.IndexByte(o.Body[drop:len(o.Body)-1], '\n'); i >= 0 {
				drop += i + 1
			}
			o.Cut += bytes.Count(o.Body[:drop], []byte("\n"))
			if mid {
				o.Cut--
			}
			if mid = o.Body[drop-1] != '\n'; mid {
				o.Cut++
			}
			o.Body = slices.Clone(o.Body[drop:])
		}
	})
	return o, err
}

func (r *Remote) Read(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var b []byte
	err := r.call(ctx, worker.Header{Kind: worker.Read, Path: path}, nil, func(f worker.Frame) { b = f.Body })
	return b, err
}

func (r *Remote) Write(ctx context.Context, path string, data []byte, expect string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return r.call(ctx, worker.Header{Kind: worker.Write, Path: path, Expect: expect}, data, func(worker.Frame) {})
}

// Update has the worker make cmd beside it release v.
func (r *Remote) Update(ctx context.Context, cmd, v string) error {
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	return r.call(ctx, worker.Header{Kind: worker.Update, Path: cmd, Version: v}, nil, func(worker.Frame) {})
}

func (r *Remote) Kill(ctx context.Context, session string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return r.call(ctx, worker.Header{Kind: worker.Kill, Session: session}, nil, func(worker.Frame) {})
}

func terminalFrame(kind string) bool {
	return kind == worker.Result || kind == worker.Done || kind == worker.Error || kind == worker.Bytes || kind == worker.Uncertain
}
