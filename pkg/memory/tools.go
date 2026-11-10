package memory

import (
	"fmt"
	"strings"
)

// A model reads memory the same way through PAI's own tools and through MCP,
// so what it is told, how many items it gets and how an item looks are here.
const (
	// defaultRecall is a model's recall when it does not say how much: enough
	// to answer from, little enough to leave the context for the work.
	defaultRecall = 8
	// maxRecall bounds what one call can pull into a model's context.
	maxRecall = 30

	RecallDoc   = "Search PAI's long-term memory: what was learnt in every earlier session, on every machine and model. Returns items with ids"
	RememberDoc = "Save one lasting item to PAI's long-term memory, as one self-contained sentence. Use for facts about the user, their machines and projects, preferences, decisions and why, todos, and procedures that worked"
	ForgetDoc   = "Mark a memory item as no longer true, by its id, with why. It stops being recalled; it is never deleted"
	TopicDoc    = "a short lower case name of the subject, shared by related items, such as deploy or database"
	ScopeDoc    = "global, or project:<name>; default global"

	// KindEnum is the kinds a model may write, as JSON: a summary comes only
	// from a compaction.
	KindEnum = `["` + Fact + `","` + Preference + `","` + Decision + `","` + Todo + `","` + Procedure + `"]`

	// SourceMCP is the source of what a client outside PAI wrote over MCP.
	SourceMCP = "mcp"

	// mcpMark tells a model that an item came from outside PAI: a client can
	// write what any page it read told it to, and this text reaches sessions
	// that run tools on workers.
	mcpMark = "[unverified, from an MCP client] "
)

// KDoc describes a recall's k.
var KDoc = fmt.Sprintf("how many items, default %d", defaultRecall)

// RecallK is the items a recall that asks for k returns.
func RecallK(k int) int {
	if k <= 0 {
		return defaultRecall
	}
	return min(k, maxRecall)
}

// Line is an item as a model reads it. Its kind and scope stay first, where
// the web app reads them from a recall.
func Line(it Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- [%s] (%s, %s) ", it.ID, it.Kind, it.Scope)
	if it.Source == SourceMCP {
		b.WriteString(mcpMark)
	}
	b.WriteString(it.Text)
	if !it.Invalid.IsZero() {
		fmt.Fprintf(&b, " [invalid: %s]", it.Reason)
	}
	b.WriteByte('\n')
	return b.String()
}
