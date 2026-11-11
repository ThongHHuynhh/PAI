package worker

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Spool records are immutable. An interrupted write is retained and refuses
// replay; shell effects cannot be atomically committed with these files.
func spoolPath(root, id, kind string) string {
	return filepath.Join(root, "calls", fmt.Sprintf("%x.%s", sha256.Sum256([]byte(id)), kind))
}

func spoolPut(root string, f Frame, kind string) error {
	p := spoolPath(root, f.ID, kind)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = Encode(w, f)
	if err == nil {
		err = w.Sync()
	}
	cerr := w.Close()
	if err != nil {
		return err
	}
	return cerr
}

func spoolGet(root, id, kind string) (Frame, error) {
	f, err := os.Open(spoolPath(root, id, kind))
	if err != nil {
		return Frame{}, err
	}
	defer f.Close()
	return Decode(bufio.NewReader(f))
}

func (s *server) reconcile(id string) Frame {
	if f, err := spoolGet(s.Scratch, id, "result"); err == nil {
		return f
	}
	return Frame{Header: Header{ID: id, Kind: Uncertain}, Body: []byte("no durable terminal result; execution outcome uncertain, do not replay")}
}

// replay sends only results. Requests are never replayed, even after restart.
func (s *server) replay() error {
	paths, err := filepath.Glob(filepath.Join(s.Scratch, "calls", "*.result"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		r, err := Decode(bufio.NewReader(f))
		f.Close()
		if err != nil {
			continue
		} // torn evidence remains available for reconciliation
		if _, err := spoolGet(s.Scratch, r.ID, "receipt"); err == nil {
			continue
		}
		if err := s.send(r); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) accept(f Frame) bool {
	err := spoolPut(s.Scratch, f, "request")
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrExist) {
		_ = s.send(s.reconcile(f.ID))
	} else {
		_ = s.send(Frame{Header: Header{ID: f.ID, Kind: Error}, Body: []byte("request persistence failed: " + err.Error())})
	}
	return false
}
