// Package lockfile is a cross-process mutex on a file created exclusively.
// A holder that died leaves its file behind; a file older than the stale age
// is taken over. Holders must finish well within that age.
package lockfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// retryEvery is how often a waiter tries again.
const retryEvery = 15 * time.Millisecond

// ErrBusy is returned when the lock stays held past the wait.
var ErrBusy = errors.New("locked by another PAI process")

// Acquire takes the lock at path, waiting up to wait. It returns the release.
func Acquire(path string, wait, stale time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > stale {
			os.Remove(path) // the holder died; take over
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, path)
		}
		time.Sleep(retryEvery)
	}
}
