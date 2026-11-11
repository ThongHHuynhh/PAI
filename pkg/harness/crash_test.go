package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"pai/pkg/statefile"
	"pai/pkg/worker"
)

func crashChain(t *testing.T, name string, events ...executionEvent) []statefile.Record {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "arguments": map[string]string{"worker": "w", "code": "rm -rf build"}})
	chain := []statefile.Record{{Header: statefile.Header{N: 1, Kind: statefile.Call, ID: "c1"}, Body: body}}
	for i, e := range events {
		b, _ := json.Marshal(e)
		chain = append(chain, statefile.Record{Header: statefile.Header{N: i + 2, Kind: executionKind, For: "c1"}, Body: b})
	}
	return chain
}

// A harness that crashed while a call ran never runs the call again on
// restart: the worker's durable result is used when it arrived, and the
// outcome is reported as uncertain when it did not.
func TestCrashRecoveryNeverReplays(t *testing.T) {
	cases := []struct {
		name   string
		chain  []statefile.Record
		expect string
	}{
		{"dispatched only", crashChain(t, "run", executionEvent{State: "dispatched"}), "outcome uncertain"},
		{"started only", crashChain(t, "run", executionEvent{State: "dispatched"}, executionEvent{State: "started"}), "outcome uncertain"},
		{"result arrived", crashChain(t, "run", executionEvent{State: "completed", Frame: worker.Frame{Header: worker.Header{Kind: worker.Result}, Body: []byte("removed 3 files")}}), "removed 3 files"},
		{"write done", crashChain(t, "write", executionEvent{State: "completed", Frame: worker.Frame{Header: worker.Header{Kind: worker.Done}}}), "recovered durable worker result"},
		{"multi-operation", crashChain(t, "patch", executionEvent{State: "completed", Frame: worker.Frame{Header: worker.Header{Kind: worker.Done}}}), "inspect postconditions"},
	}
	for _, c := range cases {
		r := recoveredExecution(c.chain, "c1")
		if r.Kind != statefile.Result || r.For != "c1" || !strings.Contains(string(r.Body), c.expect) {
			t.Errorf("%s: got %s %q", c.name, r.Kind, r.Body)
		}
	}
}
