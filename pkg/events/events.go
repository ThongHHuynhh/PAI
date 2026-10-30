// Package events is the server's live feed: what the web graph and any other
// connector watch. Publishing never blocks: a subscriber that falls behind
// loses events, and its next snapshot catches it up.
package events

import (
	"sync"
	"time"
)

// buffer is how many events a subscriber may fall behind by.
const buffer = 256

type Event struct {
	Type    string    `json:"type"` // record, worker, memory, session
	At      time.Time `json:"at"`
	Session string    `json:"session,omitempty"`
	Worker  string    `json:"worker,omitempty"`
	Data    any       `json:"data,omitempty"`
}

type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]bool
}

func New() *Bus { return &Bus{subs: map[chan Event]bool{}} }

// Publish sends e to every subscriber that has room for it.
func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.subs {
		select {
		case c <- e:
		default:
		}
	}
}

// Subscribe is a channel of events until cancel.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	c := make(chan Event, buffer)
	b.mu.Lock()
	b.subs[c] = true
	b.mu.Unlock()
	return c, func() {
		b.mu.Lock()
		delete(b.subs, c)
		b.mu.Unlock()
	}
}
