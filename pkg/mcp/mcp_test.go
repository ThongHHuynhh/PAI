package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func echoServer() *Server {
	return New(Tool{
		Name: "echo", Schema: json.RawMessage(`{"type":"object"}`),
		Call: func(_ context.Context, args json.RawMessage) (string, error) {
			if strings.Contains(string(args), "boom") {
				return "", errors.New("boom")
			}
			return string(args), nil
		},
	})
}

type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func rpc(t *testing.T, s *Server, msg string) rpcReply {
	t.Helper()
	b := s.Handle(context.Background(), []byte(msg))
	if b == nil {
		t.Fatalf("%s: no reply", msg)
	}
	var r rpcReply
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: %v: %s", msg, err, b)
	}
	return r
}

func TestInitializeNegotiates(t *testing.T) {
	for _, c := range []struct{ asked, got string }{
		{"2025-11-25", "2025-11-25"},
		{"2025-06-18", "2025-06-18"},
		{"2025-03-26", "2025-11-25"}, // allows batches, which are not served
		{"2026-07-28", "2025-11-25"},
		{"", "2025-11-25"},
	} {
		r := rpc(t, echoServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+c.asked+`"}}`)
		var res struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools *struct{} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo   struct{ Name, Version string } `json:"serverInfo"`
			Instructions string                         `json:"instructions"`
		}
		if err := json.Unmarshal(r.Result, &res); err != nil {
			t.Fatal(err)
		}
		if res.ProtocolVersion != c.got {
			t.Errorf("asked %q: got %q, want %q", c.asked, res.ProtocolVersion, c.got)
		}
		if res.ServerInfo.Name != "pai-memory" || res.ServerInfo.Version == "" || res.Capabilities.Tools == nil {
			t.Errorf("incomplete result: %s", r.Result)
		}
		for _, tool := range []string{"get_context", "recall", "remember", "forget", "edit_memory"} {
			if !strings.Contains(res.Instructions, tool) {
				t.Errorf("instructions do not mention %s", tool)
			}
		}
	}
}

func TestNoReplyWithoutARequest(t *testing.T) {
	s := echoServer()
	for _, msg := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`, // a reply to nothing this server asked
	} {
		if b := s.Handle(context.Background(), []byte(msg)); b != nil {
			t.Errorf("%s got a reply: %s", msg, b)
		}
	}
}

func TestProtocolErrors(t *testing.T) {
	s := echoServer()
	for _, c := range []struct {
		msg  string
		code int
		id   string
	}{
		{`{not json`, codeParse, "null"},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, codeInvalid, "null"},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, codeInvalid, "null"},
		{`{"jsonrpc":"2.0","id":true,"method":"ping"}`, codeInvalid, "null"},
		{`{"jsonrpc":"2.0"}`, codeInvalid, "null"},
		{`{"jsonrpc":"1.0","id":2,"method":"ping"}`, codeInvalid, "2"},
		{`{"jsonrpc":"2.0","id":"a","method":"nope"}`, codeNoMethod, `"a"`},
		{`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":9}}`, codeParams, "3"},
		{`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":"x"}`, codeParams, "4"},
		{`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`, codeParams, "5"},
	} {
		r := rpc(t, s, c.msg)
		if r.Error == nil || r.Error.Code != c.code || string(r.ID) != c.id {
			t.Errorf("%s: id %s error %+v, want id %s code %d", c.msg, r.ID, r.Error, c.id, c.code)
		}
	}
}

func TestPingAndList(t *testing.T) {
	s := echoServer()
	if r := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); r.Error != nil || string(r.Result) != "{}" {
		t.Errorf("ping: %+v %s", r.Error, r.Result)
	}
	r := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Tools) != 1 || res.Tools[0].Name != "echo" || res.Tools[0].InputSchema == nil {
		t.Errorf("list: %v %s", err, r.Result)
	}
	if r := rpc(t, New(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); !strings.Contains(string(r.Result), `"tools":[]`) {
		t.Errorf("no tools is an empty list, got %s", r.Result)
	}
}

