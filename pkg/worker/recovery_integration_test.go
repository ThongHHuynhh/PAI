package worker

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func wireWorker(t *testing.T, dir string) (net.Conn, *bufio.Reader, <-chan error) {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(a, Local{Scratch: dir}) }()
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(b)
	f, e := Decode(br)
	if e != nil || f.Kind != Hello {
		t.Fatalf("hello: %+v %v", f, e)
	}
	t.Cleanup(func() { b.Close(); a.Close() })
	return b, br, done
}
func terminalOnWire(t *testing.T, br *bufio.Reader) Frame {
	t.Helper()
	for {
		f, e := Decode(br)
		if e != nil {
			t.Fatal(e)
		}
		if f.Kind != Acknowledged && f.Kind != Started && f.Kind != Output {
			return f
		}
	}
}

func TestDisconnectRestartAndDuplicateMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "effect.txt")
	conn, br, done := wireWorker(t, dir)
	request := Frame{Header: Header{ID: "stable-call", Kind: Write, Path: path, Session: "s", Origin: "c"}, Body: []byte("original effect")}
	if e := Encode(conn, request); e != nil {
		t.Fatal(e)
	}
	result := terminalOnWire(t, br)
	if result.Kind != Done {
		t.Fatalf("%+v", result)
	}
	// Drop the receipt and restart the entire connection server.
	conn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disconnect hung")
	}
	if e := os.WriteFile(path, []byte("external change"), 0600); e != nil {
		t.Fatal(e)
	}
	conn2, br2, _ := wireWorker(t, dir)
	replay := terminalOnWire(t, br2)
	if replay.ID != request.ID || replay.Kind != Done {
		t.Fatalf("lost terminal result %+v", replay)
	}
	if e := Encode(conn2, Frame{Header: Header{ID: request.ID, Kind: Receipt}}); e != nil {
		t.Fatal(e)
	}
	// Identical delivery must report its saved result, never repeat its effect.
	if e := Encode(conn2, request); e != nil {
		t.Fatal(e)
	}
	duplicate := terminalOnWire(t, br2)
	if duplicate.Kind != Done {
		t.Fatalf("%+v", duplicate)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "external change" {
		t.Fatal("mutation replayed", string(b), e)
	}
}

func TestRestartWithRequestButNoResultIsUncertain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "must-not-exist")
	f := Frame{Header: Header{ID: "interrupted", Kind: Write, Path: path}, Body: []byte("unsafe replay")}
	if e := spoolPut(dir, f, "request"); e != nil {
		t.Fatal(e)
	}
	conn, br, _ := wireWorker(t, dir)
	if e := Encode(conn, f); e != nil {
		t.Fatal(e)
	}
	r := terminalOnWire(t, br)
	if r.Kind != Uncertain {
		t.Fatalf("%+v", r)
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("uncertain request executed", e)
	}
}
