package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"pai/pkg/provider"
	"pai/pkg/statefile"
)

// Pages fit a small model turn. Plans are capped to keep previews reviewable.
const pageBytes = 4096
const planBytes = 4096

type toolEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Offset int    `json:"offset,omitempty"`
	End    int    `json:"end,omitempty"`
}
type toolFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type toolEnvelope struct {
	Summary   string         `json:"summary"`
	Evidence  []toolEvidence `json:"evidence,omitempty"`
	Artifact  string         `json:"artifact,omitempty"`
	Truncated bool           `json:"truncated"`
	Next      *int           `json:"next_offset,omitempty"`
	Text      string         `json:"text,omitempty"`
	Plan      *filePlan      `json:"plan,omitempty"`
	Error     *toolFailure   `json:"error,omitempty"`
}
type filePlan struct {
	Path    string `json:"path"`
	Before  string `json:"before_sha256"`
	After   string `json:"after_sha256"`
	Content string `json:"content"`
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func structured(e toolEnvelope) (statefile.Record, int) {
	b, _ := json.Marshal(e)
	return statefile.Record{Header: statefile.Header{Type: "text/plain"}, Body: b}, 0
}

var readPageTool = provider.Tool{Name: "read_page", Description: "Read a bounded UTF-8 file/artifact page. Use next_offset and expected_sha256 on subsequent pages to detect changes. Byte offsets refer to the original file. No shell execution.", Schema: schema(`{"type":"object","properties":{"worker":{"type":"string"},"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":4096},"expected_sha256":{"type":"string"}},"required":["worker","path"]}`)}
var planEditTool = provider.Tool{Name: "plan_edit", Description: "Plan a single exact replacement in an existing small UTF-8 file without writing. Returns a reviewable plan with before/after SHA256. Pass that plan unchanged to apply_edit.", Schema: schema(`{"type":"object","properties":{"worker":{"type":"string"},"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["worker","path","old","new"]}`)}
var applyEditTool = provider.Tool{Name: "apply_edit", Description: "Apply a reviewed plan after checking the file SHA256, then read back and verify. Detects stale plans and post-write changes; separate Read/Write cannot exclude a concurrent write between precheck and write. Never automatically retry a conflict.", Schema: schema(`{"type":"object","properties":{"worker":{"type":"string"},"plan":{"type":"object","properties":{"path":{"type":"string"},"before_sha256":{"type":"string"},"after_sha256":{"type":"string"},"content":{"type":"string"}},"required":["path","before_sha256","after_sha256","content"]}},"required":["worker","plan"]}`)}

func structuredFile(ctx context.Context, w Worker, tool string, a args, observe func(string, []byte)) (statefile.Record, int) {
	fail := func(code, message string) (statefile.Record, int) {
		return structured(toolEnvelope{Summary: "operation did not complete", Error: &toolFailure{code, message}})
	}
	path := a.Path
	if tool == "apply_edit" {
		if a.Plan == nil {
			return fail("invalid_plan", "plan is required")
		}
		path = a.Plan.Path
	}
	if path == "" {
		return fail("invalid_path", "path is required")
	}
	b, err := w.Read(ctx, path)
	if err != nil {
		return fail("read_failed", err.Error())
	}
	hash := digest(b)
	switch tool {
	case "read_page":
		limit := a.Limit
		if limit == 0 {
			limit = pageBytes
		}
		if a.Offset < 0 || a.Offset > len(b) || limit < 1 || limit > pageBytes {
			return fail("invalid_range", "offset or limit is outside allowed range")
		}
		if a.ExpectedSHA256 != "" && a.ExpectedSHA256 != hash {
			return fail("precondition_failed", "artifact changed; restart reading explicitly")
		}
		if !utf8.Valid(b) {
			return fail("unsupported_encoding", "paged reads require UTF-8 text")
		}
		if a.Offset < len(b) && !utf8.RuneStart(b[a.Offset]) {
			return fail("invalid_range", "offset splits a UTF-8 codepoint")
		}
		end := min(a.Offset+limit, len(b))
		for end > a.Offset && end < len(b) && !utf8.RuneStart(b[end]) {
			end--
		}
		if end == a.Offset && end < len(b) {
			return fail("invalid_range", "limit cannot fit the next UTF-8 codepoint")
		}
		e := toolEnvelope{Summary: fmt.Sprintf("bytes %d..%d of %d", a.Offset, end, len(b)), Evidence: []toolEvidence{{path, hash, a.Offset, end}}, Artifact: path, Text: string(b[a.Offset:end]), Truncated: end < len(b)}
		if e.Truncated {
			e.Next = &end
		}
		observe(path, b)
		return structured(e)
	case "plan_edit":
		if len(b) > planBytes || len(a.New) > planBytes || len(a.Old) > planBytes {
			return fail("plan_too_large", "use a smaller file or existing edit tools; no write performed")
		}
		if !utf8.Valid(b) || a.Old == "" || bytes.Count(b, []byte(a.Old)) != 1 {
			return fail("ambiguous_edit", "old must be nonempty and match exactly once in a UTF-8 file")
		}
		next := bytes.Replace(b, []byte(a.Old), []byte(a.New), 1)
		if len(next) > planBytes {
			return fail("plan_too_large", "result exceeds bounded plan size")
		}
		return structured(toolEnvelope{Summary: "planned replacement; no write performed", Evidence: []toolEvidence{{Path: path, SHA256: hash}}, Artifact: path, Plan: &filePlan{path, hash, digest(next), string(next)}})
	case "apply_edit":
		p := a.Plan
		if len(p.Content) > planBytes || p.Before == "" || p.After != digest([]byte(p.Content)) {
			return fail("invalid_plan", "content or plan hash is invalid")
		}
		if hash != p.Before {
			return fail("precondition_failed", "file changed since planning; ask/replan explicitly")
		}
		if err := w.Write(ctx, path, []byte(p.Content)); err != nil {
			return fail("write_failed", err.Error())
		}
		actual, err := w.Read(ctx, path)
		if err != nil {
			return fail("verification_failed", "write may have succeeded; "+err.Error())
		}
		if digest(actual) != p.After {
			return fail("verification_failed", "file differs after write; effect uncertain; do not retry automatically")
		}
		observe(path, actual)
		return structured(toolEnvelope{Summary: "applied and verified", Evidence: []toolEvidence{{Path: path, SHA256: p.After}}, Artifact: path})
	}
	return fail("unknown_tool", tool)
}
