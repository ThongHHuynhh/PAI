// Package runtime wires PAI ULTRA for a working directory: the project,
// the coordinator and the executors. It never needs a PAI server for local
// work: the server client is loaded only when the PAI executor or a Classic
// memory import asks for it.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"pai/pkg/connector"
	"pai/pkg/ultra/coord"
	"pai/pkg/ultra/exec"
	"pai/pkg/ultra/knowledge"
)

// serverTime bounds loading the server client and one call to it.
const serverTime = 15 * time.Second

// Server loads the PAI server client on first use. A success is kept; a
// failure is not, so a server that comes back is found on the next use.
type Server struct {
	mu sync.Mutex
	c  *connector.Client
	// Load is connector.Load unless a test replaces it.
	Load func(context.Context) (*connector.Client, error)
}

// Client is the server client, or why there is none.
func (s *Server) Client(ctx context.Context) (*connector.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		return s.c, nil
	}
	load := s.Load
	if load == nil {
		load = connector.Load
	}
	cctx, cancel := context.WithTimeout(ctx, serverTime)
	defer cancel()
	c, err := load(cctx)
	if err != nil {
		return nil, fmt.Errorf("no PAI server: %w", err)
	}
	s.c = c
	return c, nil
}

// lazyPAI is the PAI executor with its server loaded on first use.
type lazyPAI struct{ s *Server }

func (l lazyPAI) Capabilities() exec.Capabilities { return exec.PAI{}.Capabilities() }

func (l lazyPAI) Available(ctx context.Context) error {
	c, err := l.s.Client(ctx)
	if err != nil {
		return err
	}
	return exec.PAI{Client: c}.Available(ctx)
}

func (l lazyPAI) Start(ctx context.Context, a exec.Assignment) (exec.Run, error) {
	c, err := l.s.Client(ctx)
	if err != nil {
		return nil, err
	}
	return exec.PAI{Client: c}.Start(ctx, a)
}

// Adapters is every executor ULTRA offers.
func Adapters(s *Server) map[string]exec.Adapter {
	return map[string]exec.Adapter{
		"claude": exec.Claude{},
		"codex":  exec.Codex{},
		"pai":    lazyPAI{s},
	}
}

// Runtime is ULTRA opened for one project.
type Runtime struct {
	*coord.Coordinator
	Server *Server
}

// Open opens ULTRA for dir. A failure here never touches Classic.
func Open(dir string) (*Runtime, error) {
	s := &Server{}
	c, err := coord.Open(dir, Adapters(s))
	if err != nil {
		return nil, err
	}
	return &Runtime{Coordinator: c, Server: s}, nil
}

// ClassicMemory fetches Classic memory items from the server for an import
// preview. It reads only; nothing in Classic changes.
func (r *Runtime) ClassicMemory(ctx context.Context, limit int) ([]knowledge.ClassicItem, error) {
	c, err := r.Server.Client(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, serverTime)
	defer cancel()
	var items []knowledge.ClassicItem
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	if err := c.Call(cctx, "GET", "/memory?"+q.Encode(), "", nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// MarshalIndent is json.MarshalIndent with two spaces, for CLI output.
func MarshalIndent(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
