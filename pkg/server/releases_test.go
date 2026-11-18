package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pai/pkg/release"
)

// releaseFixture is a download server holding signed releases, and a
// harness dir whose config names it as the source.
type releaseFixture struct {
	dir   string
	files map[string]map[string][]byte // version: file: body
	hits  atomic.Int64
	up    atomic.Bool
	mux   *http.ServeMux
	src   *httptest.Server
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := release.Key
	release.Key = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { release.Key = old })
	f := &releaseFixture{dir: t.TempDir(), files: map[string]map[string][]byte{}}
	f.up.Store(true)
	for _, v := range []string{"v1.0.0", "v1.1.0-beta.1"} {
		body := map[string][]byte{"pai-linux-amd64": []byte("bin " + v), "install.sh": []byte("#!/bin/sh")}
		var sums strings.Builder
		for n, b := range body {
			fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(b), n)
		}
		body["SHA256SUMS"] = []byte(sums.String())
		body["SHA256SUMS.sig"] = ed25519.Sign(priv, body["SHA256SUMS"])
		f.files[v] = body
	}
	f.src = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		f.hits.Add(1)
		v, n, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if v == "latest" && n == "VERSION" {
			_, _ = w.Write([]byte("v1.0.0\n"))
			return
		}
		b, ok := f.files[v][n]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.src.Close)
	c := Config{HTTP: "127.0.0.1:0", Providers: map[string]Provider{}, Releases: &Releases{URL: f.src.URL}}
	b, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(f.dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.mux = http.NewServeMux()
	newReleases(f.dir, filepath.Join(f.dir, "config.json")).handle(f.mux)
	return f
}

func (f *releaseFixture) get(path string) (int, string) {
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

func TestReleasesFetchCheckAndCache(t *testing.T) {
	f := newReleaseFixture(t)
	if code, body := f.get("/releases/v1.0.0/pai-linux-amd64"); code != 200 || body != "bin v1.0.0" {
		t.Fatalf("%d %q", code, body)
	}
	// cached: served with the source down
	f.up.Store(false)
	if code, body := f.get("/releases/v1.0.0/pai-linux-amd64"); code != 200 || body != "bin v1.0.0" {
		t.Fatalf("not served from the cache: %d %q", code, body)
	}
	if code, _ := f.get("/releases/v1.0.0/SHA256SUMS.sig"); code != 200 {
		t.Fatalf("signature not served: %d", code)
	}
	f.up.Store(true)
	if code, body := f.get("/releases/latest/VERSION"); code != 200 || body != "v1.0.0\n" {
		t.Fatalf("latest: %d %q", code, body)
	}
}

func TestReleasesRefuseTamperedAndBadNames(t *testing.T) {
	f := newReleaseFixture(t)
	f.files["v1.1.0-beta.1"]["pai-linux-amd64"] = []byte("evil")
	if code, _ := f.get("/releases/v1.1.0-beta.1/pai-linux-amd64"); code != http.StatusBadGateway {
		t.Fatalf("a tampered file was served: %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "releases", "v1.1.0-beta.1", "pai-linux-amd64")); err == nil {
		t.Fatal("a tampered file was cached")
	}
	for _, p := range []string{"/releases/v1.0.0/..%2fconfig.json", "/releases/latest/pai-linux-amd64", "/releases/1.0.0/x", "/releases/v1.0.0/CON", "/releases/v1.0.0/not-in-sums"} {
		if code, _ := f.get(p); code == 200 {
			t.Fatalf("%s was served", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

func TestReleasesNeedKey(t *testing.T) {
	f := newReleaseFixture(t)
	release.Key = ""
	if code, _ := f.get("/releases/v1.0.0/pai-linux-amd64"); code != http.StatusServiceUnavailable {
		t.Fatalf("a build without a key served a release: %d", code)
	}
}

func TestReleasesPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	rs := newReleases(dir, filepath.Join(dir, "config.json"))
	for i := range 8 {
		d := filepath.Join(dir, "releases", fmt.Sprintf("v1.0.%d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(d, at, at)
	}
	rs.prune()
	es, _ := os.ReadDir(filepath.Join(dir, "releases"))
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	if len(names) != keepReleases-1 || names[0] != "v1.0.4" {
		t.Fatalf("kept %v", names)
	}
}

func TestReadTokenAndSource(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{"github_pat_abc\n": "github_pat_abc", "GH_TOKEN=github_pat_x\n": "github_pat_x", `GH_TOKEN="t1"`: "t1"} {
		p := filepath.Join(dir, "t")
		_ = os.WriteFile(p, []byte(body), 0o600)
		if got, err := ReadToken(p); err != nil || got != want {
			t.Errorf("%q: %q %v", body, got, err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "t"), []byte("a b\nc"), 0o600)
	if _, err := ReadToken(filepath.Join(dir, "t")); err == nil {
		t.Error("a file of several words was taken as a token")
	}
	if s, err := releaseSource(Config{}, dir); err != nil || s != (release.Source{}) {
		t.Fatalf("no setting must be the place built in: %v %v", s, err)
	}
	_ = os.WriteFile(filepath.Join(dir, releaseTokenFile), []byte("tok\n"), 0o600)
	s, err := releaseSource(Config{Releases: &Releases{GitHub: "o/r"}}, dir)
	if err != nil || s.Token != "tok" || s.GitHub != "o/r" {
		t.Fatalf("%+v %v", s, err)
	}
}

// Anyone may ask: made-up versions cost the source a bounded number of
// requests, a missing version is not asked again, latest is cached.
func TestReleasesBoundUpstreamCost(t *testing.T) {
	f := newReleaseFixture(t)
	for i := range 100 {
		f.get(fmt.Sprintf("/releases/v9.9.%d/pai-linux-amd64", i))
	}
	if n := f.hits.Load(); n > upstreamMax {
		t.Fatalf("100 made-up versions made %d upstream requests", n)
	}
	f.hits.Store(0)
	f.get("/releases/v9.9.0/pai-linux-amd64")
	if f.hits.Load() != 0 {
		t.Fatal("a version the source lacked was asked again at once")
	}
	for range 20 {
		f.get("/releases/latest/VERSION")
	}
	if n := f.hits.Load(); n > 1 {
		t.Fatalf("latest asked %d times in a minute", n)
	}
}

// Many machines asking for one file at once cause one download.
func TestReleasesOneFetchPerFile(t *testing.T) {
	f := newReleaseFixture(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := f.get("/releases/v1.0.0/pai-linux-amd64"); code != 200 || body != "bin v1.0.0" {
				t.Errorf("%d %q", code, body)
			}
		}()
	}
	wg.Wait()
	// SHA256SUMS, its signature, the file: three, however many asked
	if n := f.hits.Load(); n != 3 {
		t.Fatalf("%d upstream requests for one file", n)
	}
	es, _ := os.ReadDir(filepath.Join(f.dir, "releases", "v1.0.0"))
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("a temporary file was left: %s", e.Name())
		}
	}
}
