package worker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"pai/pkg/version"
)

// activeCalls bounds worker goroutines and interpreter processes. Control
// frames bypass admission so saturation cannot prevent cancellation.
const activeCalls = 8
const backgroundCalls = 2            // reserve foreground capacity even with long-lived jobs
const sendTimeout = 30 * time.Second // a disconnected harness must not hold a job's result
var backgroundSlots = make(chan struct{}, backgroundCalls)

const reconciliationInterval = 5 * time.Second // discover jobs finishing on an earlier connection

// Local is what this machine offers: its name, the join secret that lets it
// join the mesh, where spilled output lands, and which interpreters it has.
// The first interpreter is the default. Under a root service, Operator is the
// user code runs and files are touched as; nil is the worker's own user.
type Local struct {
	Name     string
	Secret   string
	Scratch  string
	Interps  []string
	Operator *user.User
}

type server struct {
	Local
	conn       net.Conn
	wmu        sync.Mutex // one frame on the wire at a time
	emu        sync.Mutex // envs, dead
	envs       map[string]*env
	dead       map[string]bool // an environment ended since its last run; the next run says so
	slots      chan struct{}
	background chan struct{}
	ctx        context.Context
	amu        sync.Mutex
	active     map[string]context.CancelFunc
	contexts   map[string]context.Context
}

// Serve answers one harness until the connection ends, then kills every
// environment. Jobs keep running. Each request runs in its own goroutine; runs that share an
// environment queue on it. An error frame with no id is the harness ending
// the connection on purpose, and its text is the error.
func Serve(conn net.Conn, l Local) error {
	s := &server{Local: l, conn: conn, envs: map[string]*env{}, dead: map[string]bool{}, slots: make(chan struct{}, activeCalls)}
	s.background = backgroundSlots
	s.active = map[string]context.CancelFunc{}
	s.contexts = map[string]context.Context{}
	var cancel context.CancelFunc
	s.ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if err := s.send(Frame{Header: Header{Kind: Hello, Reliable: true, OS: runtime.GOOS, Arch: runtime.GOARCH, Interps: l.Interps, Scratch: l.Scratch, Version: version.V}}); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(reconciliationInterval)
		defer ticker.Stop()
		for {
			if err := s.replay(); err != nil {
				return
			}
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	br := bufio.NewReader(conn)
	for {
		f, err := Decode(br)
		if err == nil && f.Kind == Error && f.ID == "" {
			err = errors.New(string(f.Body))
		}
		if err != nil {
			cancel()
			s.emu.Lock()
			for _, e := range s.envs {
				e.kill()
			}
			s.emu.Unlock()
			return err
		}
		switch f.Kind {
		case Cancel:
			cancelJob(f.ID)
			_ = spoolPut(s.Scratch, f, "cancellation")
			s.amu.Lock()
			if cancel := s.active[f.ID]; cancel != nil {
				cancel()
			}
			s.amu.Unlock()
			continue
		case Receipt:
			if _, err := spoolGet(s.Scratch, f.ID, "result"); err == nil {
				_ = spoolPut(s.Scratch, f, "receipt")
			}
			continue
		case Reconcile:
			if err := s.send(s.reconcile(f.ID)); err != nil {
				return err
			}
			continue
		}
		if f.Kind == Kill {
			// One control operation at a time, without creating unbounded goroutines.
			s.handle(f)
			continue
		}
		slots := s.slots
		if f.Job != "" {
			slots = s.background
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); s.handle(f) }()
		default:
			_ = s.send(Frame{Header: Header{ID: f.ID, Kind: Error}, Body: []byte("worker capacity reached; request not executed")})
		}
	}
}

func (s *server) send(f Frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(sendTimeout)); err != nil {
		return err
	}
	err := Encode(s.conn, f)
	if err != nil {
		_ = s.conn.Close()
	}
	return err
}

