package worker

import (
	"testing"
	"time"
)

func TestStuck(t *testing.T) {
	for _, c := range []struct {
		off     time.Duration
		polling bool
		want    bool
	}{
		{time.Minute, true, false},
		{stuckPolling + time.Second, true, true},
		{stuckPolling + time.Second, false, false},
		{stuckSilent + time.Second, false, true},
	} {
		if got := stuck(c.off, c.polling); got != c.want {
			t.Errorf("stuck(%s, %v) = %v, want %v", c.off, c.polling, got, c.want)
		}
	}
}
