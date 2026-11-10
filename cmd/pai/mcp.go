package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"pai/pkg/connector"
	"pai/pkg/mcp"
)

// callTime bounds one MCP message, so one that hangs cannot hold up the ones
// behind it: a search or a write embeds one text, which a small local model
// can take a minute over on a busy machine.
const callTime = 2 * time.Minute

// doer is the part of a connector.Client the bridge uses.
type doer interface {
	Do(ctx context.Context, method, path, ctype string, body io.Reader) (*http.Response, error)
}

// bridge posts a client's messages to the server of this login. It outlives
// the server's restarts and a machine's networks, so it finds the server
// again when it cannot reach it.
type bridge struct {
	load func(context.Context) (doer, error)
	to   doer
}

func (b *bridge) find(ctx context.Context) error {
	to, err := b.load(ctx)
	if err == nil {
		b.to = to
	}
	return err
}

func (b *bridge) send(ctx context.Context, body []byte) ([]byte, error) {
	resp, err := b.to.Do(ctx, http.MethodPost, "/mcp", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	return io.ReadAll(resp.Body)
}

// neverSent is whether err means the request did not reach the server, so
// sending it again cannot apply a write twice.
func neverSent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// post sends one message, and once more by a route found anew when the last
// one is gone. Its error is in words the model and the person read.
func (b *bridge) post(ctx context.Context, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTime)
	defer cancel()
	if b.to == nil {
		if err := b.find(ctx); err != nil {
			return nil, fmt.Errorf("pai-server unreachable: %w", err)
		}
	}
	reply, err := b.send(ctx, body)
	if neverSent(err) && b.find(ctx) == nil {
		reply, err = b.send(ctx, body)
	}
	var refused *connector.StatusError
	switch {
	case errors.As(err, &refused):
		return nil, fmt.Errorf("pai-server refused the call: %w", err)
	case err != nil:
		return nil, fmt.Errorf("pai-server unreachable: %w", err)
	}
	return reply, nil
}

// serveMCP is `pai mcp`: an MCP client starts it and speaks stdio to it, and
// it forwards to the server's /mcp with this login's key. A server that is
// down now may be up by the first call, so only having no login ends it.
func serveMCP() {
	b := &bridge{load: func(ctx context.Context) (doer, error) {
		c, err := connector.Load(ctx)
		if err != nil {
			return nil, err
		}
		return c, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), startTime)
	err := b.find(ctx)
	cancel()
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "not logged in: pai login <api key>")
		os.Exit(1)
	}
	if err != nil {
		log.Printf("pai mcp: %v; trying again at each call", err)
	}
	if err := mcp.Stdio(context.Background(), os.Stdin, os.Stdout, b.post); err != nil {
		log.Fatal(err)
	}
}
