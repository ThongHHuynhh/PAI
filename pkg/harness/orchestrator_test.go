package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// A fake step should finish immediately; bound waits to catch lifecycle bugs.
const orchestrationWait = 5 * time.Second

// No real model is called; the ordinary envelope still compiles every turn.
func orchestrationHarness(t *testing.T, run func(context.Context, string, func(statefile.Record)) (statefile.Header, error)) *Harness {
	t.Helper()
	p := provider.Anthropic("", "")
	p.Run = func(ctx context.Context, body []byte, emit func(statefile.Record)) (statefile.Header, error) {
		var req struct{ Model string }
		if err := json.Unmarshal(body, &req); err != nil {
			return statefile.Header{}, err
		}
		return run(ctx, req.Model, emit)
	}
	return &Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}, Workers: NewPool(), Providers: map[string]provider.Provider{"fake": p}}
}

func orchestrationSession(t *testing.T, h *Harness, parent, model, tools string) string {
	t.Helper()
	id, err := h.Create(context.Background(), Create{Parent: parent, Model: "fake/" + model, ContextLimit: 100000, Tools: tools, Name: model}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitSession(t *testing.T, h *Harness, id string) {
	t.Helper()
	s := h.session(id)
	h.mu.Lock()
	done := s.done
	h.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(orchestrationWait):
			t.Fatal("session did not finish: " + id)
		}
	}
}

func TestChildReports(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			h := orchestrationHarness(t, func(ctx context.Context, model string, emit func(statefile.Record)) (statefile.Header, error) {
				if model != "parent" {
					emit(statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain"}, Body: []byte(strings.Repeat("final reply ", 500))})
					if failed {
						return statefile.Header{}, errors.New("fake failure")
					}
				}
				return statefile.Header{Stop: "end_turn", Usage: &statefile.Usage{Input: 100}}, nil
			})
			parent := orchestrationSession(t, h, "", "parent", "orchestrator")
			var children []string
			for range 2 {
				child := orchestrationSession(t, h, parent, "child", "default")
				children = append(children, child)
				msg := statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "session:" + parent}, Body: []byte("do the task")}
				if err := h.note(h.session(child), child, msg); err != nil {
					t.Fatal(err)
				}
				st, err := h.Run(context.Background(), child)
				want := statefile.StatusClosed
				if failed {
					want = statefile.StatusFailed
				}
				if err != nil || st != want {
					t.Fatalf("child: %s %v", st, err)
				}
				waitSession(t, h, parent)
				// A second run at the same tip must not append a second report.
				if _, err := h.Run(context.Background(), child); err != nil {
					t.Fatal(err)
				}
				waitSession(t, h, parent)
			}
			chain, err := h.Sessions.Load(parent)
			if err != nil {
				t.Fatal(err)
			}
			reports := 0
			for _, r := range chain {
				if r.Kind != statefile.Message || !strings.HasPrefix(r.Via, "session:") {
					continue
				}
				reports++
				_, reply, ok := strings.Cut(string(r.Body), "\n")
				if !ok || len(reply) > reportBytes || !strings.Contains(reply, "final reply") || !strings.HasPrefix(r.IdempotencyKey, "done-") {
					t.Fatalf("bad report: %+v", r)
				}
				outcome := "finished"
				if failed {
					outcome = "failed"
				}
				if !strings.Contains(string(r.Body), "child "+strings.TrimPrefix(r.Via, "session:")+" child "+outcome+" after ") {
					t.Fatalf("bad report header: %s", r.Body)
				}
			}
			if reports != len(children) || statefile.LastStep(chain) == nil || statefile.Derive(chain) != statefile.StatusClosed {
				t.Fatalf("reports=%d, parent did not consume both reports", reports)
			}
		})
	}
}

