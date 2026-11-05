package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddingResponseValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bad        bool
	}{
		{"reordered", `{"data":[{"index":1,"embedding":[0,2]},{"index":0,"embedding":[3,0]}]}`, false},
		{"duplicate", `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`, true},
		{"missing", `{"data":[{"index":0,"embedding":[1,0]}]}`, true},
		{"empty", `{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[]}]}`, true},
		{"zero", `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[1,0]}]}`, true},
		{"dimensions", `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1]}]}`, true},
		{"range", `{"data":[{"index":2,"embedding":[1,0]},{"index":1,"embedding":[1,0]}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			v, err := (OpenAI{URL: srv.URL, Model: "test"}).Embed(context.Background(), []string{"first", "second"})
			if (err != nil) != tc.bad {
				t.Fatalf("vectors=%v error=%v", v, err)
			}
			if !tc.bad && (v[0][0] != 1 || v[1][1] != 1) {
				t.Fatalf("not reordered and normalized: %v", v)
			}
		})
	}
}

func TestSemanticRetrievalAndMigration(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	s, err := Open(dir, Hash{})
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := s.Remember(ctx, Item{Text: "Automobiles require regular servicing"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Remember(ctx, Item{Text: "Bananas grow in tropical climates"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	// A deterministic model fixture makes a synonym query with no shared
	// words point at the right fact. This exercises HTTP, reindex and ranking.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		var data []map[string]any
		for i, text := range in.Input {
			v := []float32{1, 0}
			if text == "Bananas grow in tropical climates" {
				v = []float32{0, 1}
			}
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	s, err = Open(dir, NewCached(OpenAI{URL: srv.URL, Model: "fixture"}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	hits, err := s.Search(ctx, "car maintenance", "", 3)
	if err != nil || len(hits) != 1 || hits[0].Item.ID != want.ID || hits[0].Lexical != 0 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	// Identical semantic vectors must not silently discard distinct facts.
	_, fresh, err := s.Remember(ctx, Item{Text: "Automobiles never require servicing"})
	if err != nil || !fresh {
		t.Fatalf("distinct fact lost: fresh=%v err=%v", fresh, err)
	}
	_, _ = s.Invalidate(ctx, want.ID, "outdated")
	hits, err = s.Search(ctx, "car maintenance", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Item.ID == want.ID {
			t.Fatal("invalid item returned")
		}
	}
}

type countingEmbedder struct{ calls, texts int }

func (c *countingEmbedder) Name() string { return "count" }
func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.calls++
	c.texts += len(texts)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func TestEmbeddingCache(t *testing.T) {
	ctx, base := context.Background(), &countingEmbedder{}
	c := NewCached(base)
	v, err := c.Embed(ctx, []string{"one", "one"})
	if err != nil || base.texts != 1 {
		t.Fatalf("batch not deduplicated: %v %d", err, base.texts)
	}
	v[0][0] = 99
	v, err = c.Embed(ctx, []string{"one"})
	if err != nil || base.calls != 1 || math.Abs(float64(v[0][0]-1)) > 1e-6 {
		t.Fatal("cache missed or aliased")
	}
	for i := 0; i < embedCacheSize; i++ {
		_, _ = c.Embed(ctx, []string{fmt.Sprint(i)})
	}
	before := base.calls
	_, _ = c.Embed(ctx, []string{"one"})
	if base.calls != before+1 {
		t.Fatal("oldest entry not evicted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Embed(canceled, []string{"one"}); err == nil {
		t.Fatal("cancellation ignored")
	}
}
