package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func stdio(t *testing.T, in string, post func(context.Context, []byte) ([]byte, error)) []string {
	t.Helper()
	var out bytes.Buffer
	if err := Stdio(context.Background(), strings.NewReader(in), &out, post); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return nil
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Errorf("the last reply has no line end: %q", out.String())
	}
	return strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
}

// direct posts to a server in this process.
func direct(s *Server) func(context.Context, []byte) ([]byte, error) {
	return func(ctx context.Context, b []byte) ([]byte, error) { return s.Handle(ctx, b), nil }
}

func TestStdioLines(t *testing.T) {
	got := stdio(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\r\n"+
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n\n"+
		`{"jsonrpc":"2.0","id":"two","method":"ping"}`, direct(echoServer())) // no line end on the last
	if len(got) != 2 || !strings.Contains(got[0], `"id":1`) || !strings.Contains(got[1], `"id":"two"`) {
		t.Errorf("got %q", got)
	}
}

func TestStdioCompactsReplies(t *testing.T) {
	got := stdio(t, ping+"\n", func(context.Context, []byte) ([]byte, error) {
		return []byte("{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 1,\n  \"result\": {}\n}\n"), nil
	})
	if len(got) != 1 || got[0] != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("got %q", got)
	}
}

func errorOf(t *testing.T, line string) (string, rpcError) {
	t.Helper()
	var r struct {
		ID    json.RawMessage `json:"id"`
		Error *rpcError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &r); err != nil || r.Error == nil {
		t.Fatalf("not an error reply: %v %q", err, line)
	}
	return string(r.ID), *r.Error
}

func TestStdioPostFailureAnswersThatID(t *testing.T) {
	down := func(context.Context, []byte) ([]byte, error) {
		return nil, errors.New("pai-server unreachable: no route")
	}
	got := stdio(t, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`+"\n"+`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+`{"jsonrpc":"2.0","id":9,"method":"ping"}`+"\n", down)
	if len(got) != 2 {
		t.Fatalf("a failed notification must write nothing: %q", got)
	}
	if id, e := errorOf(t, got[0]); id != `"abc"` || e.Code != codeServer || !strings.Contains(e.Message, "unreachable") {
		t.Errorf("%s %+v", id, e)
	}
	if id, _ := errorOf(t, got[1]); id != "9" {
		t.Errorf("id %s", id)
	}
}

func TestStdioNotJSONIsReportedHere(t *testing.T) {
	never := func(context.Context, []byte) ([]byte, error) {
		t.Error("posted something that is not a message")
		return nil, nil
	}
	got := stdio(t, "{oops\n[1,2]\n", never)
	if len(got) != 2 {
		t.Fatalf("got %q", got)
	}
	if id, e := errorOf(t, got[0]); id != "null" || e.Code != codeParse {
		t.Errorf("%s %+v", id, e)
	}
	if id, e := errorOf(t, got[1]); id != "null" || e.Code != codeInvalid {
		t.Errorf("%s %+v", id, e)
	}
}

func TestStdioLongLines(t *testing.T) {
	// past a scanner's default 64 KiB, but within MaxBody
	long := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"a":"` + strings.Repeat("x", 300_000) + `"}}}`
	got := stdio(t, long+"\n", direct(echoServer()))
	if len(got) != 1 || !strings.Contains(got[0], strings.Repeat("x", 300_000)) {
		t.Fatalf("long line lost: %d lines", len(got))
	}
	// over MaxBody: refused, and the next line still served
	got = stdio(t, strings.Repeat("y", MaxBody+10)+"\n"+ping+"\n", direct(echoServer()))
	if len(got) != 2 || !strings.Contains(got[1], `"result":{}`) {
		t.Fatalf("got %d lines", len(got))
	}
	if _, e := errorOf(t, got[0]); e.Code != codeInvalid {
		t.Errorf("%+v", e)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestStdioStopsWhenTheClientIsGone(t *testing.T) {
	err := Stdio(context.Background(), strings.NewReader(ping+"\n"+ping+"\n"), failWriter{}, direct(echoServer()))
	if err == nil {
		t.Error("a write failure must end it")
	}
}

func TestStdioStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Stdio(ctx, strings.NewReader(ping+"\n"), &bytes.Buffer{}, direct(echoServer()))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}