func TestCascadeStop(t *testing.T) {
	started := make(chan struct{}, 3)
	h := orchestrationHarness(t, func(ctx context.Context, model string, emit func(statefile.Record)) (statefile.Header, error) {
		started <- struct{}{}
		<-ctx.Done()
		return statefile.Header{}, ctx.Err()
	})
	parent := orchestrationSession(t, h, "", "parent", "orchestrator")
	child := orchestrationSession(t, h, parent, "child", "default")
	other := orchestrationSession(t, h, parent, "other", "default")
	idle := orchestrationSession(t, h, parent, "idle", "default")
	for _, id := range []string{parent, child, other} {
		if _, err := h.Post(context.Background(), id, "test", "start", []Part{{Type: "text/plain", Body: []byte("work")}}); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(orchestrationWait):
			t.Fatal("provider did not start")
		}
	}
	// A direct stop_child is silent and identifies the parent as the actor.
	if err := h.Stop(context.Background(), child, "session:"+parent); err != nil {
		t.Fatal(err)
	}
	waitSession(t, h, child)
	// A connector stopping the parent cascades the same via to running children.
	if err := h.Stop(context.Background(), parent, "test"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent, other} {
		waitSession(t, h, id)
		chain, err := h.Sessions.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		tip := chain[len(chain)-1]
		if tip.Kind != statefile.Error || tip.Via != "test" || statefile.Derive(chain) != statefile.StatusFailed {
			t.Fatalf("%s was not stopped: %+v", id, tip)
		}
	}
	info, err := h.Info(idle)
	if err != nil || info.Status != statefile.StatusClosed {
		t.Fatalf("idle child changed: %+v, %v", info, err)
	}
	chain, err := h.Sessions.Load(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range chain {
		if r.Kind == statefile.Message && strings.HasPrefix(r.Via, "session:") {
			t.Fatalf("stop woke parent with report: %s", r.Body)
		}
	}
}

