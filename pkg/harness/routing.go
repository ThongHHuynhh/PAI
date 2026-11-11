package harness

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"pai/pkg/statefile"
)

const (
	maxRouteLevels = 8    // keep policies reviewable and escalation finite
	maxRouteSteps  = 1000 // reject accidentally unbounded policies
	routedAttempts = 2    // one transport retry before considering another approved route
)

// RouteDecision is observable metadata, never inserted in the cached prompt.
type RouteDecision struct {
	Event    string   `json:"event"`
	Model    string   `json:"model"`
	Effort   string   `json:"effort,omitempty"`
	Level    int      `json:"level"`
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence,omitempty"`
}

func (h *Harness) validateRouting(c statefile.Conf) error {
	p := c.Routing
	if p == nil {
		return nil
	}
	if p.Difficulty < 1 || p.Difficulty > 3 || p.Consequence < 1 || p.Consequence > 3 {
		return fmt.Errorf("routing difficulty and consequence must be explicitly set to 1..3")
	}
	if len(p.Levels) == 0 || len(p.Levels) > maxRouteLevels || p.MaxSteps < 1 || p.MaxSteps > maxRouteSteps || p.FailuresPerLevel < 0 || p.StallSteps < 0 {
		return fmt.Errorf("routing needs 1..%d levels, max_steps 1..%d, and nonnegative escalation thresholds", maxRouteLevels, maxRouteSteps)
	}
	if !slices.Contains(p.AllowedModels, c.Model) {
		return fmt.Errorf("session model %q is outside routing allowed_models", c.Model)
	}
	seen := map[string]bool{}
	for _, m := range p.AllowedModels {
		if seen[m] {
			return fmt.Errorf("duplicate allowed model %q", m)
		}
		seen[m] = true
		if _, _, err := h.target(m); err != nil {
			return err
		}
	}
	for _, r := range p.Rules {
		if r.Difficulty < 1 || r.Difficulty > 3 || r.Consequence < 1 || r.Consequence > 3 || r.Level < 0 || r.Level >= len(p.Levels) {
			return fmt.Errorf("invalid routing rule %+v", r)
		}
	}
	check := func(m, effort string) error {
		if !seen[m] {
			return fmt.Errorf("route %q is outside allowed_models; no content sent", m)
		}
		_, err := h.routeConfig(c, m, effort)
		return err
	}
	for _, l := range p.Levels {
		if len(l.Models) == 0 {
			return fmt.Errorf("routing level has no models")
		}
		members := map[string]bool{}
		for _, m := range l.Models {
			if members[m] {
				return fmt.Errorf("duplicate level model %q", m)
			}
			members[m] = true
			if err := check(m, l.Effort); err != nil {
				return err
			}
		}
	}
	if v := p.Verify; v != nil {
		if v.MinConsequence < 1 || v.MinConsequence > 3 {
			return fmt.Errorf("verification min_consequence must be 1..3")
		}
		if err := check(v.Model, v.Effort); err != nil {
			return err
		}
		vp, vm, _ := h.provider(v.Model)
		for _, l := range p.Levels {
			for _, m := range l.Models {
				mp, mm, _ := h.provider(m)
				if vm == mm && vp.Envelope.Name() == mp.Envelope.Name() {
					return fmt.Errorf("verifier must use a distinct model from generation routes")
				}
			}
		}
	}
	return nil
}

// routeConfig applies catalog effort without choosing a different model. Vendor
// fields are merged, preserving unrelated nested settings (e.g. summary).
func (h *Harness) routeConfig(c statefile.Conf, model, effort string) (statefile.Conf, error) {
	var err error
	if model != c.Model {
		c, err = h.moveConfig(c, model)
		if err != nil {
			return c, err
		}
	}
	c.Params = maps.Clone(c.Params)
	if effort == "" {
		return c, nil
	}
	_, preset, err := h.target(model)
	if err != nil {
		return c, err
	}
	for _, setting := range preset.Settings {
		if setting.Name != "effort" {
			continue
		}
		for _, value := range setting.Values {
			if value.Name == effort {
				c.Params = mergeRouteParams(c.Params, value.Params)
				return c, nil
			}
		}
	}
	return c, fmt.Errorf("model %q has no catalog effort %q; use explicit vendor params or a supported setting", model, effort)
}

