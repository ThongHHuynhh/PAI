package harness

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"pai/pkg/catalog"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

func TestEligibilityReason(t *testing.T) {
	image := statefile.Record{Header: statefile.Header{N: 2, Kind: statefile.Message, Type: "image/png"}}
	compact := statefile.Record{Header: statefile.Header{N: 3, Kind: statefile.Compaction, Upto: 2}, Body: []byte("summary")}
	for _, tc := range []struct {
		name  string
		used  int
		limit int
		media bool
		chain []statefile.Record
		want  string
	}{
		{name: "empty", limit: 100000},
		{name: "reserve boundary", used: 80000, limit: 100000},
		{name: "over reserve", used: 180000, limit: 160000, want: "context 180k does not fit 128k (80% of 160k context window)"},
		{name: "unknown window", used: 180000},
		{name: "unsupported image", chain: []statefile.Record{image}, want: "session holds images the model cannot read"},
		{name: "supported image", chain: []statefile.Record{image}, media: true},
		{name: "compacted image", chain: []statefile.Record{image, compact}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := tc.chain
			if tc.used > 0 {
				chain = append(chain, statefile.Record{Header: statefile.Header{Kind: statefile.Step, Usage: &statefile.Usage{Input: tc.used}}})
			}
			p := provider.Provider{Envelope: accountEnvelope{media: tc.media}}
			got := eligibilityReason(accountConf(), chain, p, catalog.Preset{ContextLimit: tc.limit})
			if got != tc.want {
				t.Fatalf("reason: %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionModelsRoute(t *testing.T) {
	h, _, _ := accountHarness(t)
	h.Providers["claude-code-2"] = provider.Provider{Envelope: accountEnvelope{name: "claude-code", media: true}}
	id := startAccountSession(t, h, accountConf(), 1000)
	f, err := h.Sessions.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "image/png"}, Body: []byte("image")}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	handler := Handler(context.Background(), h)
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	var rows []SessionModel
	if err := json.Unmarshal(call("GET", "/sessions/"+id+"/models", "", 200).Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	byModel := map[string]SessionModel{}
	for _, row := range rows {
		byModel[row.Model] = row
		if row.Provider != "anthropic" && row.Provider != "openai" && row.Provider != "claude-code-2" {
			t.Fatalf("unserved provider: %+v", row)
		}
	}
	for name, p := range h.Providers {
		for _, pr := range modelCatalog.Presets {
			if pr.Provider != catalogProvider(name, p) {
				continue
			}
			pr = modelCatalog.Presets[pr.Provider+"/"+pr.Model]
			row, ok := byModel[name+"/"+pr.Model]
			if !ok || row.Name != pr.Name || row.ContextLimit != pr.ContextLimit || row.Current != (row.Model == accountConf().Model) {
				t.Fatalf("missing or incorrect catalog model: %s/%s: %+v", name, pr.Model, row)
			}
		}
	}
	if row := byModel[accountConf().Model]; !row.Current || !row.OK || row.Reason != "" {
		t.Fatalf("current model: %+v", row)
	}
	blocked := byModel["openai/gpt-6-luna"]
	if blocked.OK || blocked.Reason != "session holds images the model cannot read" {
		t.Fatalf("media reason: %+v", blocked)
	}
	before, _ := h.Sessions.Load(id)
	w := call("PUT", "/sessions/"+id+"/model?via=test", blocked.Model, 400)
	if !strings.Contains(w.Body.String(), blocked.Reason) {
		t.Fatalf("swap and list disagree: %s", w.Body.String())
	}
	after, _ := h.Sessions.Load(id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected move changed the session")
	}
	alias := "claude-code-2/claude-sonnet-5-5"
	if !byModel[alias].OK {
		t.Fatalf("alias is not eligible: %+v", byModel[alias])
	}
	call("PUT", "/sessions/"+id+"/model?via=test", alias, 200)
	after, _ = h.Sessions.Load(id)
	conf, _ := config(after)
	if len(after) != len(before)+1 || !reflect.DeepEqual(before, after[:len(before)]) || after[len(before)].Kind != statefile.Config || after[len(before)].Via != "test" || conf.Model != alias || conf.Preset != "claude-code/claude-sonnet-5-5" {
		t.Fatalf("swap did not append the alias config: %+v", conf)
	}
	if err := json.Unmarshal(call("GET", "/sessions/"+id+"/models", "", 200).Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Current != (row.Model == alias) {
			t.Fatalf("stale current: %+v", row)
		}
	}
	call("GET", "/sessions/no-session/models", "", 400)
	call("GET", "/sessions/invalid/models", "", 400)
	call("PUT", "/sessions/"+id+"/model", alias, 400)
}

func TestSessionModelsCustomAndDisconnected(t *testing.T) {
	h, _, _ := accountHarness(t)
	h.Providers["local"] = provider.Provider{Envelope: accountEnvelope{name: "local"}}
	conf := accountConf()
	conf.Model = "local/custom"
	id := startAccountSession(t, h, conf, 0)
	for _, served := range []bool{true, false} {
		if !served {
			delete(h.Providers, "local")
		}
		models, err := h.Models(id)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range models {
			if row.Model == conf.Model {
				found = true
				if !row.Current || row.OK != served || (!served && row.Reason != "provider not served") {
					t.Fatalf("current custom model: %+v", row)
				}
			}
		}
		if !found {
			t.Fatal("current custom model missing")
		}
	}
}
