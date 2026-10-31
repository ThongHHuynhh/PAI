package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"pai/pkg/release"
	"pai/pkg/version"
	"pai/pkg/worker"
)

type settings struct {
	Version *versions `json:"version,omitempty"`
}

type versions struct {
	Current string        `json:"current"`
	Latest  string        `json:"latest,omitempty"`
	Build   version.Build `json:"build"`
}

// An update downloads one binary and restarts the harness.
const updateTime = 5 * time.Minute

func (a *api) settings(w http.ResponseWriter, r *http.Request) {
	// latest is optional: wait for it no longer than a connector waits
	latest, l := "", make(chan string, 1)
	go func() { v, _ := release.Latest(); l <- v }()
	select {
	case latest = <-l:
	case <-time.After(5 * time.Second):
	}
	reply(w, settings{&versions{version.V, latest, version.Info()}})
}

func (a *api) setSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxKey))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, fmt.Errorf(`the body is {"version"}: %w`, err))
		return
	}
	if in.Version != "" {
		// not 502, which a connector reads as the proxy losing the harness
		// to its restart
		if err := a.update(r.Context(), in.Version); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// update has the admin worker, which runs as root on this machine, make the
// harness release v.
func (a *api) update(ctx context.Context, v string) error {
	r, ok := a.pool.Get(worker.Admin)
	if !ok {
		return errors.New("the admin worker is not connected")
	}
	ctx, cancel := context.WithTimeout(ctx, updateTime)
	defer cancel()
	return r.Update(ctx, "pai-server", v)
}