func mergeRouteParams(dst, src map[string]json.RawMessage) map[string]json.RawMessage {
	dst = maps.Clone(dst)
	if dst == nil {
		dst = map[string]json.RawMessage{}
	}
	for k, v := range src {
		var a, b map[string]json.RawMessage
		if json.Unmarshal(v, &b) == nil && b != nil && json.Unmarshal(dst[k], &a) == nil && a != nil {
			dst[k], _ = json.Marshal(mergeRouteParams(a, b))
		} else {
			dst[k] = slices.Clone(v)
		}
	}
	return dst
}

// routeTurn excludes synthetic retry and job messages: retries cannot reset
// the spending bound. A new external message starts a new task budget.
func routeTurn(chain []statefile.Record) []statefile.Record {
	start := 0
	for i, r := range chain {
		if r.Kind == statefile.Message && r.Via != "retry" && r.Via != "job" {
			start = i
		}
	}
	return chain[start:]
}

func (h *Harness) chooseRoute(c statefile.Conf, chain []statefile.Record) (statefile.Conf, RouteDecision, error) {
	d := RouteDecision{Event: "request", Model: c.Model, Reason: "deliberate session model; routing disabled"}
	if c.Routing == nil {
		return c, d, nil
	}
	if err := h.validateRouting(c); err != nil {
		return c, d, err
	}
	p := c.Routing
	base := 0
	for _, r := range p.Rules {
		if p.Difficulty >= r.Difficulty && p.Consequence >= r.Consequence {
			base = max(base, r.Level)
		}
	}
	requests, steps := 0, 0
	failed := map[string]bool{}
	calls := map[string]bool{}
	var evidence []string
	for _, r := range routeTurn(chain) {
		switch r.Kind {
		case statefile.Route:
			var prev RouteDecision
			if err := json.Unmarshal(r.Body, &prev); err != nil {
				return c, d, fmt.Errorf("invalid route record %s: %w", r.ID, err)
			}
			if prev.Event == "request" {
				requests++
			}
			if prev.Event == "failure" {
				failed[prev.Model] = true
				evidence = append(evidence, r.ID)
			}
		case statefile.Call:
			calls[r.ID] = true
		case statefile.Step:
			steps++
		case statefile.Result:
			if calls[r.For] && r.Exit != nil && *r.Exit != 0 {
				evidence = append(evidence, r.ID)
				delete(calls, r.For)
			}
		}
	}
	if requests >= p.MaxSteps {
		return c, d, fmt.Errorf("routing max_steps %d reached; new user task or explicit policy change required", p.MaxSteps)
	}
	escalation := 0
	if p.FailuresPerLevel > 0 {
		escalation = len(evidence) / p.FailuresPerLevel
	}
	if p.StallSteps > 0 {
		escalation = max(escalation, steps/p.StallSteps)
	}
	level := min(base+escalation, len(p.Levels)-1)
	var skipped []string
	for ; level < len(p.Levels); level++ {
		l := p.Levels[level]
		for _, m := range l.Models {
			name, _, _ := strings.Cut(m, "/")
			switch {
			case failed[m]:
				skipped = append(skipped, m+": failed this turn")
				continue
			case limited(h.account(name), time.Now()):
				skipped = append(skipped, m+": account limited")
				continue
			}
			if why := h.modelReason(c, chain, m); why != "" {
				skipped = append(skipped, m+": "+why)
				continue
			}
			next, err := h.routeConfig(c, m, l.Effort)
			d.Model, d.Level, d.Effort, d.Evidence = m, level, l.Effort, evidence
			d.Reason = fmt.Sprintf("explicit difficulty=%d consequence=%d base=%d; observed failures=%d completed steps=%d; level=%d", p.Difficulty, p.Consequence, base, len(evidence), steps, level)
			if len(skipped) > 0 {
				d.Reason += "; skipped " + strings.Join(skipped, "; ")
			}
			return next, d, err
		}
	}
	return c, d, fmt.Errorf("no approved eligible route: %s", strings.Join(skipped, "; "))
}

func (h *Harness) recordRoute(s *session, step string, d RouteDecision) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = h.append(s, statefile.Record{Header: statefile.Header{Kind: statefile.Route, Type: "application/json", Step: step}, Body: body})
	return err
}
