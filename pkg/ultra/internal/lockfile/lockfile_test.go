package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusiveAndRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	a, err := Take(p, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Take(p, 50*time.Millisecond, time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("second holder: %v", err)
	}
	a.Release()
	b, err := Take(p, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a.Release() // a stale release must not free b's lock
	if !b.Held() {
		t.Fatal("old holder's release removed the new lock")
	}
}

func TestDeadHolderIsTakenOver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	// A pid that is not running (very high, unused) holding the lock.
	os.WriteFile(p, []byte("999999 deadbeef\n"), 0o600)
	l, err := Take(p, time.Second, time.Hour)
	if err != nil {
		t.Fatalf("dead holder kept the lock: %v", err)
	}
	l.Release()
}

func TestStaleAgeTakeoverFencesOldHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	old, _ := Take(p, time.Second, time.Hour)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(p, past, past)
	n, err := Take(p, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if old.Held() || !n.Held() {
		t.Fatal("old holder still believes it holds the lock")
	}
}
