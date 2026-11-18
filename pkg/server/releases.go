package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pai/install"
	"pai/pkg/release"
	"pai/pkg/version"
)

// A server keeps this many releases' files: the one it runs, and the newest
// others, so a worker that follows a rollback finds it without a download.
const keepReleases = 5

// releaseTokenFile holds the token a private GitHub source reads with, in
// Dir beside the config, as private as the provider keys there.
const releaseTokenFile = "release.token"

// Anyone who reaches the server may ask for releases, so what they cost the
// source is bounded: the latest release is asked at most once a minute; a
// version the source does not have is not asked again for five minutes;
// at most upstreamMax new versions are asked in upstreamWindow, which a
// fleet updating to one release never nears, and which keeps a token's API
// quota (5000 an hour on GitHub) safe; two downloads run at once.
const (
	latestFresh    = time.Minute
	missingFor     = 5 * time.Minute
	upstreamWindow = 10 * time.Minute
	upstreamMax    = 30
	downloads      = 2
	maxVersionLen  = 64
)

// errUnavailable is what a client is told when a release cannot be served;
// the reason, which may name the source, goes to the log only.
var errUnavailable = errors.New("release unavailable")

// Releases is where the harness fetches the releases it serves: the source
// the config names, GitHub or a download server, or with none the place
// built into this binary, as before.
type Releases struct {
	GitHub string `json:"github,omitempty"` // owner/name, through the GitHub API; a private one needs release.token
	URL    string `json:"url,omitempty"`    // a download server laid out as <url>/<version>/<file>
}

// releaseSource is the source c names for the harness in dir, with the
// token beside it when there is one.
func releaseSource(c Config, dir string) (release.Source, error) {
	if c.Releases == nil {
		return release.Source{}, nil
	}
	s := release.Source{GitHub: c.Releases.GitHub, URL: c.Releases.URL}
	if s.GitHub != "" {
		t, err := storedToken(dir)
		if err != nil {
			return s, err
		}
		s.Token = t
	}
	return s, nil
}

func storedToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, releaseTokenFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// releases serves release files to the server's own machines: workers and
// CLIs update from it, and new machines install from it. It fetches each file
// once from the source, checks it against the signed SHA256SUMS, and keeps it
// under dir/releases. Files are served without a key: they are signed, not
// secret, and a machine about to join has no key yet. Whoever reaches the
// server can read them.
type releases struct {
	dir    string        // the harness's: its config, token and cache
	at     func() string // the harness's URL, or none yet
	config string        // the config path, read per fetch so a new source applies at once

	cache     sync.RWMutex // fetches and serves read; prune writes
	needPrune atomic.Bool
	sem       chan struct{} // downloads running

	mu       sync.Mutex
	inflight map[string]*fetching // by version/file: one fetch, the rest wait for it
	missing  map[string]time.Time // version: when the source last failed it
	asked    []time.Time          // when new versions were asked of the source
	latest   string
	latestAt time.Time
}

type fetching struct {
	done chan struct{}
	err  error
}

func newReleases(dir, config string, at func() string) *releases {
	return &releases{dir: dir, config: config, at: at, sem: make(chan struct{}, downloads),
		inflight: map[string]*fetching{}, missing: map[string]time.Time{}}
}

func (rs *releases) source() (release.Source, error) {
	c, err := Load(rs.config)
	if err != nil {
		return release.Source{}, err
	}
	return releaseSource(c, rs.dir)
}

// Latest is the newest stable release of the source, asked at most once a
// minute.
func (rs *releases) Latest(ctx context.Context) (string, error) {
	rs.mu.Lock()
	if rs.latest != "" && time.Since(rs.latestAt) < latestFresh {
		defer rs.mu.Unlock()
		return rs.latest, nil
	}
	rs.mu.Unlock()
	s, err := rs.source()
	if err != nil {
		return "", err
	}
	v, err := s.Latest(ctx)
	if err != nil {
		return "", err
	}
	rs.mu.Lock()
	rs.latest, rs.latestAt = v, time.Now()
	rs.mu.Unlock()
	return v, nil
}

func (rs *releases) handle(mux *http.ServeMux) {
	mux.HandleFunc("GET /releases/{v}/{file}", rs.serve)
	mux.HandleFunc("GET /install.sh", rs.script)
	mux.HandleFunc("GET /install.ps1", rs.script)
}

