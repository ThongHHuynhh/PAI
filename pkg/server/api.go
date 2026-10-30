package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"pai/pkg/auth"
	"pai/pkg/coordinator"
	"pai/pkg/harness"
	"pai/pkg/identity"
	"pai/pkg/version"
)

// The URL answers a moment after the proxy or cloudflared does; a second
// between tries shows ready soon after.
const proveRetry = time.Second

// Network states in GET /: no worker can find the harness yet, or one can.
const (
	starting = "starting"
	ready    = "ready"
)

// api is the routes of the harness's machine and config, beside the
// sessions' in harness.Handler:
//
//	GET    /               -> {"version","network"}: network is "ready" once a worker can
//	                          find the harness at its URL, else why the tunnel last stopped
//	                          the harness while it starts again, else "starting"
//	GET    /join           -> {"join"}: the server URL and device code, as a worker
//	                          installs with them
//	POST   /keys           {"name"} -> {"name","key"}: a new api key; a name in use is 409
//	GET    /settings       -> {"version":{"current","latest"}}
//	POST   /settings       {"version"} -> the admin worker makes the server that release
//	                          and restarts it, so a success seldom answers
type api struct {
	dir, path string
	k         ed25519.PrivateKey
	at        func() string // the harness's URL, or none yet
	proving   atomic.Value  // the URL prove last started on
	reached   atomic.Value  // the URL a worker last found the harness at
	coord     *coordinator.Coordinator
	pool      *harness.Pool
}

func (a *api) handle(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", a.root)
	mux.HandleFunc("GET /join", a.join)
	mux.HandleFunc("POST /keys", a.key)
	mux.HandleFunc("GET /settings", a.settings)
	mux.HandleFunc("POST /settings", a.setSettings)
}

func (a *api) root(w http.ResponseWriter, r *http.Request) {
	network := starting
	if u, _ := a.reached.Load().(string); u != "" && u == a.at() {
		network = ready
	} else if b, err := os.ReadFile(filepath.Join(a.dir, tunnelFailed)); err == nil {
		network = string(b)
	}
	reply(w, map[string]string{"version": version.V, "network": network})
}

// prove marks url reached once a worker can find the harness there and get
// on the mesh: url proves the server key and passes
// the WebSocket upgrade the coordinator and the relay need. It gives up when the harness moves to another URL. A
// tunnel that starts again at the same URL proves it once.
func (a *api) prove(ctx context.Context, url string) {
	if old, _ := a.proving.Swap(url).(string); old == url {
		return
	}
	for a.at() == url {
		if u, err := reach(ctx, identity.Encode(a.k), url); err == nil && u == url {
			a.reached.Store(url)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(proveRetry):
		}
	}
}

func (a *api) join(w http.ResponseWriter, r *http.Request) {
	n, err := LoadNetwork(a.dir)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, map[string]string{"join": n.String()})
}

func (a *api) key(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, maxKey)).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" || strings.ContainsAny(in.Name, "\r\n") {
		fail(w, errors.New(`the body is {"name"}, a name of one line`))
		return
	}
	secret, err := Auth(a.dir).Issue(auth.Key, in.Name)
	if errors.Is(err, auth.ErrTaken) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, map[string]string{"name": in.Name, "key": auth.Token(auth.Key, identity.Encode(a.k), secret, a.at())})
}
