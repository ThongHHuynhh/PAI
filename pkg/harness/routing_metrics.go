package harness

import (
	"encoding/json"

	"pai/pkg/statefile"
)

// RoutingMetrics reports observations, never inferred correctness, prices, or
// confidence. Cache totals are vendor-reported; zero may mean unreported.
type RoutingMetrics struct {
	Requests           int             `json:"requests"`
	FailedRoutes       int             `json:"failed_routes"`
	CompletedSteps     int             `json:"completed_steps"`
	ReportedUsageSteps int             `json:"reported_usage_steps"`
	Attempts           int             `json:"attempts"`
	DurationMS         int64           `json:"duration_ms"`
	Usage              statefile.Usage `json:"usage"`
	Models             map[string]int  `json:"models"`
	Reviews            map[string]int  `json:"reviews"`
}

func MeasureRouting(chain []statefile.Record) (RoutingMetrics, error) {
	m := RoutingMetrics{Models: map[string]int{}, Reviews: map[string]int{}}
	for _, r := range chain {
		switch r.Kind {
		case statefile.Route:
			var d RouteDecision
			if err := json.Unmarshal(r.Body, &d); err != nil {
				return m, err
			}
			if d.Event == "request" {
				m.Requests++
				m.Models[d.Model]++
			}
			if d.Event == "failure" {
				m.FailedRoutes++
			}
		case statefile.Step:
			m.CompletedSteps++
			m.Attempts += r.Attempts
			m.DurationMS += r.DurationMS
			if r.Usage != nil {
				m.ReportedUsageSteps++
				m.Usage.Input += r.Usage.Input
				m.Usage.Output += r.Usage.Output
				m.Usage.CacheRead += r.Usage.CacheRead
				m.Usage.CacheWrite += r.Usage.CacheWrite
			}
		case statefile.Verification:
			var v routeVerification
			if err := json.Unmarshal(r.Body, &v); err != nil {
				return m, err
			}
			m.Reviews[v.Verdict]++
		}
	}
	return m, nil
}
