package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pai/pkg/coordinator"
	"pai/pkg/events"
	"pai/pkg/harness"
	"pai/pkg/memory"
	"pai/pkg/statefile"
	"pai/pkg/version"
	"pai/web"
)

// web is the routes the web app reads beside the sessions' and the
// server's, every one behind an api key:
//
//	GET    /events                      text/event-stream of events.Event, a hello first
//	GET    /graph                       -> the server, every machine and every session, for the graph
//	GET    /memory                      ?q= searches, else lists; ?kind= ?scope= ?session= ?invalid=1 ?limit=
//	GET    /memory/stats                -> memory.Stats with the bandit's arms
//	GET    /memory/retrievals           ?session= -> the newest automatic recalls, with why
//	POST   /memory                      {"text","kind","scope"} -> the item, made or merged
//	PATCH  /memory/{id}                 {"text","kind","scope"} and/or {"pinned"} -> the item
//	POST   /memory/{id}/invalidate      {"reason"} -> the item
//	POST   /memory/{id}/restore         -> the item
//	POST   /memory/{id}/feedback        {"up"} -> the item
//
// The app itself is at /app/, with no key: it asks for one.
type webAPI struct {
	h     *harness.Harness
	coord *coordinator.Coordinator
	bus   *events.Bus
	mem   *memory.Store
	url   func() string
}

// heartbeat keeps an idle event stream open through proxies that end quiet
// connections.
const heartbeat = 25 * time.Second

// maxMemoryBody bounds a memory write: an item is a sentence or a summary.
const maxMemoryBody = 256 << 10

func (wa *webAPI) handle(mux *http.ServeMux) {
	mux.HandleFunc("GET /events", wa.events)
	mux.HandleFunc("GET /graph", wa.graph)
	mux.HandleFunc("GET /memory", wa.list)
	mux.HandleFunc("GET /memory/stats", wa.stats)
	mux.HandleFunc("GET /memory/retrievals", wa.retrievals)
	mux.HandleFunc("POST /memory", wa.add)
	mux.HandleFunc("PATCH /memory/{id}", wa.patch)
	mux.HandleFunc("POST /memory/{id}/invalidate", wa.invalidate)
	mux.HandleFunc("POST /memory/{id}/restore", wa.restore)
	mux.HandleFunc("POST /memory/{id}/feedback", wa.feedback)
}

// app serves the web app's files with no key, and sends a browser that asks
// for / there.
func app(mux *http.ServeMux) {
	files, _ := fs.Sub(web.Dist, "dist")
	mux.Handle("GET /app/", http.StripPrefix("/app/", http.FileServerFS(files)))
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/app/", http.StatusFound) })
}

