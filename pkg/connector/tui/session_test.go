package tui

import (
	"testing"

	"pai/pkg/statefile"
)

func TestModelNoteFollowsConfigRecords(t *testing.T) {
	s := &session{transcript: NewScrollbox()}
	for i, model := range []string{"one/model", "one/model", "two/model", "two/model"} {
		kind := statefile.Config
		if i == 0 {
			kind = statefile.Start
		}
		s.show(statefile.Record{Header: statefile.Header{Kind: kind}, Body: []byte(`{"model":"` + model + `"}`)})
	}
	if len(s.transcript.items) != 1 {
		t.Fatalf("want one model note, got %d", len(s.transcript.items))
	}
	b, ok := s.transcript.items[0].b.(*textBlock)
	if !ok || b.s != "model: one/model -> two/model, from the next step" {
		t.Fatalf("wrong model note: %+v", s.transcript.items[0].b)
	}
}
