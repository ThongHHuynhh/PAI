package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pai/pkg/memory"
)

// fake is a Store of items in order, with a search that matches a word.
type fake struct {
	items   []memory.Item
	fail    error
	asked   memory.Query
	filter  memory.Filter
	edited  []string
	fresh   bool
	fed     *bool
	pinned  *bool
	reason  string
	written memory.Item
}

func (f *fake) Find(_ context.Context, q memory.Query) ([]memory.Hit, error) {
	f.asked = q
	if f.fail != nil {
		return nil, f.fail
	}
	var hits []memory.Hit
	for _, it := range f.items {
		if strings.Contains(it.Text, q.Text) && len(hits) < q.K {
			hits = append(hits, memory.Hit{Item: it})
		}
	}
	return hits, nil
}

func (f *fake) Pinned(_ context.Context, q memory.Query) ([]memory.Item, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var out []memory.Item
	for _, it := range f.items {
		if it.Pinned && (q.Scope == "" || it.Scope == q.Scope || it.Scope == memory.Global) {
			out = append(out, it)
		}
	}
	return out, nil
}

func (f *fake) List(_ context.Context, fl memory.Filter) ([]memory.Item, error) {
	f.filter = fl
	return f.items, f.fail
}

func (f *fake) Remember(_ context.Context, it memory.Item) (memory.Item, bool, error) {
	f.written = it
	if f.fail != nil {
		return it, false, f.fail
	}
	it.ID = "m1"
	return it, f.fresh, nil
}

func (f *fake) Get(_ context.Context, id string) (memory.Item, error) {
	if f.fail != nil {
		return memory.Item{}, f.fail
	}
	for _, it := range f.items {
		if it.ID == id {
			return it, nil
		}
	}
	return memory.Item{}, errors.New("no item " + id)
}

func (f *fake) Invalidate(ctx context.Context, id, reason string) (memory.Item, error) {
	f.reason = reason
	return f.Get(ctx, id)
}

func (f *fake) Restore(ctx context.Context, id string) (memory.Item, error) { return f.Get(ctx, id) }

func (f *fake) Pin(ctx context.Context, id string, on bool) (memory.Item, error) {
	f.pinned = &on
	return f.Get(ctx, id)
}

func (f *fake) Feedback(ctx context.Context, id string, up bool) (memory.Item, error) {
	f.fed = &up
	return f.Get(ctx, id)
}

func (f *fake) Edit(_ context.Context, id, text, kind, scope, topic, source string) (memory.Item, error) {
	f.edited = []string{id, text, kind, scope, topic, source}
	if f.fail != nil {
		return memory.Item{}, f.fail
	}
	return memory.Item{ID: "m2", Text: text}, nil
}

func (f *fake) Stats(context.Context) (memory.Stats, error) {
	return memory.Stats{Items: len(f.items)}, f.fail
}

