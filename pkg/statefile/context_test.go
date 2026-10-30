package statefile

import (
	"strings"
	"testing"
)

func rec(n int, k Kind, body string) Record {
	return Record{Header: Header{N: n, Kind: k, ID: NewID()}, Body: []byte(body)}
}

// After a compaction the context is the opening, the summary, then what came
// after the cut, in order, and the step after the cut still closes its blocks.
func TestBuildAfterCompaction(t *testing.T) {
	chain := []Record{
		rec(1, Start, ""), rec(2, Config, "{}"), rec(3, System, "system prompt"),
		rec(4, Message, "old question"), rec(5, Text, "old answer"),
	}
	chain[4].Step = "s1"
	step1 := rec(6, Step, "")
	step1.ID, step1.Upto = "s1", 4
	chain = append(chain, step1, rec(7, Message, "new question"), rec(8, Text, "new answer"))
	chain[7].Step = "s2"
	comp := rec(9, Compaction, "SUMMARY of the old exchange")
	comp.Upto = 6
	step2 := rec(10, Step, "")
	step2.ID, step2.Upto = "s2", 9
	chain = append(chain, comp, step2)
	ctx, upto := Build(chain)
	if upto != 10 {
		t.Fatalf("upto %d", upto)
	}
	var got []string
	for _, r := range ctx {
		got = append(got, string(r.Kind)+":"+string(r.Body))
	}
	want := "system:system prompt|system:SUMMARY of the old exchange|message:new question|text:new answer|step:"
	if strings.Join(got, "|") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, "|"), want)
	}
}
