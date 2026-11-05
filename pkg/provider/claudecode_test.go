package provider

import (
	"strings"
	"testing"
	"time"

	"pai/pkg/statefile"
)

// A usage limit comes as claude's own assistant notice, then a success
// result that is an error. The notice is no reply; the error fails over.
func TestClaudeLimitIsAnError(t *testing.T) {
	stream := `{"type":"assistant","uuid":"u1","message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your session limit · resets 5:50pm"}]}}
{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 5:50pm"}
`
	_, _, err := claudeStream(strings.NewReader(stream), func(r statefile.Record) { t.Fatalf("notice emitted as a reply: %s", r.Body) })
	if err == nil || !Exhausted(err) {
		t.Fatalf("limit not an exhausted error: %v", err)
	}
}

func TestClaudeRateLimitEvents(t *testing.T) {
	stream := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"five_hour","utilization":0.98,"resetsAt":2000000000,"unifiedWindows":{"five_hour":{"utilization":0.98,"resetsAt":2000000000},"seven_day":{"utilization":0.4,"resetsAt":2000100000}}}}
{"type":"result","stop_reason":"end_turn","usage":{}}
`
	var health []Health
	_, _, err := claudeStream(strings.NewReader(stream), func(statefile.Record) { t.Fatal("health emitted as a session record") }, func(v Health) { health = append(health, v) })
	if err != nil {
		t.Fatal(err)
	}
	if len(health) != 1 || health[0].Util["five_hour"] != .98 || health[0].Util["seven_day"] != .4 || !health[0].Resets["five_hour"].Equal(time.Unix(2000000000, 0)) || health[0].Updated.IsZero() {
		t.Fatalf("health: %+v", health)
	}
}
