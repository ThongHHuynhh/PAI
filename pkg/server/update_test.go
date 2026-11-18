package server

import (
	"strings"
	"testing"
)

func TestRefuseDowngrade(t *testing.T) {
	for _, c := range []struct {
		v, cur        string
		named, down   bool
		wantErr, skip bool
	}{
		{"v0.5.0", "v0.5.0-beta.3", true, false, false, false},       // forward
		{"v0.4.6", "v0.5.0-beta.3", false, false, false, true},       // latest stable behind a beta: nothing
		{"v0.4.6", "v0.5.0-beta.3", true, false, true, false},        // named rollback refused
		{"v0.4.6", "v0.5.0-beta.3", true, true, false, false},        // allowed
		{"v0.5.0-beta.3", "v0.5.0-beta.3", true, false, false, true}, // same
		{"v0.5.0-beta.10", "v0.5.0-beta.3", true, false, false, false},
	} {
		skip, err := refuseDowngrade(c.v, c.cur, c.named, c.down)
		if (err != nil) != c.wantErr || skip != c.skip {
			t.Errorf("%+v: skip %v, %v", c, skip, err)
		}
		if err != nil && !strings.Contains(err.Error(), "--allow-downgrade") {
			t.Errorf("the refusal does not say how to roll back: %v", err)
		}
	}
}
