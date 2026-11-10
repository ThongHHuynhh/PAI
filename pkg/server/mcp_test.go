package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"pai/pkg/auth"
	"pai/pkg/memory"
)

const rememberCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember","arguments":{"text":"The staging database runs on port 5433"}}}`

func mcpPost(h http.Handler, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(rememberCall))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// With no store the tools say so; a nil *memory.Store passed as the interface
// would panic instead.
func TestMCPWithoutAStore(t *testing.T) {
	mux := http.NewServeMux()
	(&webAPI{}).handle(mux)
	w := mcpPost(mux, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "this server has no memory store") || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// /mcp needs an api key, and what it writes reaches the event stream.
func TestMCPBehindKeysAndHeardLive(t *testing.T) {
	dir := t.TempDir()
	mem, err := memory.Open(dir, memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	var heard []memory.Event
	mem.OnEvent = func(e memory.Event) { heard = append(heard, e) }

	store := auth.Store{Path: filepath.Join(dir, "auth.json")}
	secret, err := store.Issue(auth.Key, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&webAPI{mem: mem}).handle(mux)
	h := store.Bearer(&auth.Limit{}, mux)

	if w := mcpPost(h, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", w.Code)
	}
	if len(heard) != 0 {
		t.Fatalf("a refused call wrote: %v", heard)
	}
	w := mcpPost(h, secret)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "remembered as") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(heard) != 1 || heard[0].Type != "remembered" || heard[0].Items[0].Source != memory.SourceMCP {
		t.Errorf("heard %+v", heard)
	}
}
