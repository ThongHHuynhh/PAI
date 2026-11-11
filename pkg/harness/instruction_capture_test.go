package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"pai/pkg/statefile"
	"pai/pkg/worker"
)

func TestStartupInstructionPersisted(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	r := &Remote{Header: worker.Header{Name: "remote", Scratch: "/only-on-worker", Interps: []string{"bash"}}, conn: server, calls: map[string]chan worker.Frame{}, left: map[string]chan struct{}{}, done: make(chan struct{})}
	go r.recv(bufio.NewReader(server))
	go func() {
		br := bufio.NewReader(client)
		for {
			f, err := worker.Decode(br)
			if err != nil {
				return
			}
			answer := worker.Frame{Header: worker.Header{ID: f.ID, Kind: worker.Result}}
			if f.Kind == worker.Read {
				answer.Kind, answer.Body = worker.Bytes, []byte("worker instructions")
			}
			if err := worker.Encode(client, answer); err != nil {
				return
			}
		}
	}()
	h := orchestrationHarness(t, nil)
	h.Workers.Add(r)
	id, err := h.Create(context.Background(), Create{Model: "fake/test", ContextLimit: 100000, Workers: []statefile.Worker{{Name: "remote"}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the projection with a fresh harness and no connected workers.
	reopened := &Harness{Sessions: h.Sessions}
	items, err := reopened.Instructions()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Worker != "remote" || items[0].Path != "/only-on-worker/AGENTS.md" || items[0].Content != "worker instructions" || items[0].Session != id {
		t.Fatalf("persisted observations: %+v", items)
	}
}

type instructionWorker struct {
	Worker
	content           []byte
	readErr, writeErr error
	writes, failAt    int
}

func (w *instructionWorker) Read(context.Context, string) ([]byte, error) {
	return w.content, w.readErr
}
func (w *instructionWorker) Write(_ context.Context, _ string, b []byte, _ string) error {
	w.writes++
	if w.failAt == w.writes {
		return errors.New("write failed")
	}
	if w.writeErr == nil {
		w.content = b
	}
	return w.writeErr
}

func TestPatchInstructionCapture(t *testing.T) {
	const add = "*** Begin Patch\n*** Add File: /skills/test/SKILL.md\n+new\n*** End Patch"
	const update = "*** Begin Patch\n*** Update File: /skills/test/SKILL.md\n@@\n-old\n+new\n*** End Patch"
	const multiple = "*** Begin Patch\n*** Add File: /skills/test/SKILL.md\n+new\n*** Add File: /AGENTS.md\n+rules\n*** Add File: /rules.md\n+later\n*** End Patch"
	for _, tc := range []struct {
		name, patch, content            string
		failAt, wantWrites, wantRecords int
		wantError                       bool
	}{
		{"add", add, "", 0, 1, 1, false},
		{"update", update, "old\n", 0, 1, 1, false},
		{"write failure", add, "", 1, 1, 0, true},
		{"partial success", multiple, "", 2, 2, 1, true},
		{"hunk failure", update, "different\n", 0, 0, 0, true},
		{"preflight failure", strings.Replace(multiple, "*** End Patch", "*** Update File: /rules.md\n@@\n-missing\n+replacement\n*** End Patch", 1), "", 0, 0, 0, true},
		{"parse failure", "invalid", "", 0, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &instructionWorker{content: []byte(tc.content), failAt: tc.failAt}
			var got []statefile.Record
			r, _ := executeObserved(context.Background(), w, "session", "patch", args{Worker: "remote", Input: tc.patch}, nil, func(r statefile.Record) { got = append(got, r) })
			if strings.HasPrefix(string(r.Body), "error: ") != tc.wantError || w.writes != tc.wantWrites || len(got) != tc.wantRecords {
				t.Fatalf("result=%s writes=%d observations=%v", r.Body, w.writes, got)
			}
			for _, record := range got {
				var item InstructionFile
				if err := json.Unmarshal(record.Body, &item); err != nil {
					t.Fatal(err)
				}
				if record.Kind != statefile.Instruction || item.Worker != "remote" || item.Path != "/skills/test/SKILL.md" || item.Content != "new\n" {
					t.Fatalf("snapshot: %+v", item)
				}
			}
		})
	}
}

func TestInstructionCapture(t *testing.T) {
	for _, tc := range []struct {
		name, tool, path, content string
		failRead, failWrite       bool
		want                      string
	}{
		{name: "read", tool: "read", path: "C:/work/AGENTS.md", content: "error: is valid Markdown", want: "error: is valid Markdown"},
		{name: "skill write", tool: "write", path: "/skills/test/SKILL.md", want: "new"},
		{name: "edit", tool: "edit", path: "/work/rules.md", content: "old rule", want: "new rule"},
		{name: "read failure", tool: "read", path: "/AGENTS.md", failRead: true},
		{name: "write failure", tool: "write", path: "/AGENTS.md", failWrite: true},
		{name: "edit failure", tool: "edit", path: "/AGENTS.md", content: "old", failWrite: true},
		{name: "edit mismatch", tool: "edit", path: "/AGENTS.md", content: "different"},
		{name: "unrelated", tool: "read", path: "/notes.md", content: "notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &instructionWorker{content: []byte(tc.content)}
			if tc.failRead {
				w.readErr = errors.New("read failed")
			}
			if tc.failWrite {
				w.writeErr = errors.New("write failed")
			}
			var got []statefile.Record
			executeObserved(context.Background(), w, "session", tc.tool, args{Worker: "remote", Path: tc.path, Content: "new", Old: "old", New: "new"}, nil, func(r statefile.Record) { got = append(got, r) })
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected observation: %v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Kind != statefile.Instruction {
				t.Fatalf("observations: %v", got)
			}
			var item InstructionFile
			if err := json.Unmarshal(got[0].Body, &item); err != nil {
				t.Fatal(err)
			}
			if item.Worker != "remote" || item.Path != tc.path || item.Content != tc.want {
				t.Fatalf("snapshot: %+v", item)
			}
		})
	}
}

func TestInstructionOutsideContext(t *testing.T) {
	chain := []statefile.Record{
		{Header: statefile.Header{N: 1, Kind: statefile.Start}},
		{Header: statefile.Header{N: 2, Kind: statefile.System}, Body: []byte("system")},
		{Header: statefile.Header{N: 3, Kind: statefile.Instruction}, Body: []byte("snapshot")},
		{Header: statefile.Header{N: 4, Kind: statefile.System}, Body: []byte("workers")},
		{Header: statefile.Header{N: 5, Kind: statefile.Message}, Body: []byte("hello")},
	}
	check := func() {
		ctx, _ := statefile.Build(chain)
		for _, r := range ctx {
			if r.Kind == statefile.Instruction {
				t.Fatal("snapshot leaked into context")
			}
		}
		if len(ctx) < 2 || string(ctx[0].Body) != "system" || string(ctx[1].Body) != "workers" {
			t.Fatalf("lost opening context: %v", ctx)
		}
	}
	check()
	chain = append(chain, statefile.Record{Header: statefile.Header{N: 6, Kind: statefile.Compaction, Upto: 5}, Body: []byte("summary")})
	check()
}
