package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"pai/pkg/catalog"
	"pai/pkg/memory"
	"pai/pkg/statefile"
)

// GET /metrics?range=24h|7d|30d|all is how PAI performs over the range: the
// models it steps, the sessions and their context, the tools the harness
// runs, and what memory collects and recalls. It reads every session file
// once per request; the files are the source of truth, so nothing is kept on
// the side to drift from them.

// Ranges are the windows /metrics takes, and each one's bucket for the series.
var ranges = map[string]struct {
	span, bucket time.Duration
}{
	"24h": {24 * time.Hour, time.Hour},
	"7d":  {7 * 24 * time.Hour, 24 * time.Hour},
	"30d": {30 * 24 * time.Hour, 24 * time.Hour},
	"all": {0, 24 * time.Hour},
}

// maxBuckets bounds the series of "all": past it a bucket is a week.
const maxBuckets = 120

// growthSteps is how many of a session's newest steps its context curve keeps.
const growthSteps = 60

// recentErrors is how many of the newest errors the harness section lists.
const recentErrors = 8

type Metrics struct {
	Range    string           `json:"range"`
	Since    time.Time        `json:"since"`
	Bucket   string           `json:"bucket"` // hour, day or week
	Totals   Totals           `json:"totals"`
	Series   []Point          `json:"series"`
	Models   []ModelStats     `json:"models"`
	Sessions []SessionStats   `json:"sessions"`
	Tools    []ToolStats      `json:"tools"`
	Harness  HarnessStats     `json:"harness"`
	Memory   *MemoryMetrics   `json:"memory,omitempty"`
	Limits   map[string]Limit `json:"limits,omitempty"` // by provider, once it has reported one
	Priced   bool             `json:"priced"`           // this build's catalog has prices
	Stops    map[string]int   `json:"stops"`
}

type Totals struct {
	Sessions  int             `json:"sessions"` // with a record in the range
	Messages  int             `json:"messages"`
	Steps     int             `json:"steps"`
	Usage     statefile.Usage `json:"usage"`
	Cost      float64         `json:"cost"`
	ToolCalls int             `json:"tool_calls"`
	Errors    int             `json:"errors"`
	P50       int64           `json:"p50_ms"`
	P95       int64           `json:"p95_ms"`
	CacheHit  float64         `json:"cache_hit"` // cache reads over the prompt
}

// Point is one bucket of the series.
type Point struct {
	At     time.Time `json:"at"`
	Steps  int       `json:"steps"`
	Tokens int       `json:"tokens"` // prompt and output
	P50    int64     `json:"p50_ms"`
	Errors int       `json:"errors"`
	Learnt int       `json:"learnt"`
	Reward *float64  `json:"reward,omitempty"` // mean of the recalls settled in it
	lat    []int64
	reward []float64
}

type ModelStats struct {
	Model    string          `json:"model"` // provider/model
	Steps    int             `json:"steps"`
	Usage    statefile.Usage `json:"usage"`
	Cost     *float64        `json:"cost,omitempty"` // none when no price is known
	Priced   string          `json:"priced,omitempty"`
	P50      int64           `json:"p50_ms"`
	P95      int64           `json:"p95_ms"`
	CacheHit float64         `json:"cache_hit"`
	Errors   int             `json:"errors"`
	Stops    map[string]int  `json:"stops"`
	lat      []int64
}

type SessionStats struct {
	ID           string          `json:"id"`
	Name         string          `json:"name,omitempty"`
	Model        string          `json:"model"`
	Status       string          `json:"status"`
	ContextUsed  int             `json:"context_used"`
	ContextLimit int             `json:"context_limit"`
	Steps        int             `json:"steps"`
	Messages     int             `json:"messages"`
	Compactions  int             `json:"compactions"`
	Usage        statefile.Usage `json:"usage"`
	Cost         float64         `json:"cost"`
	Updated      time.Time       `json:"updated"`
	Growth       []int           `json:"growth"` // the prompt of each of its newest steps
}

type ToolStats struct {
	Name      string `json:"name"`
	Calls     int    `json:"calls"`
	Failures  int    `json:"failures"`
	Truncated int    `json:"truncated"`
	P50       int64  `json:"p50_ms"`
	P95       int64  `json:"p95_ms"`
	lat       []int64
}

type HarnessStats struct {
	StepsPerMessage float64       `json:"steps_per_message"`
	Compactions     int           `json:"compactions"`
	Failovers       int           `json:"failovers"`
	Recent          []ErrorSample `json:"recent_errors"`
}