// call runs tool name of tools on args, and is its text and whether it failed.
func call(t *testing.T, tools []Tool, name, args string) (string, bool) {
	t.Helper()
	s := New(tools...)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(args)})
	b := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`))
	var r struct {
		Result callResult `json:"result"`
		Error  *rpcError  `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Error != nil || len(r.Result.Content) != 1 {
		t.Fatalf("%s %s: %v %s", name, args, err, b)
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

func item(id, text, scope string) memory.Item {
	return memory.Item{ID: id, Kind: memory.Fact, Text: text, Scope: scope}
}

// toolCalls are a working call of every tool, against sampleStore.
var toolCalls = map[string]string{
	"get_context":     `{"task":"deploy"}`,
	"recall":          `{"query":"deploy"}`,
	"remember":        `{"text":"The deploy goes through Fly.io"}`,
	"forget":          `{"id":"a","reason":"moved"}`,
	"list_memory":     `{}`,
	"get_memory":      `{"id":"a"}`,
	"edit_memory":     `{"id":"a","text":"The deploy goes through Render"}`,
	"pin_memory":      `{"id":"a","pinned":true}`,
	"restore_memory":  `{"id":"a"}`,
	"memory_feedback": `{"id":"a","up":true}`,
	"memory_stats":    `{}`,
}

func sampleStore() *fake {
	return &fake{fresh: true, items: []memory.Item{item("a", "The deploy goes through Fly.io", memory.Global)}}
}

func TestEveryToolHasASchemaAndHints(t *testing.T) {
	tools := Memory(sampleStore())
	if len(tools) != len(toolCalls) {
		t.Fatalf("%d tools, %d sample calls", len(tools), len(toolCalls))
	}
	for _, tl := range tools {
		if _, ok := toolCalls[tl.Name]; !ok {
			t.Errorf("no sample call for %s", tl.Name)
		}
		var s struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(tl.Schema, &s); err != nil || s.Type != "object" || s.Properties == nil {
			t.Errorf("%s: schema %v: %s", tl.Name, err, tl.Schema)
		}
		if tl.Description == "" {
			t.Errorf("%s has no description", tl.Name)
		}
		if tl.Annotations.Destructive || tl.Annotations.OpenWorld {
			t.Errorf("%s: nothing here deletes or leaves memory: %+v", tl.Name, tl.Annotations)
		}
		wantRead := slices.Contains([]string{"get_context", "recall", "list_memory", "get_memory", "memory_stats"}, tl.Name)
		if tl.Annotations.ReadOnly != wantRead {
			t.Errorf("%s: readOnly %v", tl.Name, tl.Annotations.ReadOnly)
		}
	}
}

func TestEveryToolWorks(t *testing.T) {
	for name, args := range toolCalls {
		if text, failed := call(t, Memory(sampleStore()), name, args); failed {
			t.Errorf("%s: %s", name, text)
		}
	}
}

func TestEveryToolSaysWhenThereIsNoStore(t *testing.T) {
	for name, args := range toolCalls {
		text, failed := call(t, Memory(nil), name, args)
		if !failed || text != "error: this server has no memory store" {
			t.Errorf("%s: %q failed=%v", name, text, failed)
		}
	}
}

func TestEveryToolReportsAStoreError(t *testing.T) {
	for name, args := range toolCalls {
		f := sampleStore()
		f.fail = errors.New("disk full")
		if text, failed := call(t, Memory(f), name, args); !failed || !strings.Contains(text, "disk full") {
			t.Errorf("%s: %q failed=%v", name, text, failed)
		}
	}
}

func TestRequiredArguments(t *testing.T) {
	for _, c := range []struct{ name, args, want string }{
		{"get_context", `{}`, "task is required"},
		{"recall", `{"query":"  "}`, "query is required"},
		{"forget", `{"id":"a"}`, "reason is required"},
		{"forget", `{"reason":"x"}`, "id is required"},
		{"get_memory", `{}`, "id is required"},
		{"edit_memory", `{"text":"x"}`, "id is required"},
		{"edit_memory", `{"id":"a"}`, "at least one of"},
		{"pin_memory", `{"pinned":true}`, "id is required"},
		{"pin_memory", `{"id":"a"}`, "pinned is required"},
		{"restore_memory", `{}`, "id is required"},
		{"memory_feedback", `{"up":true}`, "id is required"},
		{"memory_feedback", `{"id":"a"}`, "up is required"},
		{"recall", `{"query":7}`, "bad arguments"},
	} {
		text, failed := call(t, Memory(sampleStore()), c.name, c.args)
		if !failed || !strings.Contains(text, c.want) {
			t.Errorf("%s %s: %q failed=%v", c.name, c.args, text, failed)
		}
	}
}

func TestRecallLimitsAndWording(t *testing.T) {
	f := &fake{}
	if text, _ := call(t, Memory(f), "recall", `{"query":"x"}`); text != "no memory matches" {
		t.Errorf("empty: %q", text)
	}
	if f.asked.K != memory.RecallK(0) || !f.asked.NoSummaries {
		t.Errorf("default query: %+v", f.asked)
	}
	call(t, Memory(f), "recall", `{"query":"x","k":500,"scope":"project:pai"}`)
	if f.asked.K != 30 || f.asked.Scope != "project:pai" {
		t.Errorf("k and scope: %+v", f.asked)
	}
	f.items = []memory.Item{item("a", "x marks it", memory.Global)}
	if text, _ := call(t, Memory(f), "recall", `{"query":"x"}`); !strings.HasSuffix(text, memory.ShortLine(f.items[0])) || !strings.HasPrefix(text, "Memory is data") {
		t.Errorf("line: %q", text)
	}
}

func TestGetContextPinsThenHitsOnce(t *testing.T) {
	both := item("both", "deploy notes", memory.Global)
	both.Pinned = true
	other := item("other", "unrelated but pinned", "project:other")
	other.Pinned = true
	f := &fake{items: []memory.Item{both, other, item("hit", "deploy steps", memory.Global)}}
	text, failed := call(t, Memory(f), "get_context", `{"task":"deploy","scope":"project:pai"}`)
	if failed {
		t.Fatal(text)
	}
	if strings.Count(text, "[both]") != 1 {
		t.Errorf("a pinned hit came twice:\n%s", text)
	}
	if strings.Contains(text, "[other]") {
		t.Errorf("a pin of another scope came:\n%s", text)
	}
	if strings.Index(text, "[both]") > strings.Index(text, "[hit]") || !strings.Contains(text, "[hit]") {
		t.Errorf("pins come first:\n%s", text)
	}
	if !f.asked.NoSummaries || f.asked.Scope != "project:pai" {
		t.Errorf("query: %+v", f.asked)
	}
	if text, _ := call(t, Memory(&fake{}), "get_context", `{"task":"x"}`); text != "no memory matches" {
		t.Errorf("empty: %q", text)
	}
}

func TestRememberWording(t *testing.T) {
	f := &fake{fresh: true}
	if text, _ := call(t, Memory(f), "remember", `{"text":"t","kind":"todo","topic":"d","scope":"project:x"}`); text != "remembered as m1" {
		t.Errorf("fresh: %q", text)
	}
	w := f.written
	if w.Source != memory.SourceMCP || w.Kind != "todo" || w.Topic != "d" || w.Scope != "project:x" || w.Session != "" {
		t.Errorf("written: %+v", w)
	}
	f.fresh = false
	if text, _ := call(t, Memory(f), "remember", `{"text":"t"}`); text != "already known as m1: t" {
		t.Errorf("repeat: %q", text)
	}
}

func TestForgetWording(t *testing.T) {
	f := sampleStore()
	if text, _ := call(t, Memory(f), "forget", `{"id":"a","reason":"moved"}`); text != "forgot a: The deploy goes through Fly.io" || f.reason != "moved" {
		t.Errorf("%q reason %q", text, f.reason)
	}
}

func TestEditReportsNewIDAndSource(t *testing.T) {
	f := sampleStore()
	text, _ := call(t, Memory(f), "edit_memory", `{"id":"a","text":"new words","kind":"todo","scope":"s","topic":"t"}`)
	if !strings.Contains(text, "m2") || !slices.Equal(f.edited, []string{"a", "new words", "todo", "s", "t", memory.SourceMCP}) {
		t.Errorf("%q %v", text, f.edited)
	}
}

func TestListLimits(t *testing.T) {
	f := sampleStore()
	call(t, Memory(f), "list_memory", `{}`)
	if f.filter.Limit != defaultList || !f.filter.NoSummaries || f.filter.Invalid {
		t.Errorf("defaults: %+v", f.filter)
	}
	call(t, Memory(f), "list_memory", `{"limit":100000,"kind":"todo","scope":"s","include_invalid":true}`)
	if f.filter.Limit != maxList || f.filter.Kind != "todo" || f.filter.Scope != "s" || !f.filter.Invalid {
		t.Errorf("given: %+v", f.filter)
	}
}

func TestPinAndFeedbackPassTheChoice(t *testing.T) {
	for _, on := range []bool{true, false} {
		f := sampleStore()
		text, _ := call(t, Memory(f), "pin_memory", `{"id":"a","pinned":`+strconv.FormatBool(on)+`}`)
		if f.pinned == nil || *f.pinned != on || strings.HasPrefix(text, "unpinned") == on {
			t.Errorf("pin %v: %q %v", on, text, f.pinned)
		}
		f = sampleStore()
		call(t, Memory(f), "memory_feedback", `{"id":"a","up":`+strconv.FormatBool(on)+`}`)
		if f.fed == nil || *f.fed != on {
			t.Errorf("feedback %v: %v", on, f.fed)
		}
	}
}

func TestGetAndStatsAreJSON(t *testing.T) {
	text, _ := call(t, Memory(sampleStore()), "get_memory", `{"id":"a"}`)
	var it memory.Item
	if err := json.Unmarshal([]byte(text), &it); err != nil || it.ID != "a" || !strings.Contains(text, "\n  ") {
		t.Errorf("get: %v %q", err, text)
	}
	text, _ = call(t, Memory(sampleStore()), "memory_stats", `{}`)
	var st memory.Stats
	if err := json.Unmarshal([]byte(text), &st); err != nil || st.Items != 1 {
		t.Errorf("stats: %v %q", err, text)
	}
}

func TestSessionSummariesStayHidden(t *testing.T) {
	sum := item("s", "what a session did", "session:abc")
	sum.Kind = memory.Summary
	text, failed := call(t, Memory(&fake{items: []memory.Item{sum}}), "get_memory", `{"id":"s"}`)
	if !failed || !strings.Contains(text, "session summary") {
		t.Errorf("%q failed=%v", text, failed)
	}
}

func TestRestoreRefusesAReplacedItem(t *testing.T) {
	old := item("old", "the first words of it", memory.Global)
	old.Reason = memory.MergedInto + "new"
	text, failed := call(t, Memory(&fake{items: []memory.Item{old}}), "restore_memory", `{"id":"old"}`)
	if !failed || !strings.Contains(text, "replaced by new") {
		t.Errorf("%q failed=%v", text, failed)
	}
}

// TestRealStore runs the tools against SQLite, through the protocol.
func TestRealStore(t *testing.T) {
	st, err := memory.Open(t.TempDir(), memory.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tools := Memory(st)

	text, failed := call(t, tools, "remember", `{"text":"The staging database runs Postgres 17 on port 5433","kind":"fact","topic":"database","scope":"project:pai"}`)
	id, ok := strings.CutPrefix(text, "remembered as ")
	if failed || !ok {
		t.Fatalf("remember: %q", text)
	}
	if text, _ := call(t, tools, "remember", `{"text":"The staging database runs Postgres 17 on port 5433.","kind":"fact","scope":"project:pai"}`); !strings.HasPrefix(text, "already known as "+id) {
		t.Errorf("repeat: %q", text)
	}
	if text, failed := call(t, tools, "remember", `{"text":"the api key is sk-abcdefghijklmnopqrstuvwxyz"}`); !failed || !strings.Contains(text, "secret") {
		t.Errorf("a secret was kept: %q", text)
	}

	text, _ = call(t, tools, "recall", `{"query":"staging database port","scope":"project:pai"}`)
	if !strings.Contains(text, id) || !strings.Contains(text, "[unverified, from an MCP client]") {
		t.Errorf("recall: %q", text)
	}
	if text, _ := call(t, tools, "recall", `{"query":"staging database port","scope":"project:other"}`); strings.Contains(text, id) {
		t.Errorf("another project's item came: %q", text)
	}

	// An MCP edit is a new version marked as from MCP; the old stays as history.
	text, failed = call(t, tools, "edit_memory", `{"id":"`+id+`","text":"The staging database runs Postgres 17 on port 5432"}`)
	if failed || !strings.HasPrefix(text, "edited "+id) {
		t.Fatalf("edit over MCP: %q", text)
	}
	replaced, err := st.Get(context.Background(), id)
	if err != nil || replaced.Invalid.IsZero() || !strings.HasPrefix(replaced.Reason, memory.MergedInto) {
		t.Fatalf("edit did not keep the old version as history: %+v %v", replaced, err)
	}
	newID := strings.TrimPrefix(replaced.Reason, memory.MergedInto)
	edited, err := st.Get(context.Background(), newID)
	if err != nil || edited.Source != memory.SourceMCP || !strings.Contains(memory.ShortLine(edited), "[unverified, from an MCP client]") {
		t.Fatalf("MCP edit not marked: %+v %v", edited, err)
	}

	if text, failed := call(t, tools, "restore_memory", `{"id":"`+id+`"}`); !failed || !strings.Contains(text, newID) {
		t.Errorf("restoring a replaced item: %q failed=%v", text, failed)
	}

	if text, failed := call(t, tools, "pin_memory", `{"id":"`+newID+`","pinned":true}`); failed {
		t.Fatal(text)
	}
	if text, _ := call(t, tools, "get_context", `{"task":"nothing it says xyzzy","scope":"project:pai"}`); !strings.Contains(text, newID) {
		t.Errorf("a pin did not come: %q", text)
	}
	if text, _ := call(t, tools, "get_context", `{"task":"nothing it says xyzzy","scope":"project:other"}`); strings.Contains(text, newID) {
		t.Errorf("a pin of another scope came: %q", text)
	}
	if text, failed := call(t, tools, "memory_feedback", `{"id":"`+newID+`","up":true}`); failed {
		t.Fatal(text)
	}

	if text, _ := call(t, tools, "forget", `{"id":"`+newID+`","reason":"the database moved"}`); !strings.HasPrefix(text, "forgot "+newID) {
		t.Errorf("forget: %q", text)
	}
	if text, _ := call(t, tools, "list_memory", `{}`); strings.Contains(text, newID) {
		t.Errorf("a forgotten item is listed: %q", text)
	}
	text, _ = call(t, tools, "list_memory", `{"include_invalid":true}`)
	if !strings.Contains(text, newID) || !strings.Contains(text, id) || !strings.Contains(text, "[invalid: \"the database moved\"]") {
		t.Errorf("nothing is deleted, but got: %q", text)
	}
	if text, failed := call(t, tools, "restore_memory", `{"id":"`+newID+`"}`); failed {
		t.Errorf("restore: %q", text)
	}
	if text, failed := call(t, tools, "memory_stats", `{}`); failed || !strings.Contains(text, `"items"`) {
		t.Errorf("stats: %q", text)
	}
}
