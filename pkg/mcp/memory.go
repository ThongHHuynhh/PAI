package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"pai/pkg/memory"
)

// list_memory's limits: a page a model can read, and the most one call can
// put in its context.
const (
	defaultList = 50
	maxList     = 200
)

var errNoStore = errors.New("this server has no memory store")

// Store is what the tools use of memory.
type Store interface {
	Find(ctx context.Context, q memory.Query) ([]memory.Hit, error)
	Pinned(ctx context.Context, q memory.Query) ([]memory.Item, error)
	List(ctx context.Context, f memory.Filter) ([]memory.Item, error)
	Remember(ctx context.Context, it memory.Item) (memory.Item, bool, error)
	Get(ctx context.Context, id string) (memory.Item, error)
	Invalidate(ctx context.Context, id, reason string) (memory.Item, error)
	Restore(ctx context.Context, id string) (memory.Item, error)
	Pin(ctx context.Context, id string, on bool) (memory.Item, error)
	Feedback(ctx context.Context, id string, up bool) (memory.Item, error)
	Edit(ctx context.Context, id, text, kind, scope, topic, source string) (memory.Item, error)
	Stats(ctx context.Context) (memory.Stats, error)
}

var _ Store = (*memory.Store)(nil)

// The hints of each sort of tool. Nothing here deletes, so none is
// destructive, and memory is a closed world.
var (
	reads      = Annotations{ReadOnly: true, Idempotent: true}
	writes     = Annotations{}
	idempotent = Annotations{Idempotent: true}
)

const (
	idProp     = `"id":{"type":"string","description":"a memory item's id: the one in brackets in what recall lists"}`
	kindProp   = `"kind":{"type":"string","enum":` + memory.KindEnum + `}`
	topicProp  = `"topic":{"type":"string","description":"` + memory.TopicDoc + `"}`
	searchProp = `"scope":{"type":"string","description":"only this scope, such as project:pai, and global; none is global only"}`
)

var kProp = `"k":{"type":"integer","description":"` + memory.KDoc + `"}`

func object(required, props string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + required + `]}`)
}

// tool makes a tool of run, which gets its arguments decoded and a store.
func tool[A any](st Store, name, doc string, schema json.RawMessage, hints Annotations, run func(context.Context, Store, A) (string, error)) Tool {
	return Tool{Name: name, Description: doc, Schema: schema, Annotations: hints, Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
		if st == nil {
			return "", errNoStore
		}
		var a A
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		return run(ctx, st, a)
	}}
}

