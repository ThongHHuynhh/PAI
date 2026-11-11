package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// A root worker reads and writes files as the operator, so a request reaches
// no file the operator's own code could not. For each one it runs its own
// binary as the operator, `pai file read|write <path>`, with the file on
// stdout or stdin. A worker that is its operator does the work itself.

// maxRead bounds a read. It is far over any file an agent edits or views as
// media, and far under what the worker and the harness can hold: a read of a
// huge log, a FIFO or /dev/zero fails instead of filling memory. Only the
// first maxRead+1 bytes are read, to tell a file over it.
const maxRead = 64 << 20

func (s *server) read(path string) ([]byte, error) {
	b, err := s.readHead(path)
	if err == nil && len(b) > maxRead {
		return nil, fmt.Errorf("%s is over the %d byte read limit: read parts of it with run", path, maxRead)
	}
	return b, err
}

func (s *server) readHead(path string) ([]byte, error) {
	if s.Operator == nil {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(io.LimitReader(f, maxRead+1))
	}
	cmd, err := s.file("read", path)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fail(err, &stderr)
	}
	return b, nil
}

// create opens path for writing, making its directory. The error of a write
// the operator may not make comes from Close.
func (s *server) create(path string) (io.WriteCloser, error) {
	if s.Operator == nil {
		return createFile(path)
	}
	cmd, err := s.file("write", path)
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	h := &helper{WriteCloser: in, cmd: cmd}
	cmd.Stderr = &h.stderr
	return h, cmd.Start()
}

// write replaces path with data, see Replace. Writes to one path from this
// worker take turns, so two sessions' edits cannot interleave between the
// check and the rename.
func (s *server) write(path string, data []byte, expect string) error {
	mu := pathLock(path)
	mu.Lock()
	defer mu.Unlock()
	if s.Operator == nil {
		return Replace(path, bytes.NewReader(data), expect)
	}
	cmd, err := s.file("write", path, expect)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr = bytes.NewReader(data), &stderr
	if err := cmd.Run(); err != nil {
		return fail(err, &stderr)
	}
	return nil
}

var pathLocks sync.Map // cleaned absolute path -> *sync.Mutex

func pathLock(path string) *sync.Mutex {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	mu, _ := pathLocks.LoadOrStore(filepath.Clean(path), new(sync.Mutex))
	return mu.(*sync.Mutex)
}

// Absent is the expect of a write whose file must not exist yet.
const Absent = "absent"

// Digest is the expect of a write over b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Replace writes data to a temporary file beside path and renames it over
// path, so no reader sees a torn file and a failed write leaves the old one.
// A symlink's target is replaced, not the link. With expect set, the current
// content must still be what expect names, checked just before the rename: a
// file changed by another writer since it was read fails the write instead of
// losing that change. Writers outside PAI do not take the lock, so a change in
// the last instant before the rename can still be overwritten.
func Replace(path string, data io.Reader, expect string) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".pai-*")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if err := check(path, expect); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	return nil
}

// check is whether path is still what expect names: Absent, a Digest, or
// anything when expect is empty.
func check(path, expect string) error {
	if expect == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if expect == Absent {
			return nil
		}
		return fmt.Errorf("%s was removed since it was read; read it again", path)
	case err != nil:
		return err
	case expect == Absent:
		return fmt.Errorf("%s exists; read it before replacing it", path)
	case Digest(b) != expect:
		return fmt.Errorf("%s changed since it was read; read it again and redo the edit", path)
	}
	return nil
}

func (s *server) file(op, path string, extra ...string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, append([]string{"file", op, path}, extra...)...)
	return cmd, prepare(cmd, s.Operator)
}

type helper struct {
	io.WriteCloser
	cmd    *exec.Cmd
	stderr bytes.Buffer
}

func (h *helper) Close() error {
	_ = h.WriteCloser.Close()
	if err := h.cmd.Wait(); err != nil {
		return fail(err, &h.stderr)
	}
	return nil
}

// fail is the helper's own words when it said any: they name the file and
// the reason, as a direct read or write would.
func fail(err error, stderr *bytes.Buffer) error {
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return errors.New(msg)
	}
	return err
}

// File is the helper's side, run as the operator: read copies path to stdout,
// up to the bytes that tell it over maxRead, write replaces path with stdin
// as Replace does, with expect checked.
func File(op, path, expect string) error {
	switch op {
	case "read":
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(os.Stdout, io.LimitReader(f, maxRead+1))
		return err
	case "write":
		return Replace(path, os.Stdin, expect)
	}
	return fmt.Errorf("file: unknown op %q", op)
}

// createFile makes path's directory and opens path for writing, 0644 as the
// worker's writes have always been.
func createFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}