// script is the install script of the release this server runs, which
// fetches that release from this server: a new machine needs only its URL.
func (rs *releases) script(w http.ResponseWriter, r *http.Request) {
	if !release.HasKey() || !release.ValidVersion(version.V) {
		http.Error(w, "this server is not a signed release, so it serves no install script: install from the release page", http.StatusNotFound)
		return
	}
	base := ""
	if rs.at != nil {
		base = rs.at()
	}
	// the URL the server is reached at, never one a request names: it goes
	// into a script that runs, quoted, so it may hold no quote or space
	if base == "" || strings.ContainsAny(base, "'\"`$;&|<>(){}\\ \t\r\n") {
		http.Error(w, "this server is not reached at a URL yet: install from the release page", http.StatusServiceUnavailable)
		return
	}
	s, _ := install.Script(strings.TrimPrefix(r.URL.Path, "/"), version.V, strings.TrimRight(base, "/")+"/releases/"+version.V, release.Key)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(s))
}

func (rs *releases) serve(w http.ResponseWriter, r *http.Request) {
	v, file := r.PathValue("v"), r.PathValue("file")
	if v == "latest" {
		if file != "VERSION" {
			http.Error(w, "latest has only VERSION: name the release, /releases/<version>/"+file, http.StatusNotFound)
			return
		}
		l, err := rs.Latest(r.Context())
		if err != nil {
			log.Printf("releases: latest: %v", err)
			http.Error(w, errUnavailable.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(l + "\n"))
		return
	}
	if len(v) > maxVersionLen || !release.ValidVersion(v) || !release.ValidFile(file) {
		http.Error(w, "no such release file", http.StatusNotFound)
		return
	}
	if !release.HasKey() {
		http.Error(w, "this server was built without a release key, so it serves no releases", http.StatusServiceUnavailable)
		return
	}
	rs.cache.RLock()
	p, err := rs.file(r.Context(), v, file)
	if err == nil {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, p)
	}
	rs.cache.RUnlock()
	if rs.needPrune.CompareAndSwap(true, false) {
		rs.prune()
	}
	if err != nil {
		log.Printf("releases: %s/%s: %v", v, file, err)
		http.Error(w, errUnavailable.Error(), http.StatusBadGateway)
	}
}

// file is the path of file of release v in the cache, fetched and checked
// first when it is not there. One fetch of a file runs at a time; others
// asking for it wait for that one.
func (rs *releases) file(ctx context.Context, v, file string) (string, error) {
	vdir := filepath.Join(rs.dir, "releases", v)
	p := filepath.Join(vdir, file)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	k := v + "/" + file
	rs.mu.Lock()
	if f, ok := rs.inflight[k]; ok {
		rs.mu.Unlock()
		select {
		case <-f.done:
			return p, f.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f := &fetching{done: make(chan struct{})}
	rs.inflight[k] = f
	rs.mu.Unlock()
	// the fetch outlives the request that started it: others wait on it
	f.err = rs.fetch(context.WithoutCancel(ctx), v, file, vdir)
	rs.mu.Lock()
	delete(rs.inflight, k)
	rs.mu.Unlock()
	close(f.done)
	return p, f.err
}

func (rs *releases) fetch(ctx context.Context, v, file, vdir string) error {
	sums, err := rs.sums(ctx, v, vdir)
	if err != nil {
		return err
	}
	if file == "SHA256SUMS" || file == "SHA256SUMS.sig" {
		return nil
	}
	if release.Sum(sums, file) == "" {
		return fmt.Errorf("release %s has no %s", v, file)
	}
	s, err := rs.source()
	if err != nil {
		return err
	}
	rs.sem <- struct{}{}
	defer func() { <-rs.sem }()
	b, err := s.File(ctx, v, file, sums)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(vdir, file), b, 0o644)
}

// sums is release v's SHA256SUMS, from the cache when it holds a copy whose
// signature still checks, else from the source, checked and cached.
func (rs *releases) sums(ctx context.Context, v, vdir string) ([]byte, error) {
	sums, err1 := os.ReadFile(filepath.Join(vdir, "SHA256SUMS"))
	sig, err2 := os.ReadFile(filepath.Join(vdir, "SHA256SUMS.sig"))
	if err1 == nil && err2 == nil && verify(sums, sig) {
		return sums, nil
	}
	if err := rs.mayAsk(v); err != nil {
		return nil, err
	}
	s, err := rs.source()
	if err != nil {
		return nil, err
	}
	sums, sig, err = s.Sums(ctx, v)
	if err != nil {
		rs.mu.Lock()
		rs.missing[v] = time.Now()
		rs.mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(vdir, "SHA256SUMS.sig"), sig, 0o644); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(vdir, "SHA256SUMS"), sums, 0o644); err != nil {
		return nil, err
	}
	rs.needPrune.Store(true)
	return sums, nil
}

