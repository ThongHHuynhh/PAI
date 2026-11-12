package harness

import (
	"bufio"
	"net"
	"strings"
	"testing"

	"pai/pkg/worker"
)

// A worker replays unacknowledged results the moment it connects, and its
// spool refuses a request id it has seen. The update request has its own id,
// and only the answer with that id counts: a replayed Done before it is not
// the update succeeding. This was the v0.4.x loop where a worker said
// "updated, restarting" every five seconds and never updated.
func TestUpdateWaitsForItsOwnAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply func(id string) []worker.Frame
		want  string
	}{
		{"replayed result first, then the real answer", func(id string) []worker.Frame {
			return []worker.Frame{
				{Header: worker.Header{ID: "old-call", Kind: worker.Done}},
				{Header: worker.Header{ID: id, Kind: worker.Acknowledged}},
				{Header: worker.Header{ID: id, Kind: worker.Done}},
			}
		}, "updated, restarting"},
		{"replayed result first, then the update fails", func(id string) []worker.Frame {
			return []worker.Frame{
				{Header: worker.Header{ID: "old-call", Kind: worker.Done}},
				{Header: worker.Header{ID: id, Kind: worker.Error}, Body: []byte("download failed")},
			}
		}, "update failed: download failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, w := net.Pipe()
			defer h.Close()
			go func() {
				defer w.Close()
				f, err := worker.Decode(bufio.NewReader(w))
				if err != nil || f.Kind != worker.Update || f.ID == "" {
					t.Errorf("update request %+v %v: it needs an id of its own", f.Header, err)
					return
				}
				for _, r := range c.reply(f.ID) {
					if err := worker.Encode(w, r); err != nil {
						return
					}
				}
			}()
			err := update(h, bufio.NewReader(h))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}
