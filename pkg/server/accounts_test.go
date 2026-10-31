package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pai/pkg/harness"
	"pai/pkg/provider"
)

func TestAccountSettingsRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"http":"127.0.0.1:0","providers":{"claude-code":{"shape":"claude-code"},"claude-code-2":{"shape":"claude-code"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := &harness.Harness{Providers: map[string]provider.Provider{"claude-code": provider.ClaudeCode("unused"), "claude-code-2": provider.ClaudeCode("unused-2")}}
	h.Balance = func() harness.Balance { c, _ := Load(path); return c.Balance }
	mux := http.NewServeMux()
	(&webAPI{h: h, path: path}).handle(mux)
	call := func(method, route, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest(method, route, strings.NewReader(body)))
		if r.Code != want {
			t.Fatalf("%s %s: %d %s", method, route, r.Code, r.Body.String())
		}
		return r
	}
	call("GET", "/balance", "", 200)
	call("PUT", "/balance", `{"mode":"auto","pool":["claude-code/claude-sonnet-5-5"]}`, 400)
	call("PUT", "/balance", `{"mode":"auto","pool":["claude-code/claude-sonnet-5-5","missing/model"]}`, 400)
	call("PUT", "/balance", `{"mode":"auto","pool":["claude-code/claude-sonnet-5-5","claude-code-2/unknown"]}`, 400)
	call("PUT", "/balance", `{"mode":"auto","pool":["claude-code/claude-sonnet-5-5","claude-code-2/claude-sonnet-5-5"]}`, 204)
	call("PUT", "/fallbacks", `{"claude-code":"claude-code-2/claude-sonnet-5-5"}`, 204)
	call("PUT", "/fallbacks", `{"claude-code":"missing"}`, 400)
	var rows []harness.Account
	if err := json.Unmarshal(call("GET", "/accounts", "", 200).Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].Name != "claude-code-2/claude-sonnet-5-5" || rows[0].UtilFiveHour != nil {
		t.Fatalf("accounts: %+v", rows)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Balance.Mode != "auto" || c.Fallbacks["claude-code"] != "claude-code-2/claude-sonnet-5-5" || len(c.Providers) != 2 {
		t.Fatalf("settings not preserved: %+v", c)
	}
	call("PUT", "/balance", `{"mode":"off","pool":[]}`, 204)
	if err := json.Unmarshal(call("GET", "/accounts", "", 200).Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[1].Name != "claude-code-2" {
		t.Fatalf("off accounts: %+v", rows)
	}
}