type ErrorSample struct {
	Session string    `json:"session"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
}

type MemoryMetrics struct {
	Items     int               `json:"items"` // valid now
	Learnt    int               `json:"learnt"`
	ByKind    map[string]int    `json:"by_kind"`
	BySource  map[string]int    `json:"by_source"`
	Merged    int               `json:"merged"`
	Forgotten int               `json:"forgotten"`
	Recalls   int               `json:"recalls"`
	Skipped   int               `json:"skipped"`
	Reward    *float64          `json:"reward,omitempty"`
	UsedRate  float64           `json:"used_rate"` // all time: memory keeps no per-recall use
	RecallP50 int64             `json:"recall_p50_ms"`
	RecallP95 int64             `json:"recall_p95_ms"`
	Bandit    []memory.ArmStats `json:"bandit"`
}

func (wa *webAPI) metrics(w http.ResponseWriter, r *http.Request) {
	name := cmp.Or(r.URL.Query().Get("range"), "7d")
	rg, ok := ranges[name]
	if !ok {
		fail(w, fmt.Errorf("range is one of 24h, 7d, 30d, all"))
		return
	}
	m, err := wa.collect(r.Context(), name, rg.span, rg.bucket, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, m)
}

func (wa *webAPI) collect(ctx context.Context, name string, span, bucket time.Duration, now time.Time) (*Metrics, error) {
	infos, err := wa.h.List()
	if err != nil {
		return nil, err
	}
	since := time.Time{}
	if span > 0 {
		since = now.Add(-span)
	}
	chains := map[string][]statefile.Record{}
	first := now
	for _, in := range infos {
		chain, err := wa.h.Sessions.Load(in.ID)
		if err != nil {
			continue // a create that died before its first write
		}
		chains[in.ID] = chain
		if t := chain[0].Time; t.Before(first) {
			first = t
		}
	}
	if since.IsZero() {
		since = first
		for now.Sub(since)/bucket > maxBuckets {
			bucket *= 7
		}
	}
	since = since.Truncate(bucket)
	m := &Metrics{Range: name, Since: since, Bucket: map[time.Duration]string{time.Hour: "hour", 24 * time.Hour: "day"}[bucket], Stops: map[string]int{},
		Models: []ModelStats{}, Sessions: []SessionStats{}, Tools: []ToolStats{}} // lists, never null
	if m.Bucket == "" {
		m.Bucket = "week"
	}
	for t := since; !t.After(now); t = t.Add(bucket) {
		m.Series = append(m.Series, Point{At: t})
	}
	at := func(t time.Time) *Point {
		if t.Before(since) {
			return nil
		}
		i := int(t.Sub(since) / bucket)
		if i >= len(m.Series) {
			i = len(m.Series) - 1
		}
		return &m.Series[i]
	}
	cat, _ := catalog.Load()
	models, tools := map[string]*ModelStats{}, map[string]*ToolStats{}
	var all []int64
	var errs []ErrorSample
	for _, in := range infos {
		chain, ok := chains[in.ID]
		if !ok {
			continue
		}
		ss := SessionStats{ID: in.ID, Name: in.Name, Model: in.Config.Model, Status: string(in.Status), ContextUsed: in.ContextUsed, ContextLimit: in.Config.ContextLimit, Updated: in.Updated, Growth: []int{}}
		model, calls, active := "", map[string]statefile.Record{}, false
		for _, rec := range chain {
			if rec.Kind == statefile.Config {
				var c statefile.Conf
				if json.Unmarshal(rec.Body, &c) == nil {
					model = c.Model
				}
				if rec.Via == "failover" && !rec.Time.Before(since) {
					m.Harness.Failovers++
				}
			}
			if rec.Kind == statefile.Call {
				calls[rec.ID] = rec
			}
			if rec.Kind == statefile.Step && rec.Usage != nil {
				ss.Growth = append(ss.Growth, rec.Usage.Prompt())
			}
			if rec.Time.Before(since) {
				continue
			}
			active = true
			p := at(rec.Time)
			switch rec.Kind {
			case statefile.Message:
				if rec.Via != "pai" { // pai's own messages, such as a job's exit, are not a person's
					ss.Messages++
					m.Totals.Messages++
				}
			case statefile.Compaction:
				ss.Compactions++
				m.Harness.Compactions++
			case statefile.Error:
				m.Totals.Errors++
				p.Errors++
				stats(models, cmp.Or(model, in.Config.Model)).Errors++
				errs = append(errs, ErrorSample{Session: in.ID, At: rec.Time, Text: clip(string(rec.Body), 240)})
				if c, ok := calls[rec.For]; ok {
					tool(tools, c).Failures++
				}
			case statefile.Result:
				c, ok := calls[rec.For]
				if !ok {
					break
				}
				t := tool(tools, c)
				t.Calls++
				m.Totals.ToolCalls++
				t.lat = append(t.lat, rec.Time.Sub(c.Time).Milliseconds())
				if rec.Exit != nil && *rec.Exit != 0 {
					t.Failures++
				}
				if rec.Truncated {
					t.Truncated++
				}
			case statefile.Step:
				key := rec.Provider + "/" + rec.Model
				if rec.Provider == "" {
					key = cmp.Or(model, rec.Model)
				}
				ms := stats(models, key)
				ms.Steps++
				ss.Steps++
				m.Totals.Steps++
				p.Steps++
				stop := cmp.Or(rec.Stop, "unknown")
				ms.Stops[stop]++
				m.Stops[stop]++
				if u := rec.Usage; u != nil {
					add(&ms.Usage, u)
					add(&ss.Usage, u)
					add(&m.Totals.Usage, u)
					p.Tokens += u.Prompt() + u.Output
					if c, ok := price(cat, key); ok {
						usd := cost(c, u)
						if ms.Cost == nil {
							ms.Cost = new(0.0)
						}
						*ms.Cost += usd
						ss.Cost += usd
						m.Totals.Cost += usd
					}
				}
				if !rec.Sent.IsZero() {
					l := rec.Time.Sub(rec.Sent).Milliseconds()
					ms.lat, p.lat, all = append(ms.lat, l), append(p.lat, l), append(all, l)
				}
			}
		}
		if !active {
			continue
		}
		m.Totals.Sessions++
		if len(ss.Growth) > growthSteps {
			ss.Growth = ss.Growth[len(ss.Growth)-growthSteps:]
		}
		m.Sessions = append(m.Sessions, ss)
	}

	m.Totals.P50, m.Totals.P95 = pct(all, 50), pct(all, 95)
	m.Totals.CacheHit = hit(m.Totals.Usage)
	for i := range m.Series {
		m.Series[i].P50 = pct(m.Series[i].lat, 50)
	}
	for k, ms := range models {
		ms.P50, ms.P95, ms.CacheHit = pct(ms.lat, 50), pct(ms.lat, 95), hit(ms.Usage)
		if c, ok := price(cat, k); ok {
			m.Priced = true
			ms.Priced = "list"
			if c.Model != k {
				ms.Priced = c.Model // what an API account would have paid
			}
		} else {
			ms.Cost = nil
		}
		m.Models = append(m.Models, *ms)
	}
	slices.SortFunc(m.Models, func(a, b ModelStats) int { return b.Steps - a.Steps })
	for _, t := range tools {
		t.P50, t.P95 = pct(t.lat, 50), pct(t.lat, 95)
		m.Tools = append(m.Tools, *t)
	}
	slices.SortFunc(m.Tools, func(a, b ToolStats) int { return b.Calls - a.Calls })
	slices.SortFunc(m.Sessions, func(a, b SessionStats) int { return b.Updated.Compare(a.Updated) })
	if m.Totals.Messages > 0 {
		m.Harness.StepsPerMessage = float64(m.Totals.Steps) / float64(m.Totals.Messages)
	}
	slices.SortFunc(errs, func(a, b ErrorSample) int { return b.At.Compare(a.At) })
	m.Harness.Recent = errs[:min(len(errs), recentErrors)]
	if m.Harness.Recent == nil {
		m.Harness.Recent = []ErrorSample{}
	}
	if wa.mem != nil {
		mm, err := wa.memoryMetrics(ctx, since, at)
		if err != nil {
			return nil, err
		}
		m.Memory = mm
		for i, p := range m.Series {
			if len(p.reward) > 0 {
				var sum float64
				for _, r := range p.reward {
					sum += r
				}
				m.Series[i].Reward = new(sum / float64(len(p.reward)))
			}
		}
	}
	status := map[string]string{}
	for _, a := range wa.h.Accounts() {
		prov, _, _ := strings.Cut(a.Name, "/") // a pool lists provider/model
		status[prov] = a.Status
	}
	for name, h := range wa.h.Health() {
		l := Limit{At: h.Updated, Status: status[name], Windows: map[string]Window{}}
		for k, u := range h.Util {
			l.Windows[k] = Window{u, h.Resets[k]}
		}
		if m.Limits == nil {
			m.Limits = map[string]Limit{}
		}
		m.Limits[name] = l
	}
	return m, nil
}

// Limit is a provider's usage windows as it last reported them.
type Limit struct {
	At      time.Time         `json:"at"`
	Status  string            `json:"status"` // ok, or limited until it resets
	Windows map[string]Window `json:"windows"`
}

// Window is the share of one usage window spent, 0 to 1, and when it resets.
type Window struct {
	Utilization float64   `json:"utilization"`
	Resets      time.Time `json:"resets"`
}

func (wa *webAPI) memoryMetrics(ctx context.Context, since time.Time, at func(time.Time) *Point) (*MemoryMetrics, error) {
	mm := &MemoryMetrics{ByKind: map[string]int{}, BySource: map[string]int{}}
	items, err := wa.mem.List(ctx, memory.Filter{Invalid: true, Limit: 1 << 30})
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.Invalid.IsZero() {
			mm.Items++
		} else if !it.Invalid.Before(since) {
			if strings.HasPrefix(it.Reason, "merged into") || strings.HasPrefix(it.Reason, "superseded") || strings.HasPrefix(it.Reason, "replaced") {
				mm.Merged++
			} else {
				mm.Forgotten++
			}
		}
		if it.Created.Before(since) {
			continue
		}
		mm.Learnt++
		mm.ByKind[it.Kind]++
		mm.BySource[it.Source]++
		if p := at(it.Created); p != nil {
			p.Learnt++
		}
	}
	rs, err := wa.mem.Retrievals(ctx, "", 1<<30)
	if err != nil {
		return nil, err
	}
	var sum float64
	var n int
	var lat []int64
	for _, r := range rs {
		if r.At.Before(since) {
			continue
		}
		if r.K == 0 {
			mm.Skipped++
		} else {
			mm.Recalls++
			lat = append(lat, r.Ms)
		}
		if r.Reward != nil {
			sum, n = sum+*r.Reward, n+1
			if p := at(r.At); p != nil {
				p.reward = append(p.reward, *r.Reward)
			}
		}
	}
	if n > 0 {
		mm.Reward = new(sum / float64(n))
	}
	mm.RecallP50, mm.RecallP95 = pct(lat, 50), pct(lat, 95)
	st, err := wa.mem.Stats(ctx)
	if err != nil {
		return nil, err
	}
	mm.UsedRate = st.UsedRate
	if mm.Bandit, err = wa.mem.Bandit(ctx); err != nil {
		return nil, err
	}
	return mm, nil
}

func stats(models map[string]*ModelStats, key string) *ModelStats {
	if models[key] == nil {
		models[key] = &ModelStats{Model: key, Stops: map[string]int{}}
	}
	return models[key]
}

// tool is the stats of the tool call c named.
func tool(tools map[string]*ToolStats, c statefile.Record) *ToolStats {
	var body struct{ Name string }
	_ = json.Unmarshal(c.Body, &body)
	name := cmp.Or(body.Name, "unknown")
	if tools[name] == nil {
		tools[name] = &ToolStats{Name: name}
	}
	return tools[name]
}

func add(to *statefile.Usage, u *statefile.Usage) {
	to.Input += u.Input
	to.Output += u.Output
	to.CacheRead += u.CacheRead
	to.CacheWrite += u.CacheWrite
}

func hit(u statefile.Usage) float64 {
	if p := u.Prompt(); p > 0 {
		return float64(u.CacheRead) / float64(p)
	}
	return 0
}

// pct is the p-th percentile of ls, which it sorts.
func pct(ls []int64, p int) int64 {
	if len(ls) == 0 {
		return 0
	}
	slices.Sort(ls)
	return ls[min(len(ls)-1, len(ls)*p/100)]
}

// priced is a preset's prices and the preset they are from.
type priced struct {
	catalog.Cost
	Model string
}

// price is what model costs: its preset's prices, or, for a subscription,
// whose preset has none, an API preset's of the same model, so a Claude or
// ChatGPT plan shows what an API account would have paid.
func price(cat catalog.Catalog, model string) (priced, bool) {
	paid := func(c catalog.Cost) bool { return c.Input+c.Output+c.CacheRead+c.CacheWrite > 0 }
	if p, ok := cat.Presets[model]; ok && paid(p.Cost) {
		return priced{p.Cost, model}, true
	}
	_, name, _ := strings.Cut(model, "/")
	for _, id := range slices.Sorted(maps.Keys(cat.Presets)) {
		if p := cat.Presets[id]; p.Model == name && paid(p.Cost) {
			return priced{p.Cost, id}, true
		}
	}
	return priced{}, false
}

func cost(c priced, u *statefile.Usage) float64 {
	return (float64(u.Input)*c.Input + float64(u.Output)*c.Output + float64(u.CacheRead)*c.CacheRead + float64(u.CacheWrite)*c.CacheWrite) / 1e6
}