// handle answers one request. An error is text for the model.
func (s *server) handle(f Frame) {
	if !s.accept(f) {
		return
	}
	parent := s.ctx
	if parent == nil || f.Job != "" {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	s.amu.Lock()
	s.active[f.ID], s.contexts[f.ID] = cancel, ctx
	s.amu.Unlock()
	defer func() {
		cancel()
		s.amu.Lock()
		delete(s.active, f.ID)
		delete(s.contexts, f.ID)
		s.amu.Unlock()
	}()
	if err := s.send(Frame{Header: Header{ID: f.ID, Kind: Acknowledged}}); err != nil {
		return
	}
	if _, err := spoolGet(s.Scratch, f.ID, "cancellation"); err == nil {
		cancel()
	}
	var reply Frame
	var err error
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		reply, err = s.answer(f)
	}
	if err != nil {
		reply = Frame{Header: Header{Kind: Error}, Body: []byte(err.Error())}
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			reply.Kind = Uncertain
			reply.Body = []byte("execution interrupted; effects uncertain, do not replay: " + err.Error())
		}
	}
	reply.ID = f.ID
	reply.Session, reply.Origin = f.Session, f.Origin
	if err := spoolPut(s.Scratch, reply, "result"); err != nil {
		_ = s.send(Frame{Header: Header{ID: f.ID, Kind: Uncertain}, Body: []byte("terminal result persistence failed: " + err.Error())})
		return
	}
	_ = s.send(reply) // a failed send means the connection is gone; Serve sees it
}

func (s *server) answer(f Frame) (Frame, error) {
	if f.Deadline != 0 && time.Now().UnixMilli() >= f.Deadline {
		return Frame{}, errors.New("request deadline expired before execution")
	}
	switch f.Kind {
	case Run:
		return s.run(f)
	case Read:
		b, err := s.read(f.Path)
		return Frame{Header: Header{Kind: Bytes}, Body: b}, err
	case Write:
		return Frame{Header: Header{Kind: Done}}, s.write(f.Path, f.Body, f.Expect)
	case Kill:
		s.kill(f.Session)
		return Frame{Header: Header{Kind: Done}}, nil
	case Update:
		if f.Path == "pai-server" {
			return Frame{Header: Header{Kind: Done}}, updateServer(f.Version)
		}
		exe, err := update(f.Version)
		if err != nil {
			return Frame{}, err
		}
		_ = s.send(Frame{Header: Header{ID: f.ID, Kind: Done}})
		restart(exe)
	}
	return Frame{}, fmt.Errorf("unknown request %q", f.Kind)
}

