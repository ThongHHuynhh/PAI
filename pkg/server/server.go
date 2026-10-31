// Package server assembles the roles a pai-server process runs. Today
// that is one role, harness.
package server

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"

	"pai/pkg/auth"
	"pai/pkg/coordinator"
	"pai/pkg/events"
	"pai/pkg/harness"
	"pai/pkg/identity"
	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/relay"
	"pai/pkg/statefile"
	"pai/pkg/tunnel"
	"pai/pkg/worker"
)

// A person who saves the config sees it applied within this.
const reread = 2 * time.Second

// A provider body, a key and a URL, is far shorter than this.
const maxKey = 4 << 10

// Config is the self-hosted config file. Provider values are expanded from
// the environment, so a key can be "$ANTHROPIC_API_KEY". The directory it is
// in is the harness's: the harness key, the auth table, the mesh and the
// tunnel are there, and sessions in sessions/ under it.
type Config struct {
	HTTP      string              `json:"http"`             // loopback address the tunnel or the user's proxy carries connectors and workers to
	URL       string              `json:"url,omitempty"`    // the harness URL, which the user's own proxy carries to HTTP: no tunnel
	Tunnel    *Tunnel             `json:"tunnel,omitempty"` // the user's own Cloudflare tunnel; none, and no URL, is loopback only
	Providers map[string]Provider `json:"providers"`
	Links     [][2]string         `json:"links,omitempty"`  // pairs of workers, by name, that reach each other on the mesh
	Memory    *MemoryConfig       `json:"memory,omitempty"` // how memory embeds; none is the built-in hash embedder
	// Fallbacks names, for a provider, the one its sessions move to when its
	// account is out of usage, such as {"claude-code": "claude-code-2"}.
	Fallbacks map[string]string `json:"fallbacks,omitempty"`
	Balance   harness.Balance   `json:"balance,omitempty"`
}

// MemoryConfig names the embedder memory uses: an OpenAI compatible
// /embeddings endpoint, such as OpenAI's or Ollama's at
// http://localhost:11434/v1. Values expand from the environment.
type MemoryConfig struct {
	URL   string `json:"url"`
	Key   string `json:"key,omitempty"`
	Model string `json:"model"`
}

// embedder is the one c names, else the hash embedder, which needs nothing.
func (c *MemoryConfig) embedder() memory.Embedder {
	if c == nil || c.URL == "" || c.Model == "" {
		return memory.Hash{}
	}
	return memory.OpenAI{URL: os.ExpandEnv(c.URL), Key: os.ExpandEnv(c.Key), Model: os.ExpandEnv(c.Model)}
}

// Tunnel is a tunnel the user made in Cloudflare, which routes URL to it and
// it to HTTP.
type Tunnel struct {
	URL   string `json:"url"`
	Token string `json:"token"` // the tunnel's, as Cloudflare shows it
}

// Public is the URL workers and connectors reach the server at: the user's
// proxy's, the tunnel's, or with neither the loopback address, for one
// machine only.
func (c Config) Public() string {
	switch {
	case c.URL != "":
		return c.URL
	case c.Tunnel != nil:
		return c.Tunnel.URL
	}
	return "http://" + c.HTTP
}

// Auth is the auth table of the harness in dir.
func Auth(dir string) auth.Store { return auth.Store{Path: filepath.Join(dir, "auth.json")} }

// Key is the harness key every token carries, made on first use.
func Key(dir string) (ed25519.PrivateKey, error) {
	return identity.Key(filepath.Join(dir, "harness.key"))
}

// Provider is one the config names. Its name is free, and the catalog's ids
// are the names connectors know; its shape picks the code.
type Provider struct {
	Shape   string `json:"shape"`
	Key     string `json:"key,omitempty"`     // none for a shape that logs in
	URL     string `json:"url,omitempty"`     // the base url its SDK takes; none is the vendor's own
	Region  string `json:"region,omitempty"`  // for a shape whose endpoint names one
	Session string `json:"session,omitempty"` // the header the session's routing key goes in, for a server that routes its cache on one
}

// providerDir is the directory of provider name's own state, such as a
// login's tokens, in the harness's dir.
func providerDir(dir, name string) string { return filepath.Join(dir, "providers", name) }

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	c.named()
	return c, err
}

