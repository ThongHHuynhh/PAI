// Command retrieval compares real semantic embeddings and a lexical baseline.
// Its stores and reports belong in an explicitly supplied benchmark directory,
// never the live memory store. Cases are public synthetic facts, not user data.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"pai/pkg/localembed"
	"pai/pkg/memory"
)

type dataset struct {
	Version int `json:"version"`
	Items   []struct {
		ID, Text string
		Invalid  bool
	} `json:"items"`
	Queries []struct{ ID, Group, Query, Want string } `json:"queries"`
}
type latency struct {
	N             int `json:"n"`
	P50, P95, Max float64
}
type quality struct {
	N                           int `json:"n"`
	Top1, Recall3, Recall6, MRR float64
}
type queryResult struct {
	ID, Group  string
	Rank       int
	Hits       int
	BestCosine float64
	FirstMS    float64
}
type result struct {
	Mode              string `json:"mode"`
	Items             int    `json:"items"`
	IndexMS           float64
	Quality           quality
	Groups            map[string]quality
	FirstPass, Cached latency
	InvalidHits       int
	Queries           []queryResult
}

func main() {
	data := flag.String("data", "bench/retrieval/cases.json", "labeled synthetic dataset")
	work := flag.String("work", "", "fresh isolated work directory (required)")
	out := flag.String("out", "", "JSON report path (required)")
	url := flag.String("url", "http://127.0.0.1:11434/v1", "embedding endpoint")
	model := flag.String("model", "embeddinggemma", "embedding model")
	repeats := flag.Int("repeats", 5, "cached passes")
	managed := flag.String("managed-root", "", "check lifecycle of an already installed private runtime, then exit")
	check := flag.Bool("check", false, "require semantic top1 >= 95%, recall3 = 100%, and no invalid hits on this fixture")
	flag.Parse()
	if *managed != "" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		endpoint := "http://127.0.0.1:11435/v1"
		start := time.Now()
		stop, err := localembed.Start(ctx, *managed, endpoint)
		if err != nil {
			fatal(err)
		}
		if err := localembed.Warm(ctx, endpoint, *model); err != nil {
			stop()
			fatal(err)
		}
		elapsed := ms(time.Since(start))
		stop()
		resp, err := (&http.Client{Timeout: time.Second}).Get("http://127.0.0.1:11435/api/version")
		if err == nil {
			_ = resp.Body.Close()
			fatal(fmt.Errorf("managed runtime listener survived shutdown"))
		}
		fmt.Printf("private runtime startup/warmup %.2fms; stopped and listener closed\n", elapsed)
		return
	}
	if *work == "" || *out == "" || *repeats < 1 {
		fatal(fmt.Errorf("supply -work and -out, with repeats >= 1"))
	}
	if err := os.Mkdir(*work, 0o700); err != nil {
		fatal(err)
	}
	b, err := os.ReadFile(*data)
	if err != nil {
		fatal(err)
	}
	var d dataset
	if err := json.Unmarshal(b, &d); err != nil {
		fatal(err)
	}
	ctx := context.Background()
	var results []result
	for _, mode := range []string{"hash-hybrid", "semantic", "model-hybrid"} {
		var e memory.Embedder = memory.Hash{}
		if mode != "hash-hybrid" {
			e = memory.NewCached(memory.OpenAI{URL: *url, Model: *model})
		}
		r, err := run(ctx, d, filepath.Join(*work, mode), mode, e, *repeats)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", mode, err))
		}
		results = append(results, r)
		fmt.Printf("%s: top1=%.1f%% recall3=%.1f%% recall6=%.1f%% first_p50=%.2fms cached_p50=%.2fms invalid=%d\n", mode, r.Quality.Top1*100, r.Quality.Recall3*100, r.Quality.Recall6*100, r.FirstPass.P50, r.Cached.P50, r.InvalidHits)
	}
	report := struct {
		Version    int
		At         string
		Model, URL string
		Repeats    int
		Results    []result
	}{d.Version, time.Now().UTC().Format(time.RFC3339), *model, *url, *repeats, results}
	b, err = json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o600); err != nil {
		fatal(err)
	}
	if *check {
		r := results[1]
		if r.Quality.Top1 < .95 || r.Quality.Recall3 < 1 || r.InvalidHits != 0 {
			fatal(fmt.Errorf("semantic retrieval release gate failed"))
		}
	}
}

