package worker

import (
	"testing"
	"time"
)

func TestStuck(t *testing.T) {
	for _, c := range []struct {
		off     time.Duration
		polling bool
		moved   bool
		want    bool
	}{
		{time.Minute, true, false, false},
		{stuckPolling + time.Second, true, false, true},
		{stuckPolling + time.Second, false, false, false},
		// a laptop that woke off its dock: the poll wedges, the harness is up
		{stuckPolling + time.Second, false, true, true},
		{stuckSilent + time.Second, false, false, true},
	} {
		if got := stuck(c.off, c.polling, c.moved); got != c.want {
			t.Errorf("stuck(%s, %v, %v) = %v, want %v", c.off, c.polling, c.moved, got, c.want)
		}
	}
}
