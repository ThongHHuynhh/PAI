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
	body, _ := json.Marshal(map[string]any{"model": o.Model, "input": texts})
	ctx, cancel := context.WithTimeout(ctx, embedTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(o.URL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
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
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embeddings: index %d out of range", d.Index)
		}
		out[d.Index] = normalize(d.Embedding)
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