func TestDelegateRoutes(t *testing.T) {
	release := make(chan struct{})
	var input atomic.Int64
	input.Store(100)
	h := orchestrationHarness(t, func(ctx context.Context, model string, emit func(statefile.Record)) (statefile.Header, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return statefile.Header{}, ctx.Err()
		}
		emit(statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain"}, Body: []byte("work completed")})
		return statefile.Header{Stop: "end_turn", Usage: &statefile.Usage{Input: int(input.Load())}}, nil
	})
	parent := orchestrationSession(t, h, "", "parent", "orchestrator")
	if _, err := h.setConf(parent, "test", func(c *statefile.Conf, _ []statefile.Record) error {
		c.Params = map[string]json.RawMessage{"max_tokens": json.RawMessage(`4096`)}
		c.Input = []string{"image/png"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, _ := h.Info(parent)
	a := orchestratorArgs{Name: "router", Task: "Improve delegation routing", Files: []string{"pkg/harness/route.go"}}
	send := func(key, action string) string {
		t.Helper()
		out, err := h.dispatch(context.Background(), parent, info.Config, key, a)
		if err != nil || !strings.HasPrefix(out, action+" child ") {
			t.Fatalf("want %s, got %q %v", action, out, err)
		}
		return strings.TrimSuffix(strings.Fields(out)[2], ":")
	}
	child := send("spawn", "spawn")
	queued := send("queue", "queue")
	if queued != child || send("queue", "queue") != child {
		t.Fatal("related work did not queue to the same child")
	}
	close(release)
	waitSession(t, h, child)
	waitSession(t, h, parent)
	chain, err := h.Sessions.Load(child)
	if err != nil {
		t.Fatal(err)
	}
	queuedMessages := 0
	for _, r := range chain {
		if r.Kind == statefile.Message && r.IdempotencyKey == "queue" {
			queuedMessages++
			if !strings.HasPrefix(string(r.Body), "QUEUED TASK.") || r.Via != "session:"+parent {
				t.Fatalf("bad queued message: %+v", r)
			}
		}
	}
	if queuedMessages != 1 {
		t.Fatalf("duplicate queued messages: %d", queuedMessages)
	}
	input.Store(70000)
	if send("resume", "resume") != child {
		t.Fatal("related work did not resume the same child")
	}
	waitSession(t, h, child)
	waitSession(t, h, parent)
	fresh := send("handoff", "spawn")
	waitSession(t, h, fresh)
	waitSession(t, h, parent)
	chain, err = h.Sessions.Load(fresh)
	if err != nil {
		t.Fatal(err)
	}
	conf, _ := config(chain)
	if fresh == child || conf.Parent != parent || conf.Model != info.Config.Model || conf.ContextLimit != info.Config.ContextLimit || conf.Tools != "default" {
		t.Fatalf("bad fresh child config: %+v", conf)
	}
	if string(conf.Params["max_tokens"]) != "4096" || len(conf.Input) != 1 || conf.Input[0] != "image/png" {
		t.Fatalf("child lost provider options: %+v", conf)
	}
	for _, r := range chain {
		if r.Kind == statefile.Message {
			if !strings.Contains(string(r.Body), "Related work already done by session "+child) {
				t.Fatalf("missing handoff: %s", r.Body)
			}
			if !strings.Contains(string(r.Body), "work completed") {
				t.Fatalf("handoff lost final reply: %s", r.Body)
			}
			break
		}
	}
}

func TestChildDeliveryDuringStop(t *testing.T) {
	h := orchestrationHarness(t, func(ctx context.Context, model string, emit func(statefile.Record)) (statefile.Header, error) {
		<-ctx.Done()
		return statefile.Header{}, ctx.Err()
	})
	parent := orchestrationSession(t, h, "", "parent", "orchestrator")
	child := orchestrationSession(t, h, parent, "child", "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.OnRecord = func(id string, r statefile.Record) {
		if id == child && r.Kind == statefile.Message {
			cancel() // a stop landed after delivery, before Kick returns
		}
	}
	if err := h.postChild(ctx, parent, child, "racing", "task"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delivery: %v", err)
	}
	waitSession(t, h, child)
	info, err := h.Info(parent)
	if err != nil || info.Status != statefile.StatusClosed {
		t.Fatalf("stop report woke parent: %+v %v", info, err)
	}
}

func TestOrchestratorBoundary(t *testing.T) {
	h := orchestrationHarness(t, func(context.Context, string, func(statefile.Record)) (statefile.Header, error) {
		return statefile.Header{Stop: "end_turn"}, nil
	})
	parent := orchestrationSession(t, h, "", "parent", "orchestrator")
	child := orchestrationSession(t, h, parent, "child", "default")
	other := orchestrationSession(t, h, "", "other", "default")
	for _, c := range []Create{
		{Parent: child, Model: "fake/child", ContextLimit: 1000},
		{Parent: parent, Model: "fake/child", ContextLimit: 1000, Tools: "orchestrator"},
		{Parent: "1234abcd", Model: "fake/child", ContextLimit: 1000},
	} {
		if _, err := h.Create(context.Background(), c, "test"); err == nil {
			t.Fatalf("invalid parent accepted: %+v", c)
		}
	}
	chain, err := h.Sessions.Load(parent)
	if err != nil {
		t.Fatal(err)
	}
	if last(chain, "orchestrator") != orchestratorSystem {
		t.Fatal("missing orchestrator instructions")
	}
	conf, _ := config(chain)
	for _, tool := range []string{"tell", "stop_child"} {
		args, _ := json.Marshal(orchestratorArgs{ID: other, Text: "hello"})
		r := h.orchestratorCall(context.Background(), parent, conf, statefile.Record{}, provider.Call{Name: tool, Arguments: args})
		if !strings.Contains(string(r.Body), "not your child") {
			t.Fatalf("%s accepted an unrelated session: %s", tool, r.Body)
		}
	}
	info, err := h.Info(child)
	if err != nil || info.Parent != parent || info.Config.Parent != parent {
		t.Fatalf("parent not exposed: %+v %v", info, err)
	}
	// Claude Code accepts the same supplied tool set, without a name allowlist.
	p := provider.ClaudeCode("")
	body, _, err := p.Envelope.Compile(provider.Request{Model: "test", Tools: Tools("orchestrator", nil), Context: []statefile.Record{system("harness", "test")}})
	if err != nil {
		t.Fatal(err)
	}
	for tool := range orchestratorTools {
		if !strings.Contains(string(body), `"name":"`+tool+`"`) {
			t.Fatalf("Claude Code missing tool %s", tool)
		}
	}
}

func TestRoomFor(t *testing.T) {
	h := orchestrationHarness(t, nil)
	h.Providers["other"] = h.Providers["fake"]
	spend := func(name string, util float64) {
		h.health(name, provider.Health{Util: map[string]float64{"five_hour": util}, Resets: map[string]time.Time{"five_hour": time.Now().Add(time.Hour)}})
	}
	if err := h.roomFor("fake/m", largeNeed); err != nil {
		t.Fatalf("an account that reports nothing must pass: %v", err)
	}
	spend("fake", 0.5) // 50% left
	if err := h.roomFor("fake/m", smallNeed); err != nil {
		t.Fatalf("small with 50%% left: %v", err)
	}
	if err := h.roomFor("fake/m", largeNeed); err == nil || !strings.Contains(err.Error(), "resets at") {
		t.Fatalf("large with 50%% left, sharing the parent's reserve, must be refused with the reset time: %v", err)
	}
	spend("fake", 0.9) // 10% left: under the parent's own reserve
	if err := h.roomFor("fake/m", smallNeed); err == nil {
		t.Fatal("a child must leave the parent its reserve")
	}
}
