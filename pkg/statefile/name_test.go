package statefile

import (
	"strings"
	"testing"
)

func TestNewName(t *testing.T) {
	for _, list := range [][]string{adjectives, nouns} {
		seen := map[string]bool{}
		for _, w := range list {
			if seen[w] {
				t.Errorf("%q twice", w)
			}
			seen[w] = true
		}
	}
	for range 1000 {
		if id := NewName(func(string) bool { return false }); !ValidID(id) || strings.Count(id, "-") != 1 {
			t.Fatalf("NewName made %q", id)
		}
	}
	// every two-word name taken: a number is added
	id := NewName(func(id string) bool { return strings.Count(id, "-") == 1 })
	if !ValidID(id) || strings.Count(id, "-") != 2 {
		t.Fatalf("with every pair taken, NewName made %q", id)
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"calm-otter", "swift-falcon-12", "b87de877"} {
		if !ValidID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", "calm", "Calm-otter", "calm_otter", "../etc", "calm-otter/x", "b87de87", "calm-otter-12345", "a-otter"} {
		if ValidID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}
