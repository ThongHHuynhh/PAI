package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
	"pai/pkg/worker"
)

// A credential filled into a call reaches the worker, and nothing the
// session file keeps: not the execution trace of the request, not the
// worker's echo of it, not a path.
func TestCredentialNeverReachesTheSessionFile(t *testing.T) {
	ctx := context.Background()
	const pw = "s3cret-value-123"
	const quoted = "q\"uo\\te\n-pass" // a quote, a backslash and a newline: JSON escapes it inside a snapshot
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, c := range []memory.Credential{{Name: "db", Description: "database password", Value: pw}, {Name: "q", Description: "a token", Value: quoted}} {
		if err := st.SaveCredential(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	r := &Remote{Header: worker.Header{Name: "remote", Scratch: "/scratch", Interps: []string{"bash"}}, conn: server, calls: map[string]chan worker.Frame{}, left: map[string]chan struct{}{}, done: make(chan struct{})}
	go r.recv(bufio.NewReader(server))
	got := make(chan string, 8)
	go func() {
		br := bufio.NewReader(client)
		for {
			f, err := worker.Decode(br)
			if err != nil {
				return
			}
			got <- string(f.Body) + f.Path
			// The worker echoes what it ran, as a shell with set -x would.
			answer := worker.Frame{Header: worker.Header{ID: f.ID, Kind: worker.Result}, Body: append([]byte("ran: "), f.Body...)}
			if f.Kind == worker.Read { // over the limits, so the result names its file
				answer.Kind, answer.Body = worker.Bytes, bytes.Repeat([]byte("a line of the file\n"), 4000)
			}
			if err := worker.Encode(client, answer); err != nil {
				return
			}
		}
	}()

	h := orchestrationHarness(t, nil)
	h.Memory = st
	h.Workers.Add(r)
	id, err := h.Create(ctx, Create{Model: "fake/test", ContextLimit: 100000, Workers: []statefile.Worker{{Name: "remote"}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	conf := statefile.Conf{Model: "fake/test", Tools: "default", Workers: []statefile.Worker{{Name: "remote"}}, ContextLimit: 100000}
	call := func(name string, a map[string]any) statefile.Record {
		args, _ := json.Marshal(a)
		body, _ := json.Marshal(provider.Call{Name: name, Arguments: args})
		return statefile.Record{Header: statefile.Header{Kind: statefile.Call, ID: statefile.NewID(), Type: "application/json"}, Body: body}
	}
	for _, c := range []statefile.Record{
		call("run", map[string]any{"worker": "remote", "code": "psql --password={{cred:db}}"}),
		call("read", map[string]any{"worker": "remote", "path": "/home/{{cred:db}}/notes.txt"}),
		call("write", map[string]any{"worker": "remote", "path": "/scratch/AGENTS.md", "content": "token: {{cred:q}}"}),
	} {
		keep := func(r statefile.Record) {
			if err := h.note(h.session(id), id, r); err != nil {
				t.Fatal(err)
			}
		}
		res := h.execute(ctx, id, conf, c, keep) // keep records the instruction snapshot, as a step does
		keep(res)
		if strings.Contains(string(res.Body), pw) {
			t.Fatalf("%s result holds the value: %q", c.ID, res.Body)
		}
	}

	sent := ""
	for len(got) > 0 {
		sent += <-got
	}
	if !strings.Contains(sent, pw) {
		t.Fatalf("the worker never got the value; it got %q", sent)
	}
	file, err := os.ReadFile(h.Sessions.Path(id))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	traced := 0
	for _, rec := range chain {
		if rec.Kind != statefile.Execution {
			continue
		}
		traced++
		var e executionEvent
		if err := json.Unmarshal(rec.Body, &e); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(e.Frame.Body)+e.Frame.Path+e.Frame.Full, pw) {
			t.Errorf("execution record %d (%s) holds the value", rec.N, e.State)
		}
	}
	if traced == 0 {
		t.Fatal("no execution records: the test checks nothing")
	}
	escaped, _ := json.Marshal(quoted)
	for _, v := range []string{pw, quoted, strings.Trim(string(escaped), `"`)} {
		if strings.Contains(string(file), v) {
			t.Errorf("%q is in the session file as written", v)
		}
	}
	snapshots := 0
	for _, rec := range chain {
		if rec.Kind == statefile.Instruction {
			snapshots++
		}
		if rec.Kind == statefile.Result && rec.FullOutput != "" && !strings.Contains(rec.FullOutput, "{{cred:db}}") {
			t.Errorf("full output path %q lost its placeholder", rec.FullOutput)
		}
	}
	if snapshots == 0 {
		t.Error("no instruction snapshot: the AGENTS.md write was not checked")
	}
}

// When the credential store cannot be read, a record keeps nothing it could
// not check, and a withheld instruction snapshot does not break the listing.
func TestWithheldWhenTheStoreCannotBeRead(t *testing.T) {
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil { // every read of it now fails
		t.Fatal(err)
	}
	h := orchestrationHarness(t, nil)
	h.Memory = st
	id, err := h.Create(context.Background(), Create{Model: "fake/test", ContextLimit: 100000}, "test")
	if err != nil {
		t.Fatal(err)
	}
	snap, ok := instructionRecord("remote", "/scratch/AGENTS.md", []byte("token: s3cret-value-123"))
	if !ok {
		t.Fatal("no snapshot record")
	}
	for _, r := range []statefile.Record{snap, result("c1", "echo s3cret-value-123")} {
		m := h.mask(context.Background(), r)
		if strings.Contains(string(m.Body), "s3cret") || !strings.Contains(string(m.Body), "withheld") {
			t.Fatalf("%s body %q", r.Kind, m.Body)
		}
		if err := h.note(h.session(id), id, m); err != nil {
			t.Fatal(err)
		}
	}
	if items, err := h.Instructions(); err != nil || len(items) != 0 {
		t.Fatalf("instructions %+v %v", items, err)
	}
}
