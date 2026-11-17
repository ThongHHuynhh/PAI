// Package lockfile is a cross-process mutex on a file created exclusively.
//
// The file holds the holder's pid and a random token. Release removes it
// only while it still holds that token. A lock whose holder process is gone
// is taken over at once; one older than the stale age is taken over too, as
// a last resort for a holder that hung. Takeover renames the old file to a
// unique name first, so of two waiters that both judge it stale, only one
// removes it. Holders must finish well within the stale age.
package lockfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// retryEvery is how often a waiter tries again.
const retryEvery = 15 * time.Millisecond

// ErrBusy is returned when the lock stays held past the wait.
var ErrBusy = errors.New("locked by another PAI process")

func token() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Acquire takes the lock at path, waiting up to wait. It returns the release.
func Acquire(path string, wait, stale time.Duration) (func(), error) {
	l, err := Take(path, wait, stale)
	if err != nil {
		return nil, err
	}
	return l.Release, nil
}

// Take takes the lock at path, waiting up to wait.
func Take(path string, wait, stale time.Duration) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	mine := fmt.Sprintf("%d %s\n", os.Getpid(), token())
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(mine)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				os.Remove(path)
				return nil, errors.Join(werr, cerr)
			}
			return &Lock{path: path, mine: mine}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if seen, ok := abandoned(path, stale); ok {
			takeOver(path, seen)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, path)
		}
		time.Sleep(retryEvery)
	}
}

// abandoned is whether the lock's holder is gone or past the stale age. A
// file still being written (no content yet) is young and not abandoned.
// It returns the content it judged, so a takeover removes only that.
func abandoned(path string, stale time.Duration) (string, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	if time.Since(st.ModTime()) > stale {
		return string(b), true
	}
	pidText, _, ok := strings.Cut(string(b), " ")
	pid, err := strconv.Atoi(pidText)
	if !ok || err != nil || pid == os.Getpid() {
		return "", false
	}
	return string(b), !alive(pid)
}

// guardStale is how old a takeover guard may get before it is presumed
// left by a crash: a takeover is a read and a rename.
const guardStale = 5 * time.Second

// takeOver moves an abandoned lock aside. Takeovers are serialized by a
// guard file, and the lock is moved only if it still holds what was judged
// abandoned: a waiter never removes a lock someone else just took.
func takeOver(path, seen string) {
	guard := path + ".takeover"
	g, err := os.OpenFile(guard, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if st, serr := os.Stat(guard); serr == nil && time.Since(st.ModTime()) > guardStale {
			os.Remove(guard)
		}
		time.Sleep(retryEvery)
		return
	}
	g.Close()
	defer os.Remove(guard)
	if cur, err := os.ReadFile(path); err != nil || string(cur) != seen {
		return
	}
	aside := path + ".stale-" + token()
	if os.Rename(path, aside) == nil {
		os.Remove(aside)
	}
}

// releaseTries bounds retries of a remove that fails while another process
// reads the file: Windows refuses to delete a file open elsewhere.
const releaseTries = 200

// release removes the lock if it is still ours.
func release(path, mine string) {
	for range releaseTries {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != mine {
			return
		}
		if os.Remove(path) == nil {
			return
		}
		time.Sleep(retryEvery / 3)
	}
}

// Lock is a held lock that can check it is still held.
type Lock struct {
	path, mine string
}

// Held is whether the lock file still holds this holder's token: false
// after a takeover. Check it right before a write that must be exclusive.
func (l *Lock) Held() bool {
	b, err := os.ReadFile(l.path)
	return err == nil && string(b) == l.mine
}

// Release removes the lock if it is still ours.
func (l *Lock) Release() { release(l.path, l.mine) }
