package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

const twoQuestions = `{"questions":[
 {"header":"Scope","question":"Which scope?","options":[{"label":"Small","description":"a"},{"label":"Big","description":"b"}]},
 {"header":"Extras","question":"Which extras?","multiSelect":true,"options":[{"label":"Docs","description":"a"},{"label":"Tests","description":"b"}]}]}`

// start runs an ask call and returns its result when it ends.
func start(h *Harness, ctx context.Context, args string) chan statefile.Record {
	out := make(chan statefile.Record, 1)
	go func() {
		out <- h.ask(ctx, "s1", statefile.Record{Header: statefile.Header{ID: "c1"}}, provider.Call{Name: "ask", Arguments: json.RawMessage(args)})
	}()
	return out
}

func waiting(t *testing.T, h *Harness) {
	t.Helper()
	for range 200 {
		h.mu.Lock()
		n := len(h.asks)
		h.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the ask never waited")
}

func TestAskAnswered(t *testing.T) {
	h := &Harness{}
	out := start(h, context.Background(), twoQuestions)
	waiting(t, h)
	if err := h.Answer("s1", Reply{Call: "c1", Answers: [][]string{{"Small"}}}); err == nil {
		t.Fatal("one answer for two questions was accepted")
	}
	if err := h.Answer("s1", Reply{Call: "c1", Answers: [][]string{{"Small", "Big"}, {"Docs"}}}); err == nil {
		t.Fatal("two answers to a single choice were accepted")
	}
	if err := h.Answer("s1", Reply{Call: "c1", Answers: [][]string{{"Small"}, {"Docs", "Tests"}}}); err != nil {
		t.Fatal(err)
	}
	want := `User has answered your questions: "Which scope?"="Small", "Which extras?"="Docs, Tests". You can now continue with the user's answers in mind.`
	if got := string((<-out).Body); got != want {
		t.Fatalf("got %q", got)
	}
	if err := h.Answer("s1", Reply{Call: "c1", Declined: true}); !errors.Is(err, ErrNoAsk) {
		t.Fatalf("an answer to nothing: %v", err)
	}
}

func TestAskDeclined(t *testing.T) {
	h := &Harness{}
	out := start(h, context.Background(), twoQuestions)
	waiting(t, h)
	if err := h.Answer("s1", Reply{Call: "c1", Declined: true}); err != nil {
		t.Fatal(err)
	}
	if got := string((<-out).Body); got != "The user declined to answer the questions." {
		t.Fatalf("got %q", got)
	}
}

func TestAskStopAndTimeout(t *testing.T) {
	h := &Harness{}
	ctx, cancel := context.WithCancel(context.Background())
	out := start(h, ctx, twoQuestions)
	waiting(t, h)
	cancel()
	if got := string((<-out).Body); got != "stopped, effect unknown" {
		t.Fatalf("got %q", got)
	}
	askTime = 20 * time.Millisecond
	defer func() { askTime = 30 * time.Minute }()
	if got := string((<-start(h, context.Background(), twoQuestions)).Body); !strings.HasPrefix(got, "error: no answer in") {
		t.Fatalf("got %q", got)
	}
	if len(h.asks) != 0 {
		t.Fatal("a finished ask is still pending")
	}
}

func TestAskLimits(t *testing.T) {
	opt := `[{"label":"a","description":""},{"label":"b","description":""}]`
	for name, args := range map[string]string{
		"none":         `{"questions":[]}`,
		"five":         `{"questions":[` + strings.Repeat(`{"header":"h","question":"q","options":`+opt+`},`, 4) + `{"header":"h","question":"q","options":` + opt + `}]}`,
		"long header":  `{"questions":[{"header":"thirteen char","question":"q","options":` + opt + `}]}`,
		"one option":   `{"questions":[{"header":"h","question":"q","options":[{"label":"a","description":""}]}]}`,
		"same label":   `{"questions":[{"header":"h","question":"q","options":[{"label":"a","description":""},{"label":"a","description":""}]}]}`,
		"no question":  `{"questions":[{"header":"h","question":" ","options":` + opt + `}]}`,
		"not an array": `{"questions":"x"}`,
	} {
		if got := string((<-start(&Harness{}, context.Background(), args)).Body); !strings.HasPrefix(got, "error: ") {
			t.Errorf("%s: got %q", name, got)
		}
	}
}