// saveConfig writes c over the config at path through a file beside it, so a
// read or a crash meets the old config or the new one, never half of one.
// Only the running harness calls it, under keyMu: the new file is its user's,
// as the old one was.
func saveConfig(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	// synced before the rename, so a power loss never leaves an empty config
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// named gives a provider with no shape its name as its shape, as each had in
// v0.5.3, so a config from then still loads.
func (c Config) named() {
	for name, p := range c.Providers {
		p.Shape = cmp.Or(p.Shape, name)
		c.Providers[name] = p
	}
}

// providers builds the config's providers. Each has a directory of its own
// under dir, where a login keeps what it needs.
func providers(dir string, cfg map[string]Provider) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	for name, p := range cfg {
		build, ok := provider.Shapes[p.Shape]
		if !ok {
			return nil, fmt.Errorf("provider %s: unknown shape %q; shapes: %s", name, p.Shape, strings.Join(slices.Sorted(maps.Keys(provider.Shapes)), ", "))
		}
		if p.Session != "" && !httpguts.ValidHeaderFieldName(p.Session) {
			return nil, fmt.Errorf("provider %s: session %q is not a header name", name, p.Session)
		}
		pp := build(provider.Config{Key: os.ExpandEnv(p.Key), URL: os.ExpandEnv(p.URL), Region: os.ExpandEnv(p.Region), Dir: providerDir(dir, name)})
		pp.Session = cmp.Or(p.Session, pp.Session)
		out[name] = pp
	}
	return out, nil
}