func TestToolFailureIsAResult(t *testing.T) {
	r := rpc(t, echoServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"a":"boom"}}}`)
	if r.Error != nil {
		t.Fatalf("a tool failure came back as a protocol error: %+v", r.Error)
	}
	var res callResult
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) != 1 || res.Content[0].Type != "text" || res.Content[0].Text != "error: boom" {
		t.Errorf("got %+v", res)
	}
}

func TestToolCallWithoutArguments(t *testing.T) {
	r := rpc(t, echoServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`)
	var res callResult
	if err := json.Unmarshal(r.Result, &res); err != nil || res.IsError || res.Content[0].Text != "{}" {
		t.Errorf("%v %+v", err, res)
	}
}

func post(s http.Handler, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://pai.example/mcp", strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

const ping = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

func TestHTTP(t *testing.T) {
	s := echoServer()
	if w := post(s, ping); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `"result":{}`) {
		t.Errorf("request: %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if w := post(s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Errorf("notification: %d %s", w.Code, w.Body)
	}
	if w := post(s, `{"jsonrpc":"2.0","id":1,"method":"nope"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "-32601") {
		t.Errorf("unknown method answers in JSON-RPC: %d %s", w.Code, w.Body)
	}
	if w := post(s, `{oops`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "-32700") {
		t.Errorf("parse error: %d %s", w.Code, w.Body)
	}
	if w := post(s, `[`+ping+`]`); w.Code != http.StatusBadRequest {
		t.Errorf("batch: %d %s", w.Code, w.Body)
	}
}

func TestHTTPMethods(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		w := httptest.NewRecorder()
		echoServer().ServeHTTP(w, httptest.NewRequest(m, "http://pai.example/mcp", nil))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
			t.Errorf("%s: %d allow %q", m, w.Code, w.Header().Get("Allow"))
		}
	}
}

func TestHTTPBodyLimit(t *testing.T) {
	if w := post(echoServer(), strings.Repeat(" ", MaxBody)+ping); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: %d", w.Code)
	}
	if w := post(echoServer(), strings.Repeat(" ", MaxBody-len(ping))+ping); w.Code != http.StatusOK {
		t.Errorf("at the limit: %d", w.Code)
	}
}

func TestHTTPOrigin(t *testing.T) {
	s := echoServer()
	for _, c := range []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK},
		{"https://pai.example", http.StatusOK},
		{"https://PAI.example", http.StatusOK},
		{"https://evil.example", http.StatusForbidden},
		{"null", http.StatusForbidden},
	} {
		var headers []string
		if c.origin != "" {
			headers = []string{"Origin", c.origin}
		}
		if w := post(s, ping, headers...); w.Code != c.want {
			t.Errorf("origin %q: %d, want %d", c.origin, w.Code, c.want)
		}
	}
}

func TestHTTPProtocolVersionHeader(t *testing.T) {
	s := echoServer()
	if w := post(s, ping); w.Code != http.StatusOK {
		t.Errorf("no header: %d", w.Code)
	}
	for _, v := range versions {
		if w := post(s, ping, "MCP-Protocol-Version", v); w.Code != http.StatusOK {
			t.Errorf("%s: %d", v, w.Code)
		}
	}
	// A client that tries the newer revision first reads a 400 with no modern
	// error in it as its cue to fall back to initialize.
	w := post(s, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, "MCP-Protocol-Version", "2026-07-28")
	if w.Code != http.StatusBadRequest || bytes.Contains(w.Body.Bytes(), []byte("jsonrpc")) {
		t.Errorf("newer revision: %d %s", w.Code, w.Body)
	}
}

func TestVersionsLeaveOutBatches(t *testing.T) {
	if slices.Contains(versions, "2025-03-26") {
		t.Error("2025-03-26 allows batches, which ServeHTTP refuses")
	}
}
