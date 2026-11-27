package harness

import (
	"context"
	"log"
	"slices"
	"time"

	"pai/pkg/statefile"
)

// Info is what GET /sessions/{id} and each entry of GET /sessions say about a
// session: everything the tip states and its name, no cost. ContextUsed is
// the last step's prompt in tokens, against Config.ContextLimit. Updated is
// the newest record that is not a name, so a rename does not reorder a list.
type Info struct {
	Executions  []ExecutionInfo  `json:"executions,omitempty"`
	ID          string           `json:"id"`
	Parent      string           `json:"parent,omitempty"`
	Name        string           `json:"name,omitempty"`
	Status      statefile.Status `json:"status"`
	Config      statefile.Conf   `json:"config"`
	Created     time.Time        `json:"created"`
	Updated     time.Time        `json:"updated"`
	N           int              `json:"n"`
	ContextUsed int              `json:"context_used"`
	Usage       statefile.Usage  `json:"usage"` // its steps
	// Background is what the harness's own calls for the session used:
	// memory and compaction, on the session's model.
	Background statefile.Usage `json:"background"`
}

// Info reads one session's file.
func (h *Harness) Info(id string) (Info, error) {
	chain, err := h.Sessions.Load(id)
	if err != nil {
		return Info{}, err
	}
	conf, err := config(chain)
	if err != nil {
		return Info{}, err
	}
	tip := chain[len(chain)-1]
	info := Info{ID: id, Parent: conf.Parent, Status: h.status(id, chain), Config: conf, Created: chain[0].Time, N: tip.N, ContextUsed: statefile.ContextUsed(chain)}
	info.Executions = executionInfo(chain)
	for _, r := range chain {
		switch r.Kind {
		case statefile.Name:
			info.Name = string(r.Body)
		case statefile.Maintenance: // lands after the turn; must not reorder a list
		default:
			info.Updated = r.Time
		}
		if r.Usage == nil {
			continue
		}
		switch r.Kind {
		case statefile.Step:
			info.Usage.Add(*r.Usage)
		case statefile.Maintenance:
			info.Background.Add(*r.Usage)
		}
	}
	h.lockIndex()
	if !h.gone[id] { // a read that began before a delete does not bring it back
		h.index[id] = info
	}
	h.imu.Unlock()
	return info, nil
}

// lockIndex takes imu with the maps ready.
func (h *Harness) lockIndex() {
	h.imu.Lock()
	if h.index == nil {
		h.index, h.dirty, h.gone = map[string]Info{}, map[string]bool{}, map[string]bool{}
	}
}

// touch marks a session's index entry stale. Memory is a cache of the file:
// the next list reads the file again.
func (h *Harness) touch(id string) {
	h.lockIndex()
	h.dirty[id] = true
	h.imu.Unlock()
}

// List is every session, newest updated first, from the index. Entries a
// write touched since the last list are read again; the rest are served from
// memory. The mark is cleared before the read, so a write that lands during
// the read marks it again. A file that does not read is logged and left out
// until its next write. Resume fills the index on start, and touch adds a
// created session.
func (h *Harness) List() ([]Info, error) {
	h.lockIndex()
	ids := make([]string, 0, len(h.index)+len(h.dirty))
	for id := range h.index {
		ids = append(ids, id)
	}
	for id := range h.dirty {
		if _, ok := h.index[id]; !ok {
			ids = append(ids, id)
		}
	}
	h.imu.Unlock()
	infos := make([]Info, 0, len(ids))
	for _, id := range ids {
		h.imu.Lock()
		info, ok := h.index[id]
		stale := h.dirty[id]
		delete(h.dirty, id)
		if !ok && !stale || h.gone[id] {
			h.imu.Unlock()
			continue // deleted since the ids were taken
		}
		h.imu.Unlock()
		if stale {
			var err error
			if info, err = h.Info(id); err != nil {
				log.Printf("session %s: %v", id, err)
				continue
			}
		}
		infos = append(infos, info)
	}
	slices.SortFunc(infos, func(a, b Info) int { return b.Updated.Compare(a.Updated) })
	return infos, nil
}

// copyLogs writes the session's file to every worker its config lists, at
// <scratch>/logs/<id>.log, the bodies the envelope takes as media dropped so
// rg works on it. A
// worker not connected is skipped; the session's next stop writes again.
func (h *Harness) copyLogs(ctx context.Context, s *session) {
	id, n := s.id, s.exports.Add(1)
	s.logs.Lock()
	defer s.logs.Unlock()
	if s.exports.Load() != n {
		return // a newer call waits and writes a newer snapshot
	}
	chain, err := h.Sessions.Load(id)
	if err != nil {
		log.Printf("session %s: log copy: %v", id, err)
		return
	}
	conf, err := config(chain)
	if err != nil {
		log.Printf("session %s: log copy: %v", id, err)
		return
	}
	p, _, err := h.provider(conf.Model)
	if err != nil {
		log.Printf("session %s: log copy: %v", id, err)
		return
	}
	var b []byte
	for _, r := range chain {
		if p.Envelope.Accepts(r.Type) {
			r.Body = nil
		}
		enc, err := statefile.Encode(r)
		if err != nil {
			log.Printf("session %s: log copy: %v", id, err)
			return
		}
		b = append(b, enc...)
	}
	for _, w := range conf.Workers {
		r, ok := h.Workers.Get(w.Name)
		if !ok {
			continue
		}
		if err := r.Write(ctx, r.Scratch+"/logs/"+id+".log", b, ""); err != nil {
			log.Printf("session %s: log copy to %s: %v", id, w.Name, err)
		}
	}
}
