package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestMemorySetupRequiresOptIn(t *testing.T) {
	for _, tc := range []struct {
		answer string
		local  bool
	}{{"yes\n", true}, {"Y\n", true}, {"no\n", false}, {"\n", false}, {"", false}, {"maybe\nyes\n", true}} {
		var out bytes.Buffer
		c, err := memorySetup(strings.NewReader(tc.answer), &out)
		if err != nil || c.Local != tc.local {
			t.Fatalf("answer %q: config=%+v error=%v", tc.answer, c, err)
		}
		if !tc.local && c.Mode != "hash" {
			t.Fatalf("decline should require no runtime: %+v", c)
		}
		if tc.local && c.Mode != "semantic" {
			t.Fatalf("opt-in should use semantic search: %+v", c)
		}
		if !strings.Contains(out.String(), "data folder") {
			t.Fatal("download location was not explained")
		}
	}
}
