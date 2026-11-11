package worker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSpoolNeverRewritesRequest(t *testing.T) {
	dir := t.TempDir()
	f := Frame{Header: Header{ID: "call", Kind: Write}, Body: []byte("first")}
	if err := spoolPut(dir, f, "request"); err != nil {
		t.Fatal(err)
	}
	f.Body = []byte("second")
	if err := spoolPut(dir, f, "request"); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	got, err := spoolGet(dir, f.ID, "request")
	if err != nil || string(got.Body) != "first" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestSpoolRestartReconciliation(t *testing.T) {
	dir := t.TempDir()
	f := Frame{Header: Header{ID: "call", Kind: Run}}
	if err := spoolPut(dir, f, "request"); err != nil {
		t.Fatal(err)
	}
	s := &server{Local: Local{Scratch: dir}}
	if got := s.reconcile(f.ID); got.Kind != Uncertain {
		t.Fatalf("%+v", got)
	}
	f.Kind, f.Body = Result, []byte("durable output")
	if err := spoolPut(dir, f, "result"); err != nil {
		t.Fatal(err)
	}
	restarted := &server{Local: Local{Scratch: dir}}
	if got := restarted.reconcile(f.ID); got.Kind != Result || string(got.Body) != "durable output" {
		t.Fatalf("%+v", got)
	}
}

func TestSpoolTornResultRemainsUncertain(t *testing.T) {
	dir := t.TempDir()
	f := Frame{Header: Header{ID: "call", Kind: Run}}
	if err := spoolPut(dir, f, "request"); err != nil {
		t.Fatal(err)
	}
	p := spoolPath(dir, f.ID, "result")
	if err := os.WriteFile(p, []byte("{torn"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &server{Local: Local{Scratch: dir}}
	if got := s.reconcile(f.ID); got.Kind != Uncertain {
		t.Fatalf("%+v", got)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "{torn" {
		t.Fatal("history changed")
	}
}

func TestInterpreterAdmissionCancellation(t *testing.T) {
	var lock contextLock
	if err := lock.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := lock.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