// Harness runs the harness role on one loopback port, behind the user's own
// proxy or tunnel, or on loopback alone, until ctx ends or the listener,
// the mesh node or the tunnel fails: /id proves the harness key to anyone before they send a
// secret, /ts2021 coordinates the mesh, /derp relays between its nodes,
// connectors call every other route with an api key, every session that was
// open on start resumes. The harness joins its own mesh, and workers dial it
// there. The relay map follows the server URL. The config
// at path is read again when it changes: providers and links apply
// at once.
func Harness(ctx context.Context, path string) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	if err := c.way(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	ps, err := providers(dir, c.Providers)
	if err != nil {
		return err
	}
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return err
	}
	k, err := Key(dir)
	if err != nil {
		return err
	}
	if err := network(dir); err != nil {
		return err
	}
	store := Auth(dir)
	t, err := store.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	limit := &auth.Limit{}
	code := func() (string, error) {
		n, err := LoadNetwork(dir)
		return n.Code, err
	}
	join := func(secret, addr string) (string, error) {
		if err := limit.Wait(addr); err != nil {
			return "", err
		}
		c, err := code()
		if err != nil {
			return "", err
		}
		switch {
		case hmac.Equal([]byte(secret), []byte(adminSecret(k, c))):
			return worker.Admin, nil
		case hmac.Equal([]byte(secret), []byte(auth.Secret(k, c))):
			return "", nil
		}
		limit.Refused(addr)
		return "", errors.New("join secret refused: install again with the server URL and the device code")
	}
	coord, err := coordinator.New(dir, k, join)
	if err != nil {
		return err
	}
	pool := harness.NewPool()
	coord.Gone = pool.Drop
	coord.Link(c.Links)
	bus := events.New()
	mem, err := memory.Open(filepath.Join(dir, "memory"), c.Memory.embedder())
	if err != nil {
		return err
	}
	defer func() { _ = mem.Close() }()
	mem.OnEvent = func(e memory.Event) {
		bus.Publish(events.Event{Type: "memory", Session: e.Session, Data: e})
	}
	pool.OnChange = func(name string, connected bool) {
		bus.Publish(events.Event{Type: "worker", Worker: name, Data: map[string]bool{"connected": connected}})
	}
	var fallbacks atomic.Pointer[map[string]string]
	fallbacks.Store(&c.Fallbacks)
	var balance atomic.Pointer[harness.Balance]
	balance.Store(&c.Balance)
	h := &harness.Harness{Sessions: statefile.Sessions{Dir: sessions}, Providers: ps, Workers: pool, Remove: coord.Remove, Rename: coord.Rename, Memory: mem,
		Fallbacks: func() map[string]string { return *fallbacks.Load() },
		Balance:   func() harness.Balance { return *balance.Load() },
		OnRecord:  func(id string, r statefile.Record) { bus.Publish(recordEvent(id, r)) }}
	h.SetProviders(ps)
	go watch(ctx, path, func(c Config) error {
		ps, err := providers(dir, c.Providers)
		if err == nil {
			h.SetProviders(ps)
			coord.Link(c.Links)
			fallbacks.Store(&c.Fallbacks)
			balance.Store(&c.Balance)
		}
		return err
	})
	var at atomic.Value
	at.Store("")
	mux := http.NewServeMux()
	mux.Handle("/id", identity.ID(k, func() string { return at.Load().(string) }, coord.Public().String()))
	mux.Handle("POST /join", auth.JoinHandler(k, identity.Encode(k), code, limit))
	coord.Handle(mux)
	derp := relay.New()
	derp.SetVerifyClientFunc(coord.Known)
	relay.Handle(mux, derp)
	routes := http.NewServeMux()
	routes.Handle("/", harness.Handler(ctx, h))
	routes.HandleFunc("PUT /providers/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := setProvider(path, r.PathValue("name"), r.Body, h.SetProviders); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	logins := &logins{ctx: ctx, path: path, serve: h.SetProviders, m: map[string]*login{}}
	routes.HandleFunc("POST /providers/{name}/login", logins.start)
	routes.HandleFunc("POST /providers/{name}/login/code", logins.paste)
	routes.HandleFunc("GET /providers/{name}/login", logins.get)
	a := &api{dir: dir, path: path, k: k, at: func() string { return at.Load().(string) }, coord: coord, pool: pool}
	a.handle(routes)
	(&webAPI{h: h, coord: coord, bus: bus, mem: mem, url: a.at, path: path}).handle(routes)
	app(mux)
	api := store.Bearer(limit, routes)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && browser(r) {
			http.Redirect(w, r, "/app/", http.StatusFound)
			return
		}
		api.ServeHTTP(w, r)
	}))
	srv := &http.Server{Addr: c.HTTP, Handler: mux}
	if c.URL != "" {
		srv.Handler = proxied(mux)
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		_ = derp.Close()
	}()
	if err := h.Resume(ctx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", c.HTTP)
	if err != nil {
		return err
	}
	log.Printf("harness: http on %s, sessions in %s, %d api keys", c.HTTP, sessions, len(t[auth.Key]))
	errs := make(chan error, 4)
	go func() { errs <- srv.Serve(ln) }()
	go func() {
		ls, err := coord.Listen(ctx, filepath.Join(dir, "mesh"), "http://"+c.HTTP, worker.Port, worker.APIPort)
		if err != nil {
			errs <- err
			return
		}
		// The CLI on a worker, over the mesh: no tunnel, so no CF-Connecting-IP
		// to trust; the limit counts the node's address.
		go func() {
			errs <- http.Serve(ls[1], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Header.Del("CF-Connecting-IP")
				api.ServeHTTP(w, r)
			}))
		}()
		errs <- pool.Serve(ls[0], coord.Node)
	}()
	up := func(url string) {
		log.Printf("harness: reached at %s", url)
		_ = os.Remove(filepath.Join(dir, tunnelFailed)) // from a run before; GET / shows it only until this one is up
		at.Store(url)
		go a.prove(ctx, url)
		coord.Relay(url)
	}
	go func() {
		err := reached(ctx, c, dir, up)
		// for Reached, which a service's log does not reach
		if ctx.Err() == nil {
			_ = os.WriteFile(filepath.Join(dir, tunnelFailed), []byte(err.Error()), 0o600)
		}
		errs <- err
	}()
	if err := <-errs; ctx.Err() == nil {
		return err
	}
	return nil
}

// way is why c gives the harness no one way in, if it does not.
func (c Config) way() error {
	if host, _, _ := net.SplitHostPort(c.HTTP); !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("http %q: the harness listens on loopback only, such as 127.0.0.1:8080; the tunnel or your proxy is the one way in", c.HTTP)
	}
	if u, err := url.Parse(c.URL); c.URL != "" && (c.Tunnel != nil || err != nil || u.Hostname() == "" || c.URL != "https://"+u.Hostname()) {
		return fmt.Errorf("url %q: https://<host>, with no port or path, that your proxy carries to http, and no tunnel beside it", c.URL)
	}
	return nil
}

