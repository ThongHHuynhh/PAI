package worker

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n > 0 {
		r.n--
		return copy(p, "partial "), nil
	}
	return 0, errors.New("disk gone")
}

func only(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestReplaceFailedWriteKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(p, &failingReader{n: 3}, ""); err == nil {
		t.Fatal("a failed write succeeded")
	}
	if b, _ := os.ReadFile(p); string(b) != "old" {
		t.Fatalf("old file torn: %q", b)
	}
	if n := only(t, dir); len(n) != 1 {
		t.Fatalf("temporary file left behind: %v", n)
	}
}

func TestReplaceRefusesChangedOrExistingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	_ = os.WriteFile(p, []byte("read"), 0o644)
	read := Digest([]byte("read"))
	_ = os.WriteFile(p, []byte("changed by someone else"), 0o644)
	if err := Replace(p, strings.NewReader("mine"), read); err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("stale write not refused: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "changed by someone else" {
		t.Fatalf("other writer's change lost: %q", b)
	}
	if err := Replace(p, strings.NewReader("mine"), Absent); err == nil {
		t.Fatal("create over an existing file allowed")
	}
	if err := Replace(filepath.Join(dir, "new", "g.txt"), strings.NewReader("x"), Absent); err != nil {
		t.Fatal(err)
	}
	if err := Replace(p, strings.NewReader("mine"), Digest([]byte("changed by someone else"))); err != nil {
		t.Fatal(err)
	}
}

// Two sessions editing one file through the same worker: every edit reads,
// changes and writes with expect. None may be lost.
func TestConcurrentEditsLoseNothing(t *testing.T) {
	s := &server{}
	p := filepath.Join(t.TempDir(), "list.txt")
	_ = os.WriteFile(p, nil, 0o644)
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				b, err := s.read(p)
				if err != nil {
					t.Error(err)
					return
				}
				next := append(append([]byte{}, b...), byte('a'+i%26), '\n')
				err = s.write(p, next, Digest(b))
				if err == nil {
					return
				}
				if !strings.Contains(err.Error(), "changed since") {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(p)
	if got := strings.Count(string(b), "\n"); got != n {
		t.Fatalf("%d of %d edits kept", got, n)
	}
}

// TestHelperCrashWriter is the child of TestWriterCrashKeepsOldFile: it
// starts a replace and blocks until it is killed.
func TestHelperCrashWriter(t *testing.T) {
	p := os.Getenv("PAI_CRASH_FILE")
	if p == "" {
		t.Skip("helper process only")
	}
	r, w := io.Pipe()
	go func() { _, _ = w.Write([]byte("half of the new content")); os.Stdout.WriteString("started\n") }()
	_ = Replace(p, r, "") // never returns: the pipe never closes
}

// A writer killed mid-write, as a worker whose machine loses power, leaves
// the old file whole, and the next write still succeeds.
func TestWriterCrashKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	_ = os.WriteFile(p, []byte("old"), 0o644)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashWriter$")
	cmd.Env = append(os.Environ(), "PAI_CRASH_FILE="+p)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	deadline := time.Now().Add(10 * time.Second)
	for got := ""; !strings.Contains(got, "started"); {
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		k, err := out.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got += string(buf[:k])
	}
	time.Sleep(100 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if b, _ := os.ReadFile(p); string(b) != "old" {
		t.Fatalf("crash tore the file: %q", b)
	}
	if err := Replace(p, strings.NewReader("new"), Digest([]byte("old"))); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("write after crash: %q", b)
	}
}
