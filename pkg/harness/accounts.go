package harness

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"pai/pkg/catalog"
	"pai/pkg/provider"
	"pai/pkg/statefile"
)

const (
	// Without a reset hint, wait long enough to avoid hammering a limited
	// account, but allow a probe rather than excluding it for the whole day.
	exhaustedBackoff = 30 * time.Minute
	// Leave a little usage for in-flight requests on the same account.
	accountCeiling = 0.97
	// Reserve space for the response and token-count differences across models.
	moveContextFraction = 0.8
	// Providers without telemetry participate, without being assumed empty.
	unknownHeadroom = 0.5
)

var modelCatalog, modelCatalogErr = catalog.Load()
var resetTimestamp = regexp.MustCompile(`\d{4}-\d\d-\d\d[T ][0-9:.]+(?:Z|[+-]\d\d:\d\d)`)
var resetUnix = regexp.MustCompile(`(?i)(?:resets?[_ ]?at|reset)\W+(\d{10})\b`)
var resetClock = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(\d{1,2})(?::(\d\d))?\s*(am|pm)(?:\s*\(([^)]+)\))?`)
var resetDuration = regexp.MustCompile(`(?i)(?:reset|retry).*?\bin\s+(\d+)\s*(seconds?|minutes?|hours?|s|m|h)\b`)

// Balance lists complete models the user considers interchangeable.
type Balance struct {
	Mode string   `json:"mode"`
	Pool []string `json:"pool"`
}

type accountState struct {
	provider.Health
	Until time.Time
}

// Account is the public, read-only view. Unknown utilization is null.
type Account struct {
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	LimitedUntil time.Time `json:"limited_until"`
	UtilFiveHour *float64  `json:"util_five_hour"`
	UtilSevenDay *float64  `json:"util_seven_day"`
	Updated      time.Time `json:"updated"`
}

func (h *Harness) balance() Balance {
	if h.Balance != nil {
		b := h.Balance()
		if b.Mode != "" {
			return b
		}
	}
	return Balance{Mode: "off", Pool: []string{}}
}

func (h *Harness) health(name string, v provider.Health) {
	h.amu.Lock()
	defer h.amu.Unlock()
	if h.accounts == nil {
		h.accounts = map[string]accountState{}
	}
	a := h.accounts[name]
	if v.Updated.IsZero() {
		v.Updated = time.Now().UTC()
	}
	if v.Updated.Before(a.Updated) {
		return
	}
	// Events may report only one window: retain the other window's facts.
	a.Util, a.Resets = maps.Clone(a.Util), maps.Clone(a.Resets)
	if a.Util == nil {
		a.Util = map[string]float64{}
	}
	if a.Resets == nil {
		a.Resets = map[string]time.Time{}
	}
	for k, u := range v.Util {
		a.Util[k] = min(1, max(0, u))
	}
	for k, t := range v.Resets {
		a.Resets[k] = t
	}
	a.Updated = v.Updated
	h.accounts[name] = a
}

func (h *Harness) account(name string) accountState {
	h.amu.Lock()
	defer h.amu.Unlock()
	return h.accounts[name] // maps are immutable after publication
}

func utilization(a accountState, now time.Time) (float64, bool) {
	// After an exhausted backoff expires, stale telemetry must not prevent
	// the next request from probing the account.
	if !a.Until.IsZero() && !now.Before(a.Until) && !a.Updated.After(a.Until) {
		return 0, false
	}
	u, known := 0.0, false
	for k, v := range a.Util {
		if reset := a.Resets[k]; !reset.IsZero() && !now.Before(reset) {
			continue
		}
		u, known = max(u, v), true
	}
	return u, known
}

func headroom(a accountState, now time.Time) float64 {
	if now.Before(a.Until) {
		return -1
	}
	if u, ok := utilization(a, now); ok {
		return 1 - u
	}
	return unknownHeadroom
}

func limited(a accountState, now time.Time) bool {
	u, _ := utilization(a, now)
	return now.Before(a.Until) || u >= accountCeiling
}

func exhaustedUntil(err error, a accountState, now time.Time) time.Time {
	msg := err.Error()
	for _, s := range resetTimestamp.FindAllString(msg, -1) {
		if t, e := time.Parse(time.RFC3339Nano, strings.Replace(s, " ", "T", 1)); e == nil && t.After(now) {
			return t
		}
	}
	if m := resetUnix.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if t := time.Unix(n, 0); t.After(now) {
			return t
		}
	}
	if m := resetDuration.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := time.Second
		switch strings.ToLower(m[2][:1]) {
		case "m":
			unit = time.Minute
		case "h":
			unit = time.Hour
		}
		if n > 0 {
			return now.Add(time.Duration(n) * unit)
		}
	}
	if m := resetClock.FindStringSubmatch(msg); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute, _ := strconv.Atoi(m[2])
		loc := now.Location()
		valid := hour >= 1 && hour <= 12 && minute < 60
		if m[4] != "" {
			var err error
			loc, err = time.LoadLocation(m[4])
			valid = valid && err == nil
		}
		if valid {
			hour %= 12
			if strings.EqualFold(m[3], "pm") {
				hour += 12
			}
			local := now.In(loc)
			t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
			if !t.After(now) {
				t = t.AddDate(0, 0, 1)
			}
			return t
		}
	}
	var reset time.Time
	for k, t := range a.Resets {
		if a.Util[k] >= accountCeiling && t.After(now) && t.After(reset) {
			reset = t
		}
	}
	if reset.IsZero() {
		for _, t := range a.Resets {
			if t.After(now) && (reset.IsZero() || t.Before(reset)) {
				reset = t
			}
		}
	}
	if reset.IsZero() {
		reset = now.Add(exhaustedBackoff)
	}
	return reset
}

func (h *Harness) exhausted(name string, err error) {
	h.amu.Lock()
	defer h.amu.Unlock()
	if h.accounts == nil {
		h.accounts = map[string]accountState{}
	}
	a := h.accounts[name]
	a.Until = exhaustedUntil(err, a, time.Now())
	h.accounts[name] = a
}

// target validates served models. An account alias may borrow its envelope's
// catalog entry; custom providers without a catalog remain usable.
func (h *Harness) target(model string) (provider.Provider, catalog.Preset, error) {
	p, m, err := h.provider(model)
	if err != nil {
		return p, catalog.Preset{}, err
	}
	if modelCatalogErr != nil {
		return p, catalog.Preset{}, modelCatalogErr
	}
	name, _, _ := strings.Cut(model, "/")
	if _, known := modelCatalog.Providers[name]; !known && p.Envelope != nil {
		name = p.Envelope.Name()
	}
	pr, ok := modelCatalog.Presets[name+"/"+m]
	if _, known := modelCatalog.Providers[name]; known && !ok {
		return p, pr, fmt.Errorf("unknown model %q in catalog", model)
	}
	return p, pr, nil
}

// ValidateTarget also accepts a provider alone for a same-model fallback.
func (h *Harness) ValidateTarget(to string) error {
	if !strings.Contains(to, "/") {
		if _, ok := h.providers()[to]; !ok {
			return fmt.Errorf("unknown provider %q", to)
		}
		return nil
	}
	_, _, err := h.target(to)
	return err
}

func (h *Harness) ValidateBalance(b Balance) error {
	if b.Mode != "off" && b.Mode != "auto" {
		return fmt.Errorf("balance mode must be off or auto")
	}
	if (b.Mode == "auto" || len(b.Pool) > 0) && len(b.Pool) < 2 {
		return fmt.Errorf("auto balance needs at least two provider/model entries")
	}
	seen := map[string]bool{}
	for _, m := range b.Pool {
		if seen[m] {
			return fmt.Errorf("duplicate pool member %q", m)
		}
		seen[m] = true
		if _, _, err := h.target(m); err != nil {
			return err
		}
	}
	return nil
}

// eligible is shared by automatic and manual failover. Check the built
// context so compacted-away media does not prevent a later vendor switch.
func (h *Harness) eligible(conf statefile.Conf, chain []statefile.Record, target string) bool {
	p, pr, err := h.target(target)
	if err != nil {
		return false
	}
	if pr.ContextLimit > 0 && statefile.ContextUsed(chain) > int(float64(pr.ContextLimit)*moveContextFraction) {
		return false
	}
	var records []statefile.Record
	if len(chain) > 0 {
		records, _ = statefile.Build(chain)
	}
	for _, r := range records {
		if slices.Contains(conf.Input, r.Type) && (p.Envelope == nil || !p.Envelope.Accepts(r.Type)) {
			return false
		}
	}
	return true
}

// moveConfig is the single config transition for SetModel and failover.
func (h *Harness) moveConfig(c statefile.Conf, to string) (statefile.Conf, error) {
	p, pr, err := h.target(to)
	if err != nil {
		return c, err
	}
	old, _, err := h.provider(c.Model)
	if err != nil {
		return c, err
	}
	c.Model, c.Preset = to, ""
	if pr.Model != "" {
		c.Preset = to
		if _, ok := modelCatalog.Presets[to]; !ok {
			// Account aliases share the catalog preset's settings, while the
			// full model keeps routing to the chosen account.
			_, model, _ := strings.Cut(to, "/")
			c.Preset = pr.Provider + "/" + model
		}
	}
	if pr.ContextLimit > 0 {
		c.ContextLimit = min(c.ContextLimit, pr.ContextLimit)
	}
	c.Input = slices.DeleteFunc(slices.Clone(c.Input), func(t string) bool { return !p.Envelope.Accepts(t) })
	if old.Envelope.Name() != p.Envelope.Name() {
		c.Params = maps.Clone(pr.Params)
	} else {
		c.Params = maps.Clone(c.Params)
	}
	for k := range c.Params {
		if p.Envelope.Owns(k) {
			delete(c.Params, k)
		}
	}
	return c, nil
}

func triedProviders(model string, chain []statefile.Record) map[string]bool {
	name, _, _ := strings.Cut(model, "/")
	tried := map[string]bool{name: true}
	for _, r := range chain {
		if r.Kind == statefile.Message && r.Via != "job" {
			tried = map[string]bool{name: true}
		}
		if r.Kind == statefile.System && r.Source == "failover" {
			from, _, _ := strings.Cut(string(r.Body), " ")
			tried[from] = true
		}
	}
	return tried
}

func (h *Harness) best(conf statefile.Conf, chain []statefile.Record, exclude map[string]bool) string {
	b := h.balance()
	name, _, _ := strings.Cut(conf.Model, "/")
	if b.Mode != "auto" || !slices.ContainsFunc(b.Pool, func(m string) bool { n, _, _ := strings.Cut(m, "/"); return n == name }) {
		return ""
	}
	best, score, now := "", -1.0, time.Now()
	for _, m := range b.Pool {
		n, _, _ := strings.Cut(m, "/")
		if exclude[n] || !h.eligible(conf, chain, m) {
			continue
		}
		if v := headroom(h.account(n), now); v > score || v == score && v >= 0 && m == conf.Model {
			best, score = m, v
		}
	}
	return best
}

// Pick chooses for a new session/delegate, without moving existing sessions.
// Create additionally applies the chosen model's config and media checks.
func (h *Harness) Pick(model string) string {
	if b := h.balance(); b.Mode != "auto" || !slices.Contains(b.Pool, model) {
		return model
	}
	if to := h.best(statefile.Conf{Model: model}, nil, nil); to != "" {
		return to
	}
	return model
}

// Health is what each reporting provider last said of its usage windows.
func (h *Harness) Health() map[string]provider.Health {
	h.amu.Lock()
	defer h.amu.Unlock()
	out := make(map[string]provider.Health, len(h.accounts))
	for name, a := range h.accounts {
		out[name] = a.Health
	}
	return out
}

func (h *Harness) Accounts() []Account {
	names := slices.Sorted(maps.Keys(h.providers()))
	if b := h.balance(); b.Mode == "auto" {
		names = b.Pool
	}
	rows, now := make([]Account, 0, len(names)), time.Now()
	for _, name := range names {
		n, _, _ := strings.Cut(name, "/")
		a := h.account(n)
		r := Account{Name: name, Status: "ok", LimitedUntil: a.Until, Updated: a.Updated}
		if limited(a, now) {
			r.Status = "limited"
			for k, reset := range a.Resets {
				if a.Util[k] >= accountCeiling && reset.After(now) && reset.After(r.LimitedUntil) {
					r.LimitedUntil = reset
				}
			}
		} else {
			r.LimitedUntil = time.Time{}
		}
		if u, ok := a.Util["five_hour"]; ok {
			r.UtilFiveHour = &u
		}
		if u, ok := a.Util["seven_day"]; ok {
			r.UtilSevenDay = &u
		}
		rows = append(rows, r)
	}
	return rows
}