// reached carries the harness URL to the harness until ctx ends, and calls up
// with the URL each time it is: the user's own proxy does at once, a tunnel
// each time cloudflared starts.
func reached(ctx context.Context, c Config, dir string, up func(url string)) error {
	switch {
	case c.URL != "":
		up(c.URL)
		<-ctx.Done()
		return ctx.Err()
	case c.Tunnel != nil:
		return tunnel.Own(ctx, dir, c.Tunnel.URL, c.Tunnel.Token, "http://"+c.HTTP, up)
	}
	up(c.Public())
	<-ctx.Done()
	return ctx.Err()
}

// proxied is h behind the user's own proxy, not cloudflared, so a client may
// send any CF-Connecting-IP: the proxy's own X-Forwarded-For hop, the last,
// is the client.
func proxied(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("CF-Connecting-IP")
		if f := r.Header.Values("X-Forwarded-For"); len(f) > 0 {
			hops := strings.Split(f[len(f)-1], ",")
			r.Header.Set("CF-Connecting-IP", strings.TrimSpace(hops[len(hops)-1]))
		}
		h.ServeHTTP(w, r)
	})
}

// keyMu keeps two config writes at once, of providers or settings, from
// writing over each other, and keeps reads apart from them: Windows fails a
// rename over a file that is open.
var keyMu sync.RWMutex

// loadConfig is Load for a reader in the running harness.
func loadConfig(path string) (Config, error) {
	keyMu.RLock()
	defer keyMu.RUnlock()
	return Load(path)
}

// setProvider makes the body, a Provider, the provider name in the config at
// path: the fields it sets replace the ones there, so a key alone renews the
// key of a provider the config has. It serves the providers it makes at
// once: the README runs pai new right after pai key, sooner than
// the watch rereads.
func setProvider(path, name string, body io.Reader, serve func(map[string]provider.Provider)) error {
	b, err := io.ReadAll(io.LimitReader(body, maxKey+1))
	if err != nil {
		return err
	}
	var in Provider
	if len(b) > maxKey || json.Unmarshal(b, &in) != nil {
		return fmt.Errorf("the body is a provider of at most %d bytes: {\"shape\",\"key\",\"url\",\"region\",\"session\"}", maxKey)
	}
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" || strings.ContainsAny(in.Key, "\r\n") {
		return errors.New("a key is one line")
	}
	return saveProvider(path, name, in, serve)
}

// saveProvider writes in over the provider name in the config at path, each
// field in sets, and serves the providers. The lock holds the write and serve
// in one order.
func saveProvider(path, name string, in Provider, serve func(map[string]provider.Provider)) error {
	keyMu.Lock()
	defer keyMu.Unlock()
	c, err := Load(path)
	if err != nil {
		return err
	}
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	p := c.Providers[name]
	for _, f := range [][2]*string{{&p.Shape, &in.Shape}, {&p.Key, &in.Key}, {&p.URL, &in.URL}, {&p.Region, &in.Region}, {&p.Session, &in.Session}} {
		if *f[1] != "" {
			*f[0] = *f[1]
		}
	}
	c.Providers[name] = p
	ps, err := providers(filepath.Dir(path), c.Providers)
	if err != nil {
		return err
	}
	if err := saveConfig(path, c); err != nil {
		return err
	}
	serve(ps)
	return nil
}

// watch applies the config at path each time it changes, and logs one it
// cannot apply: the harness keeps what it had.
func watch(ctx context.Context, path string, apply func(Config) error) {
	var mod time.Time
	if fi, err := os.Stat(path); err == nil {
		mod = fi.ModTime()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(reread):
		}
		fi, err := os.Stat(path)
		if err != nil || fi.ModTime().Equal(mod) {
			continue
		}
		mod = fi.ModTime()
		c, err := loadConfig(path)
		if err == nil {
			err = apply(c)
		}
		if err != nil {
			log.Printf("harness: %s: %v; the config before it holds", path, err)
			continue
		}
		log.Printf("harness: %s applied: %d providers, %d links; any other change applies at the next start", path, len(c.Providers), len(c.Links))
	}
}