// mayAsk is whether the source may be asked for version v now: not a version
// it failed lately, and not past the bound on new versions.
func (rs *releases) mayAsk(v string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	now := time.Now()
	for k, at := range rs.missing {
		if now.Sub(at) > missingFor {
			delete(rs.missing, k)
		}
	}
	if at, ok := rs.missing[v]; ok {
		return fmt.Errorf("release %s was not at the source %s ago; asked again after %s", v, now.Sub(at).Round(time.Second), missingFor)
	}
	rs.asked = slices.DeleteFunc(rs.asked, func(t time.Time) bool { return now.Sub(t) > upstreamWindow })
	if len(rs.asked) >= upstreamMax {
		return fmt.Errorf("%d releases asked of the source in %s: try again later", len(rs.asked), upstreamWindow)
	}
	rs.asked = append(rs.asked, now)
	return nil
}

// prune keeps the release this server runs and the newest others by
// version, up to keepReleases in all. It waits for every fetch and serve.
func (rs *releases) prune() {
	rs.cache.Lock()
	defer rs.cache.Unlock()
	es, err := os.ReadDir(filepath.Join(rs.dir, "releases"))
	if err != nil {
		return
	}
	var vs []string
	for _, e := range es {
		if e.IsDir() && e.Name() != version.V && release.ValidVersion(e.Name()) {
			vs = append(vs, e.Name())
		}
	}
	slices.SortFunc(vs, func(a, b string) int {
		switch {
		case release.Older(a, b):
			return 1
		case release.Older(b, a):
			return -1
		}
		return 0
	})
	for _, v := range vs[min(len(vs), keepReleases-1):] {
		_ = os.RemoveAll(filepath.Join(rs.dir, "releases", v))
	}
}

// verify is whether sig is the release key's signature of sums.
func verify(sums, sig []byte) bool {
	k, err := base64.StdEncoding.DecodeString(release.Key)
	return err == nil && len(k) == ed25519.PublicKeySize && ed25519.Verify(k, sums, sig)
}

// writeFile writes b to p through a new file of its own name beside it and
// a rename: a reader never sees half a file, the temporary name is none a
// client may ask for, and a link planted at p is replaced, not written
// through. A file root writes for the harness gets Dir's owner.
func writeFile(p string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	err = errors.Join(err, f.Sync(), f.Close(), os.Chmod(tmp, mode))
	if err == nil && os.Geteuid() == 0 {
		err = likeDir(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}

// SetReleases makes the installed harness fetch releases from r, nil for the
// place built in. A GitHub source reads with token, or with no token given
// the one stored before. GitHub must answer before anything is saved; a
// download server may hold betas only, with no latest. It returns the newest
// stable release the source offers, or "".
func SetReleases(r *Releases, token string) (string, error) {
	if token == "" && r != nil && r.GitHub != "" {
		t, err := storedToken(Dir)
		if err != nil {
			return "", err
		}
		token = t
	}
	s := release.Source{}
	if r != nil {
		s = release.Source{GitHub: r.GitHub, URL: r.URL, Token: token}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// the repository must answer; a latest stable release it need not have
	if err := s.Reach(ctx); err != nil {
		hint := ""
		if token == "" {
			hint = "; a private repository needs --token-file"
		}
		return "", fmt.Errorf("%s does not answer: %w%s", s, err, hint)
	}
	latest, _ := s.Latest(ctx)
	tp := filepath.Join(Dir, releaseTokenFile)
	if r != nil && r.GitHub != "" && token != "" {
		if err := writeFile(tp, []byte(token+"\n"), 0o600); err != nil {
			return "", err
		}
	} else if err := os.Remove(tp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// read now, after the check, so nothing the harness saved meanwhile is lost
	c, err := Load(Conf)
	if err != nil {
		return "", fmt.Errorf("no installed harness: %w", err)
	}
	c.Releases = r
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeFile(Conf, b, 0o600); err != nil {
		return "", err
	}
	return latest, nil
}

// ReadToken is the token in file f: the whole file, or the value of its one
// NAME=value line, such as GH_TOKEN=... in an environment file.
func ReadToken(f string) (string, error) {
	b, err := os.ReadFile(f)
	if err != nil {
		return "", err
	}
	t := string(bytes.TrimSpace(b))
	if _, v, ok := strings.Cut(t, "="); ok && !strings.ContainsAny(t, "\n ") {
		t = v
	}
	t = strings.Trim(t, `"'`)
	if t == "" || strings.ContainsAny(t, " \t\r\n") {
		return "", fmt.Errorf("%s holds no single token", f)
	}
	return t, nil
}
