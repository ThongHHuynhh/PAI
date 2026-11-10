package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"pai/pkg/auth"
	"pai/pkg/harness"
	"pai/pkg/statefile"
)

func TestInstructionRoutesReadOnlyAuthenticated(t *testing.T) {
	h := &harness.Harness{Sessions: statefile.Sessions{Dir: t.TempDir()}}
	mux := http.NewServeMux()
	(&webAPI{h: h}).handle(mux)
	store := auth.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	key, err := store.Issue(auth.Key, "test")
	if err != nil {
		t.Fatal(err)
	}
	handler := store.Bearer(&auth.Limit{}, mux)
	for _, tc := range []struct {
		method, url string
		authorized  bool
		status      int
	}{
		{"GET", "/instructions", false, 401},
		{"GET", "/instructions?path=C:/private/AGENTS.md", true, 200},
		{"POST", "/instructions", true, 405},
		{"GET", "/instructions/AGENTS.md", true, 404},
	} {
		r := httptest.NewRequest(tc.method, tc.url, nil)
		if tc.authorized {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.url, w.Code, w.Body.String())
		}
		if w.Code == 200 && w.Body.String() != "[]\n" {
			t.Fatalf("empty inventory/path query: %s", w.Body.String())
		}
	}
}
