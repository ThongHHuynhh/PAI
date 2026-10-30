package term

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// records is s typed as a console window types it: a win32 key record down
// and up for each character, Enter for each line break.
func records(s string) string {
	var b strings.Builder
	for _, r := range s {
		vk, uc := int(r), int(r)
		if r == '\n' {
			vk, uc = 13, 13
		} else if r >= 'a' && r <= 'z' {
			vk = int(r - 'a' + 'A')
		}
		fmt.Fprintf(&b, "\x1b[%d;0;%d;1;0;1_\x1b[%d;0;%d;0;0;1_", vk, uc, vk, uc)
	}
	return b.String()
}

func feed(chunks ...string) []Event {
	var d decoder
	var evs []Event
	for _, c := range chunks {
		d.feed([]byte(c), func(e Event) { evs = append(evs, e) })
	}
	d.timeout(func(e Event) { evs = append(evs, e) })
	return evs
}

func TestBurstPaste(t *testing.T) {
	for _, c := range []struct {
		name   string
		chunks []string
		want   []Event
	}{
		{"console paste of two lines", []string{records("ab\ncd")}, []Event{Paste{Text: "ab\ncd"}}},
		{"console paste ending in a line break", []string{records("long enough\n")}, []Event{Paste{Text: "long enough\n"}}},
		{"plain bytes, as without win32 records", []string{"one\rtwo"}, []Event{Paste{Text: "one\ntwo"}}},
		{"a key a read is typing", []string{records("h"), records("i"), records("\n")}, []Event{Key{Code: 'h'}, Key{Code: 'i'}, Key{Code: KeyEnter}}},
		{"a short line and Enter in one read is typing", []string{"hi\r"}, []Event{Key{Code: 'h'}, Key{Code: 'i'}, Key{Code: KeyEnter}}},
		{"a line with no break is keys", []string{records("hello there")}, nil},
		{"a Ctrl key is not a paste", []string{"ab\x16\rcd"}, nil},
	} {
		got := feed(c.chunks...)
		if c.want == nil {
			for _, e := range got {
				if _, ok := e.(Paste); ok {
					t.Errorf("%s: became a paste: %v", c.name, got)
				}
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
	}
}

func TestBracketedPasteStays(t *testing.T) {
	got := feed("\x1b[200~a\rb\x1b[201~")
	if want := []Event{Paste{Text: "a\nb"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}