func (s *server) run(f Frame) (Frame, error) {
	name := f.Interp
	if name == "" {
		name = s.Interps[0]
	}
	if !slices.Contains(s.Interps, name) {
		return Frame{}, fmt.Errorf("unknown interpreter %q, this worker runs: %s", name, strings.Join(s.Interps, ", "))
	}
	if f.Job != "" {
		it, err := s.interp(name)
		if err != nil {
			return Frame{}, err
		}
		return s.job(f, it)
	}
	key := f.Session + "/" + name
	s.amu.Lock()
	parent := s.contexts[f.ID]
	s.amu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	timeout := time.Duration(f.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	if f.Deadline != 0 && time.UnixMilli(f.Deadline).Before(deadline) {
		deadline = time.UnixMilli(f.Deadline)
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	e, reset, err := s.acquire(ctx, key, f.Session, name)
	if err != nil {
		return Frame{}, err
	}
	if err := s.send(Frame{Header: Header{ID: f.ID, Kind: Started, Pid: e.cmd.Process.Pid}}); err != nil {
		s.release(key, e, true)
		return Frame{}, err
	}
	var tail []byte
	sp := &spill{path: filepath.Join(s.Scratch, "output", f.ID), create: s.create}
	emit := func(b []byte) error {
		tail = append(tail, b...)
		if len(tail) > MaxBytes {
			tail = bytes.Clone(tail[len(tail)-MaxBytes:])
		}
		if err := sp.add(b); err != nil {
			return err
		}
		return s.send(Frame{Header: Header{ID: f.ID, Kind: Output}, Body: b})
	}
	if reset {
		if err := emit([]byte("environment was reset\n")); err != nil {
			e.kill()
			s.release(key, e, false)
			_, _ = sp.close()
			return Frame{}, err
		}
	}
	exit, alive, err := e.run(ctx, string(f.Body), "pai-done-"+f.ID, emit)
	if err != nil && alive {
		// output could not be delivered; what the process still holds is unread
		e.kill()
		alive = false
	}
	s.release(key, e, alive)
	full, cerr := sp.close()
	if ctx.Err() != nil {
		return Frame{}, fmt.Errorf("environment was reset: %w", ctx.Err())
	}
	if err != nil {
		return Frame{}, err
	}
	if cerr != nil {
		return Frame{}, cerr
	}
	return Frame{Header: Header{Kind: Result, Exit: exit, Full: full}, Body: tail}, nil
}

// acquire finds or starts the environment for key and holds it for one run.
// An environment that ended while this run waited for it is not used: the
// loop finds its replacement and the reset flag its end left.
func (s *server) acquire(ctx context.Context, key, session, name string) (*env, bool, error) {
	it, err := s.interp(name)
	if err != nil {
		return nil, false, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		s.emu.Lock()
		e, ok := s.envs[key]
		if !ok {
			if e, err = start(it, s.Operator, session); err != nil {
				s.emu.Unlock()
				return nil, false, err
			}
			s.envs[key] = e
		}
		reset := s.dead[key]
		delete(s.dead, key)
		s.emu.Unlock()
		if err := e.mu.lock(ctx); err != nil {
			return nil, false, err
		}
		s.emu.Lock()
		current := s.envs[key] == e
		s.emu.Unlock()
		if current {
			return e, reset, nil
		}
		e.mu.Unlock()
	}
}

// release hands the environment back. A dead one is forgotten so the next run
// starts fresh and says so.
func (s *server) release(key string, e *env, alive bool) {
	s.emu.Lock()
	if !alive {
		delete(s.envs, key)
		s.dead[key] = true
	}
	s.emu.Unlock()
	e.mu.Unlock()
}

// kill ends every environment and job of a session. The harness decides when
// a session's environments stop being worth keeping; the worker only obeys.
func (s *server) kill(session string) {
	killJobs(session)
	s.emu.Lock()
	defer s.emu.Unlock()
	for key, e := range s.envs {
		if strings.HasPrefix(key, session+"/") {
			e.kill()
			delete(s.envs, key)
			s.dead[key] = true
		}
	}
}

// spill holds a run's output in memory up to the limits and, once over, writes
// all of it to a file the result names. Lines are counted as the harness
// counts them, so the file exists exactly when the harness cuts.
type spill struct {
	path   string
	create func(string) (io.WriteCloser, error)
	held   []byte
	f      io.WriteCloser
}

func (s *spill) add(b []byte) error {
	if s.f == nil {
		s.held = append(s.held, b...)
		nl := []byte("\n")
		if len(s.held) <= MaxBytes && bytes.Count(bytes.TrimSuffix(s.held, nl), nl) < MaxLines {
			return nil
		}
		f, err := s.create(s.path)
		if err != nil {
			return err
		}
		s.f, b, s.held = f, s.held, nil
	}
	_, err := s.f.Write(b)
	return err
}

// close returns the file's path, or nothing when the output stayed in memory.
// A file that could not be finished is an error, not a path to half of it.
func (s *spill) close() (string, error) {
	if s.f == nil {
		return "", nil
	}
	if syncer, ok := s.f.(interface{ Sync() error }); ok {
		if err := syncer.Sync(); err != nil {
			_ = s.f.Close()
			return "", err
		}
	}
	return s.path, s.f.Close()
}
