package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Embedder turns texts into unit vectors. Name changes whenever the vectors
// would: the store embeds every item again with a new one.
type Embedder interface {
	Name() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// hashDims is the hash embedder's width: wide enough that unrelated words
// seldom share a slot in a sentence-sized text.
const hashDims = 1024

// Hash embeds by feature hashing: words, word pairs and character trigrams,
// each to a signed slot. It needs no model and no network, so memory works
// on any server from the first session; it finds rewordings that share
// words and stems, not synonyms. A model embedder finds those.
type Hash struct{}

func (Hash) Name() string { return fmt.Sprintf("hash-%d", hashDims) }

func (Hash) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, hashDims)
		words := Words(t)
		add := func(f string, w float32) {
			h := fnv.New64a()
			_, _ = h.Write([]byte(f))
			x := h.Sum64()
			sign := float32(1)
			if x>>63 == 1 {
				sign = -1
			}
			v[x%hashDims] += sign * w
		}
		for j, w := range words {
			add("w:"+w, 1)
			add("s:"+stem(w), 0.7)
			if j > 0 {
				add("b:"+words[j-1]+" "+w, 0.5)
			}
			r := []rune(" " + w + " ")
			for k := 0; k+3 <= len(r); k++ {
				add("c:"+string(r[k:k+3]), 0.25)
			}
		}
		out[i] = normalize(v)
	}
	return out, nil
}

// Words is t lower cased and split on anything but letters and digits, with
// the commonest English words left out.
func Words(t string) []string {
	f := strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, w := range f {
		if !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an and are as at be by for from has have i in is it its of on or that the this to was were will with you your we our they them he she his her not do does did so if but then than there their what which who how when where can could should would may might") {
		stop[w] = true
	}
}

// stem is a crude suffix strip, enough that plurals and tenses share a slot.
func stem(w string) string {
	for _, suf := range []string{"ingly", "edly", "ing", "ers", "ies", "ed", "es", "er", "ly", "s"} {
		if len(w) > len(suf)+2 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

// OpenAI embeds through an OpenAI compatible /embeddings endpoint: OpenAI's
// own, or a local server such as Ollama's at http://localhost:11434/v1.
type OpenAI struct {
	URL, Key, Model string
}

func (o OpenAI) Name() string { return "openai:" + o.URL + ":" + o.Model }

// embedTime bounds one call: a batch of 64 sentences embeds in well under it.
const embedTime = 60 * time.Second

func (o OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]any{"model": o.Model, "input": texts})
	ctx, cancel := context.WithTimeout(ctx, embedTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(o.URL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w (check the endpoint and that model %q is installed)", err, o.Model)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w (check the endpoint and that model %q is installed)", err, o.Model)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("embeddings: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: %d vectors for %d texts", len(r.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	dims := 0
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embeddings: index %d out of range", d.Index)
		}
		if out[d.Index] != nil {
			return nil, fmt.Errorf("embeddings: duplicate index %d", d.Index)
		}
		if dims == 0 {
			dims = len(d.Embedding)
		}
		if dims == 0 || len(d.Embedding) != dims {
			return nil, fmt.Errorf("embeddings: empty or inconsistent vector dimensions")
		}
		var norm float64
		for _, x := range d.Embedding {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return nil, fmt.Errorf("embeddings: non-finite vector")
			}
			norm += float64(x) * float64(x)
		}
		if norm == 0 {
			return nil, fmt.Errorf("embeddings: zero vector")
		}
		out[d.Index] = normalize(d.Embedding)
	}
	return out, nil
}

// Floored is an embedder with the cosine below which a query match is noise
// for its model.
type Floored struct {
	Embedder
	Floor float64
}

// Floor gives a known embedding model its noise floor, and leaves others as
// they are. embeddinggemma's: of 572 hits recalled before 2026-11-12, those
// below 0.28 answered "ok", "status?" or "Reply with only DONE" with
// unrelated items, while the one marked used scored 0.41. Task prompts were
// tried on the same data and dropped: they lost a relevant update procedure.
func Floor(e Embedder, model string) Embedder {
	if strings.HasPrefix(model, "embeddinggemma") {
		return Floored{Embedder: e, Floor: 0.28}
	}
	return e
}

// embedQuery is a search query's vector and the cosine below which a match
// is noise.
func embedQuery(ctx context.Context, e Embedder, text string) ([]float32, float64, error) {
	v, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, 0, err
	}
	floor := minCosine
	if f, ok := e.(Floored); ok {
		floor = f.Floor
	}
	return v[0], floor, nil
}

// embedCacheSize bounds retained texts and vectors; the cache is process-local
// and model-specific. Stored item vectors remain in SQLite across restarts.
const embedCacheSize = 256

type cachedEmbedder struct {
	base   Embedder
	mu     sync.Mutex
	values map[string][]float32
	order  []string
}

// NewCached avoids repeated model calls for identical queries and for the
// Related -> Remember path. Errors are never cached; vectors are copied so a
// caller cannot mutate a later result. FIFO eviction keeps memory bounded.
func NewCached(e Embedder) Embedder {
	return &cachedEmbedder{base: e, values: make(map[string][]float32)}
}

func (c *cachedEmbedder) Name() string { return c.base.Name() }

func (c *cachedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	var missing []string
	positions := make(map[string][]int)
	c.mu.Lock()
	for i, text := range texts {
		if v, ok := c.values[text]; ok {
			out[i] = append([]float32(nil), v...)
		} else {
			if _, ok := positions[text]; !ok {
				missing = append(missing, text)
			}
			positions[text] = append(positions[text], i)
		}
	}
	c.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	vectors, err := c.base.Embed(ctx, missing)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(missing) {
		return nil, fmt.Errorf("embeddings: %d vectors for %d texts", len(vectors), len(missing))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, text := range missing {
		v := vectors[i]
		for _, pos := range positions[text] {
			out[pos] = append([]float32(nil), v...)
		}
		if _, ok := c.values[text]; ok {
			continue
		}
		if len(c.order) == embedCacheSize {
			delete(c.values, c.order[0])
			c.order = c.order[1:]
		}
		c.values[text] = append([]float32(nil), v...)
		c.order = append(c.order, text)
	}
	return out, nil
}

func normalize(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return v
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v
}

// cosine of two unit vectors; 0 when their widths differ.
func cosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func pack(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func unpack(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}