// browser is whether r is a person's browser asking for a page.
func browser(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (wa *webAPI) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx
	c, cancel := wa.bus.Subscribe()
	defer cancel()
	send := func(e events.Event) bool {
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send(events.Event{Type: "hello", At: time.Now().UTC(), Data: version.V}) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-c:
			if !send(e) {
				return
			}
		case <-time.After(heartbeat):
			if _, err := fmt.Fprint(w, ": beat\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// Machine is a node of the graph: what the mesh knows of it, and what its
// worker said when it connected.
type Machine struct {
	coordinator.Machine
	Connected bool     `json:"connected"` // its worker is connected to the server
	OSArch    string   `json:"os_arch,omitempty"`
	Interps   []string `json:"interps,omitempty"`
	Version   string   `json:"version,omitempty"`
	Refused   string   `json:"refused,omitempty"`
}

type graph struct {
	Server struct {
		Version string `json:"version"`
		URL     string `json:"url"`
	} `json:"server"`
	Machines []Machine         `json:"machines"`
	Sessions []harness.Info    `json:"sessions"`
	Memory   *memory.Stats     `json:"memory,omitempty"`
	Bandit   []memory.ArmStats `json:"bandit,omitempty"`
}

func (wa *webAPI) graph(w http.ResponseWriter, r *http.Request) {
	var g graph
	g.Server.Version, g.Server.URL = version.V, wa.url()
	listed := map[string]harness.Listed{}
	for _, l := range wa.h.Workers.List() {
		listed[l.Name] = l
	}
	for _, m := range wa.coord.Machines() {
		gm := Machine{Machine: m}
		if l, ok := listed[m.Name]; ok {
			gm.Connected, gm.Refused = l.Refused == "", l.Refused
			gm.OSArch, gm.Interps, gm.Version = l.OS+"/"+l.Arch, l.Interps, l.Version
		}
		g.Machines = append(g.Machines, gm)
	}
	infos, err := wa.h.List()
	if err != nil {
		fail(w, err)
		return
	}
	g.Sessions = infos
	if wa.mem != nil {
		if st, err := wa.mem.Stats(r.Context()); err == nil {
			g.Memory = &st
		}
		g.Bandit, _ = wa.mem.Bandit(r.Context())
	}
	reply(w, g)
}

func (wa *webAPI) store(w http.ResponseWriter) bool {
	if wa.mem == nil {
		http.Error(w, "this server has no memory store", http.StatusNotFound)
		return false
	}
	return true
}

func (wa *webAPI) list(w http.ResponseWriter, r *http.Request) {
	if !wa.store(w) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if s := strings.TrimSpace(q.Get("q")); s != "" {
		hits, err := wa.mem.Search(r.Context(), s, q.Get("session"), max(limit, 30))
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, orNone(hits))
		return
	}
	items, err := wa.mem.List(r.Context(), memory.Filter{Kind: q.Get("kind"), Scope: q.Get("scope"), Session: q.Get("session"), Invalid: q.Get("invalid") == "1", Limit: limit})
	if err != nil {
		fail(w, err)
		return
	}
	hits := make([]memory.Hit, len(items))
	for i, it := range items {
		hits[i] = memory.Hit{Item: it}
	}
	reply(w, hits)
}

func orNone(h []memory.Hit) []memory.Hit {
	if h == nil {
		return []memory.Hit{}
	}
	return h
}

func (wa *webAPI) stats(w http.ResponseWriter, r *http.Request) {
	if !wa.store(w) {
		return
	}
	st, err := wa.mem.Stats(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	arms, err := wa.mem.Bandit(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, map[string]any{"stats": st, "bandit": arms})
}

func (wa *webAPI) retrievals(w http.ResponseWriter, r *http.Request) {
	if !wa.store(w) {
		return
	}
	rs, err := wa.mem.Retrievals(r.Context(), r.URL.Query().Get("session"), 100)
	if err != nil {
		fail(w, err)
		return
	}
	if rs == nil {
		rs = []memory.Retrieval{}
	}
	reply(w, rs)
}

type memoryBody struct {
	Text, Kind, Scope, Reason string
	Pinned                    *bool
	Up                        bool
}

func decodeMemory(w http.ResponseWriter, r *http.Request) (memoryBody, bool) {
	var b memoryBody
	if r.ContentLength == 0 {
		return b, true
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxMemoryBody)).Decode(&b); err != nil && err != io.EOF {
		fail(w, err)
		return b, false
	}
	return b, true
}

func (wa *webAPI) add(w http.ResponseWriter, r *http.Request) {
	b, ok := decodeMemory(w, r)
	if !ok || !wa.store(w) {
		return
	}
	it, _, err := wa.mem.Remember(r.Context(), memory.Item{Text: b.Text, Kind: b.Kind, Scope: b.Scope, Source: "person"})
	wa.answer(w, it, err)
}

func (wa *webAPI) patch(w http.ResponseWriter, r *http.Request) {
	b, ok := decodeMemory(w, r)
	if !ok || !wa.store(w) {
		return
	}
	id := r.PathValue("id")
	var it memory.Item
	var err error
	if b.Text != "" || b.Kind != "" || b.Scope != "" {
		if it, err = wa.mem.Edit(r.Context(), id, b.Text, b.Kind, b.Scope); err != nil {
			fail(w, err)
			return
		}
	}
	if b.Pinned != nil {
		it, err = wa.mem.Pin(r.Context(), id, *b.Pinned)
	}
	if it.ID == "" && err == nil {
		it, err = wa.mem.Get(r.Context(), id)
	}
	wa.answer(w, it, err)
}

func (wa *webAPI) invalidate(w http.ResponseWriter, r *http.Request) {
	b, ok := decodeMemory(w, r)
	if !ok || !wa.store(w) {
		return
	}
	if strings.TrimSpace(b.Reason) == "" {
		b.Reason = "invalidated by a person"
	}
	it, err := wa.mem.Invalidate(r.Context(), r.PathValue("id"), b.Reason)
	wa.answer(w, it, err)
}

func (wa *webAPI) restore(w http.ResponseWriter, r *http.Request) {
	if !wa.store(w) {
		return
	}
	it, err := wa.mem.Restore(r.Context(), r.PathValue("id"))
	wa.answer(w, it, err)
}

func (wa *webAPI) feedback(w http.ResponseWriter, r *http.Request) {
	b, ok := decodeMemory(w, r)
	if !ok || !wa.store(w) {
		return
	}
	it, err := wa.mem.Feedback(r.Context(), r.PathValue("id"), b.Up)
	wa.answer(w, it, err)
}

func (wa *webAPI) answer(w http.ResponseWriter, it memory.Item, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, it)
}

// recordEvent is a record as the event stream carries it: its header, and
// for what people read, the start of its body.
func recordEvent(id string, r statefile.Record) events.Event {
	data := map[string]any{"n": r.N, "kind": r.Kind, "id": r.ID, "time": r.Time, "via": r.Via, "source": r.Source}
	switch r.Kind {
	case statefile.Message, statefile.Text, statefile.Compaction, statefile.Error, statefile.System:
		if strings.HasPrefix(r.Type, "text/") {
			data["text"] = clip(string(r.Body), 400)
		}
	case statefile.Call:
		data["text"] = clip(string(r.Body), 200)
	case statefile.Step:
		data["model"], data["usage"], data["stop"], data["provider"] = r.Model, r.Usage, r.Stop, r.Provider
	}
	return events.Event{Type: "record", Session: id, Data: data}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