func run(ctx context.Context, d dataset, dir, mode string, e memory.Embedder, repeats int) (result, error) {
	r := result{Mode: mode, Groups: map[string]quality{}}
	s, err := memory.Open(dir, e)
	if err != nil {
		return r, err
	}
	defer func() { _ = s.Close() }()
	s.Hybrid = mode != "semantic"
	ids := map[string]string{}
	invalid := map[string]bool{}
	start := time.Now()
	for _, item := range d.Items {
		it, fresh, err := s.Remember(ctx, memory.Item{Text: item.Text, Source: "benchmark"})
		if err != nil {
			return r, err
		}
		if !fresh {
			return r, fmt.Errorf("fixture %s was unexpectedly deduplicated", item.ID)
		}
		ids[item.ID] = it.ID
		if item.Invalid {
			if _, err := s.Invalidate(ctx, it.ID, "superseded benchmark fact"); err != nil {
				return r, err
			}
			invalid[it.ID] = true
		}
	}
	r.IndexMS = ms(time.Since(start))
	r.Items = len(d.Items)
	var first, cached []float64
	for _, q := range d.Queries {
		start := time.Now()
		hits, err := s.Search(ctx, q.Query, "", 6)
		elapsed := ms(time.Since(start))
		if err != nil {
			return r, err
		}
		first = append(first, elapsed)
		qr := queryResult{ID: q.ID, Group: q.Group, Hits: len(hits), FirstMS: elapsed}
		for i, h := range hits {
			if invalid[h.Item.ID] {
				r.InvalidHits++
			}
			if mode == "semantic" && h.Lexical != 0 {
				return r, fmt.Errorf("semantic retrieval used lexical ranking")
			}
			if h.Item.ID == ids[q.Want] && q.Want != "" {
				qr.Rank = i + 1
			}
			qr.BestCosine = math.Max(qr.BestCosine, h.Cosine)
		}
		r.Queries = append(r.Queries, qr)
		if q.Want != "" {
			r.Quality = add(r.Quality, qr.Rank)
			r.Groups[q.Group] = add(r.Groups[q.Group], qr.Rank)
		}
	}
	for range repeats {
		for _, q := range d.Queries {
			start := time.Now()
			_, err := s.Search(ctx, q.Query, "", 6)
			if err != nil {
				return r, err
			}
			cached = append(cached, ms(time.Since(start)))
		}
	}
	r.Quality = finish(r.Quality)
	for k, v := range r.Groups {
		r.Groups[k] = finish(v)
	}
	r.FirstPass = summary(first)
	r.Cached = summary(cached)
	return r, nil
}
func add(q quality, rank int) quality {
	q.N++
	if rank == 1 {
		q.Top1++
	}
	if rank > 0 && rank <= 3 {
		q.Recall3++
	}
	if rank > 0 && rank <= 6 {
		q.Recall6++
	}
	if rank > 0 {
		q.MRR += 1 / float64(rank)
	}
	return q
}
func finish(q quality) quality {
	if q.N > 0 {
		n := float64(q.N)
		q.Top1 /= n
		q.Recall3 /= n
		q.Recall6 /= n
		q.MRR /= n
	}
	return q
}
func summary(v []float64) latency {
	slices.Sort(v)
	if len(v) == 0 {
		return latency{}
	}
	return latency{len(v), v[int(math.Ceil(.5*float64(len(v))))-1], v[int(math.Ceil(.95*float64(len(v))))-1], v[len(v)-1]}
}
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
func fatal(err error)            { fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error())); os.Exit(1) }