func need(name, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func lines(items []memory.Item) string {
	if len(items) == 0 {
		return "no memory matches"
	}
	var b strings.Builder
	for _, it := range items {
		b.WriteString(memory.Line(it))
	}
	return b.String()
}

func indented(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return string(b), err
}

// Memory is the tools over st, which may be nil: then each says so. Pass a
// nil interface, not a nil *memory.Store inside one.
func Memory(st Store) []Tool {
	type (
		idArgs     struct{ ID string }
		searchArgs struct {
			Query, Task, Scope string
			K                  int
		}
		rememberArgs struct{ Text, Kind, Topic, Scope string }
		forgetArgs   struct{ ID, Reason string }
		listArgs     struct {
			Kind, Scope    string
			IncludeInvalid bool `json:"include_invalid"`
			Limit          int
		}
		editArgs struct{ ID, Text, Kind, Scope, Topic string }
		pinArgs  struct {
			ID     string
			Pinned *bool // a pointer, so leaving it out is an error and not an unpin
		}
		feedbackArgs struct {
			ID string
			Up *bool
		}
		none struct{}
	)
	return []Tool{
		tool(st, "get_context", "What PAI knows for a task: the pinned items and the items most relevant to it. Call it at the start of a task",
			object(`"task"`, `"task":{"type":"string","description":"what you are about to do"},`+searchProp+`,`+kProp), reads,
			func(ctx context.Context, st Store, a searchArgs) (string, error) {
				if err := need("task", a.Task); err != nil {
					return "", err
				}
				q := memory.Query{Text: a.Task, Scope: a.Scope, K: memory.RecallK(a.K), NoSummaries: true}
				items, err := st.Pinned(ctx, q)
				if err != nil {
					return "", err
				}
				hits, err := st.Find(ctx, q)
				if err != nil {
					return "", err
				}
				seen := map[string]bool{}
				for _, it := range items {
					seen[it.ID] = true
				}
				for _, h := range hits {
					if !seen[h.Item.ID] {
						seen[h.Item.ID] = true
						items = append(items, h.Item)
					}
				}
				return lines(items), nil
			}),
		tool(st, "recall", memory.RecallDoc,
			object(`"query"`, `"query":{"type":"string"},`+kProp+`,`+searchProp), reads,
			func(ctx context.Context, st Store, a searchArgs) (string, error) {
				if err := need("query", a.Query); err != nil {
					return "", err
				}
				hits, err := st.Find(ctx, memory.Query{Text: a.Query, Scope: a.Scope, K: memory.RecallK(a.K), NoSummaries: true})
				if err != nil {
					return "", err
				}
				items := make([]memory.Item, len(hits))
				for i, h := range hits {
					items[i] = h.Item
				}
				return lines(items), nil
			}),
		tool(st, "remember", memory.RememberDoc,
			object(`"text"`, `"text":{"type":"string"},`+topicProp+`,`+kindProp+`,"scope":{"type":"string","description":"`+memory.ScopeDoc+`"}`), writes,
			func(ctx context.Context, st Store, a rememberArgs) (string, error) {
				it, fresh, err := st.Remember(ctx, memory.Item{Text: a.Text, Kind: a.Kind, Scope: a.Scope, Topic: a.Topic, Source: memory.SourceMCP})
				if err != nil {
					return "", err
				}
				if !fresh {
					return "already known as " + it.ID + ": " + it.Text, nil
				}
				return "remembered as " + it.ID, nil
			}),
		tool(st, "forget", memory.ForgetDoc,
			object(`"id","reason"`, idProp+`,"reason":{"type":"string"}`), idempotent,
			func(ctx context.Context, st Store, a forgetArgs) (string, error) {
				if err := errors.Join(need("id", a.ID), need("reason", a.Reason)); err != nil {
					return "", err
				}
				it, err := st.Invalidate(ctx, a.ID, a.Reason)
				if err != nil {
					return "", err
				}
				return "forgot " + it.ID + ": " + it.Text, nil
			}),
		tool(st, "list_memory", "List memory items, newest first, without searching. Items no longer true show only with include_invalid",
			object(``, kindProp+`,"scope":{"type":"string","description":"exactly this scope"},"include_invalid":{"type":"boolean"},`+fmt.Sprintf(`"limit":{"type":"integer","description":"how many items, default %d, at most %d"}`, defaultList, maxList)), reads,
			func(ctx context.Context, st Store, a listArgs) (string, error) {
				limit := defaultList
				if a.Limit > 0 {
					limit = min(a.Limit, maxList)
				}
				items, err := st.List(ctx, memory.Filter{Kind: a.Kind, Scope: a.Scope, Invalid: a.IncludeInvalid, Limit: limit, NoSummaries: true})
				if err != nil {
					return "", err
				}
				return lines(items), nil
			}),
		tool(st, "get_memory", "One memory item by id, whole, as JSON: where it came from, its utility, and why it is invalid if it is",
			object(`"id"`, idProp), reads,
			func(ctx context.Context, st Store, a idArgs) (string, error) {
				if err := need("id", a.ID); err != nil {
					return "", err
				}
				it, err := st.Get(ctx, a.ID)
				if err != nil {
					return "", err
				}
				if it.Kind == memory.Summary {
					return "", fmt.Errorf("%s is a session summary, which this server does not show", a.ID)
				}
				return indented(it)
			}),
		tool(st, "edit_memory", "Request a correction. MCP cannot authorize replacing existing knowledge; a person must confirm through PAI. Existing facts stay unchanged on refusal.",
			object(`"id"`, idProp+`,"text":{"type":"string"},`+kindProp+`,"scope":{"type":"string","description":"`+memory.ScopeDoc+`"},`+topicProp), writes,
			func(ctx context.Context, st Store, a editArgs) (string, error) {
				if err := need("id", a.ID); err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Text+a.Kind+a.Scope+a.Topic) == "" {
					return "", errors.New("give at least one of text, kind, scope or topic to change")
				}
				it, err := st.Edit(ctx, a.ID, a.Text, a.Kind, a.Scope, a.Topic, memory.SourceMCP)
				if err != nil {
					return "", err
				}
				return "edited " + a.ID + "; the new version is " + it.ID + ": " + it.Text, nil
			}),
		tool(st, "pin_memory", "Pin a memory item or unpin it. A pinned item is recalled in every session whatever it is about, and only the newest pins are carried, so pin only what every session needs",
			object(`"id","pinned"`, idProp+`,"pinned":{"type":"boolean"}`), idempotent,
			func(ctx context.Context, st Store, a pinArgs) (string, error) {
				if err := need("id", a.ID); err != nil {
					return "", err
				}
				if a.Pinned == nil {
					return "", errors.New("pinned is required")
				}
				it, err := st.Pin(ctx, a.ID, *a.Pinned)
				if err != nil {
					return "", err
				}
				if *a.Pinned {
					return "pinned " + it.ID + ": " + it.Text, nil
				}
				return "unpinned " + it.ID + ": " + it.Text, nil
			}),
		tool(st, "restore_memory", "Make a forgotten memory item valid again. An item an edit or a merge replaced cannot be restored: change the item that replaced it",
			object(`"id"`, idProp), idempotent,
			func(ctx context.Context, st Store, a idArgs) (string, error) {
				if err := need("id", a.ID); err != nil {
					return "", err
				}
				old, err := st.Get(ctx, a.ID)
				if err != nil {
					return "", err
				}
				if into, ok := strings.CutPrefix(old.Reason, memory.MergedInto); ok {
					return "", fmt.Errorf("%s was replaced by %s; restoring it would make both valid, so change %s instead", a.ID, into, into)
				}
				it, err := st.Restore(ctx, a.ID)
				if err != nil {
					return "", err
				}
				return "restored " + it.ID + ": " + it.Text, nil
			}),
		tool(st, "memory_feedback", "Say whether a memory item was useful: up or down. It moves how the item ranks in later searches, by more than one use does",
			object(`"id","up"`, idProp+`,"up":{"type":"boolean","description":"true if it helped, false if it did not"}`), writes,
			func(ctx context.Context, st Store, a feedbackArgs) (string, error) {
				if err := need("id", a.ID); err != nil {
					return "", err
				}
				if a.Up == nil {
					return "", errors.New("up is required")
				}
				it, err := st.Feedback(ctx, a.ID, *a.Up)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("noted for %s; its utility is now %.2f", it.ID, it.Utility), nil
			}),
		tool(st, "memory_stats", "How much PAI remembers: items by kind and scope, recalls, and how often recalled items were used, as JSON",
			object(``, ``), reads,
			func(ctx context.Context, st Store, _ none) (string, error) {
				s, err := st.Stats(ctx)
				if err != nil {
					return "", err
				}
				return indented(s)
			}),
	}
}
