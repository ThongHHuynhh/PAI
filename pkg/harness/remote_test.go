package harness

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"pai/pkg/worker"
)

func testRemote(t *testing.T) (*Remote, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	r := &Remote{conn: a, calls: map[string]chan worker.Frame{}, left: map[string]chan struct{}{}, done: make(chan struct{})}
	go r.recv(bufio.NewReader(a))
	t.Cleanup(func() { a.Close(); b.Close() })
	return r, b
}

func TestRemoteRunServerDeadline(t *testing.T) {
	r, peer := testRemote(t)
	go func() { _, _ = worker.Decode(bufio.NewReader(peer)) }()
	start := time.Now()
	_, err := r.Run(context.Background(), "s", "bash", "hang", 1)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("worker silence escaped server deadline")
	}
}

func TestRemoteWriterAdmissionCancelled(t *testing.T) {
	r, _ := testRemote(t)
	r.writeOnce.Do(func() { r.writer = make(chan struct{}, 1) })
	r.writer <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Read(ctx, "unused")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("writer admission ignored cancellation")
	}
}

func TestRemoteBlockedConsumerDoesNotHoldMap(t *testing.T) {
	r, peer := testRemote(t)
	r.cmu.Lock()
	r.calls["slow"] = make(chan worker.Frame) // simulate a consumer that stopped
	r.left["slow"] = make(chan struct{})
	r.cmu.Unlock()
	go worker.Encode(peer, worker.Frame{Header: worker.Header{ID: "slow", Kind: worker.Output}})
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("recv blocked on consumer")
	}
	r.cmu.Lock()
	r.cmu.Unlock()
}

func TestRemoteAckAndDuplicateTerminal(t *testing.T) {
	r, peer := testRemote(t)
	r.Reliable = true
	go func() {
		f, err := worker.Decode(bufio.NewReader(peer))
		if err != nil {
			return
		}
		for _, kind := range []string{worker.Acknowledged, worker.Started, worker.Result, worker.Result} {
			if worker.Encode(peer, worker.Frame{Header: worker.Header{ID: f.ID, Kind: kind, Exit: 7}}) != nil {
				return
			}
		}
	}()
	o, err := r.Run(context.Background(), "s", "bash", "x", 1)
	if err != nil || o.Exit != 7 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestRemoteDisconnectIsUncertain(t *testing.T) {
	r, peer := testRemote(t)
	go func() { _, _ = worker.Decode(bufio.NewReader(peer)); peer.Close() }()
	err := r.Write(context.Background(), "file", []byte("mutation"))
	if err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("%v", err)
	}
}
