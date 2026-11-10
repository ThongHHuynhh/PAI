// Package mcp serves tools over the Model Context Protocol, so a client
// outside PAI, such as Claude Code, reaches what PAI already holds. It has
// the two transports a client needs: Streamable HTTP, on pai-server, with the
// optional SSE stream left out, and stdio, which forwards to that HTTP.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"pai/pkg/version"
)

// MaxBody bounds one message: a memory item is a sentence or a summary, and
// four times the web app's 256 KiB write leaves room for JSON's escapes.
const MaxBody = 1 << 20

// JSON-RPC error codes.
const (
	codeParse    = -32700
	codeInvalid  = -32600
	codeNoMethod = -32601
	codeParams   = -32602
	// codeServer is a failure on the way to pai-server, from the range
	// JSON-RPC leaves to implementations.
	codeServer = -32000
)

// versions are the protocol revisions served, newest first. 2025-03-26 is
// left out because it allows batches, and every message here is one object.
// 2026-07-28 has no initialize, so it is a separate piece of work; a client
// that can also speak the revisions below falls back to them.
var versions = []string{"2025-11-25", "2025-06-18"}

const instructions = `This server is PAI's long-term memory, shared with PAI's own sessions on every machine and model. Call get_context at the start of a task. Call recall when prior knowledge may matter. Call remember for anything lasting, as one self-contained sentence. Call forget or edit_memory when an item is wrong; nothing is deleted.`

// Annotations are hints a client shows or acts on; they promise nothing.
type Annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

// Tool is one tool. Call's text goes to the model, and so does its error,
// which is the tool's failure and not the protocol's.
type Tool struct {
	Name        string                                                          `json:"name"`
	Description string                                                          `json:"description"`
	Schema      json.RawMessage                                                 `json:"inputSchema"`
	Annotations Annotations                                                     `json:"annotations"`
	Call        func(ctx context.Context, args json.RawMessage) (string, error) `json:"-"`
}

// Server answers MCP messages with its tools. It keeps no state between
// messages, so any number of clients share one.
type Server struct{ tools []Tool }

// New is a server of tools.
func New(tools ...Tool) *Server {
	return &Server{tools: append([]Tool{}, tools...)}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

var noID = json.RawMessage("null")

func failure(id json.RawMessage, code int, format string, args ...any) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{code, fmt.Sprintf(format, args...)}}
}

// Handle answers one message, nil for one that takes no reply.
func (s *Server) Handle(ctx context.Context, msg []byte) []byte {
	r := s.handle(ctx, msg)
	if r == nil {
		return nil
	}
	return encode(r)
}

func encode(r *response) []byte {
	b, err := json.Marshal(r)
	if err != nil { // only a tool's own types could do this
		b, _ = json.Marshal(failure(r.ID, codeServer, "%v", err))
	}
	return b
}

func (s *Server) handle(ctx context.Context, msg []byte) *response {
	if !json.Valid(msg) {
		return failure(noID, codeParse, "parse error: not JSON")
	}
	var in struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(msg, &in); err != nil {
		return failure(noID, codeInvalid, "invalid request: a message is one JSON-RPC object, and batches are not served")
	}
	if in.Method == "" {
		if in.ID != nil && (in.Result != nil || in.Error != nil) {
			return nil // a reply to a request this server never sends
		}
		return failure(noID, codeInvalid, "invalid request: no method")
	}
	if in.ID == nil {
		return nil // a notification: none needs acting on, with no state kept
	}
	if !validID(in.ID) {
		return failure(noID, codeInvalid, "invalid request: an id is a string or a number")
	}
	if in.JSONRPC != "2.0" {
		return failure(in.ID, codeInvalid, "invalid request: jsonrpc must be 2.0")
	}
	result, e := s.call(ctx, in.Method, in.Params)
	if e != nil {
		return &response{JSONRPC: "2.0", ID: in.ID, Error: e}
	}
	return &response{JSONRPC: "2.0", ID: in.ID, Result: result}
}

func validID(id json.RawMessage) bool {
	return len(id) > 0 && (id[0] == '"' || id[0] == '-' || id[0] >= '0' && id[0] <= '9')
}

func (s *Server) call(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := decode(params, &p); err != nil {
			return nil, &rpcError{codeParams, "invalid params: " + err.Error()}
		}
		return map[string]any{
			"protocolVersion": negotiate(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "pai-memory", "version": version.V},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := decode(params, &p); err != nil {
			return nil, &rpcError{codeParams, "invalid params: " + err.Error()}
		}
		i := slices.IndexFunc(s.tools, func(t Tool) bool { return t.Name == p.Name })
		if i < 0 {
			return nil, &rpcError{codeParams, "unknown tool: " + p.Name}
		}
		return run(ctx, s.tools[i], p.Arguments), nil
	}
	return nil, &rpcError{codeNoMethod, "method not found: " + method}
}

// decode reads params, which a method without any may leave out.
func decode(params json.RawMessage, into any) error {
	if len(params) == 0 {
		return nil
	}
	return json.Unmarshal(params, into)
}

// negotiate is the revision to speak: the client's when it is served, else
// the newest, which the client may refuse.
func negotiate(asked string) string {
	if slices.Contains(versions, asked) {
		return asked
	}
	return versions[0]
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func run(ctx context.Context, t Tool, args json.RawMessage) callResult {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	text, err := t.Call(ctx, args)
	if err != nil {
		return callResult{Content: []content{{"text", "error: " + err.Error()}}, IsError: true}
	}
	return callResult{Content: []content{{"text", text}}}
}

// ServeHTTP is Streamable HTTP with JSON answers: one message a POST, 202
// when it takes no reply. The caller authenticates.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "origin refused", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST one JSON-RPC message; this server has no event stream or sessions", http.StatusMethodNotAllowed)
		return
	}
	// A client that omits the header predates it, or is pai mcp, which is
	// served the same. The refusal's body is plain text: a client trying the
	// newer protocol reads that as its cue to fall back to initialize.
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(versions, v) {
		http.Error(w, "unsupported MCP protocol version "+v+"; this server speaks "+strings.Join(versions, ", "), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var over *http.MaxBytesError
		if errors.As(err, &over) {
			http.Error(w, fmt.Sprintf("a message is at most %d bytes", MaxBody), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply := s.handle(r.Context(), body)
	if reply == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if reply.Error != nil && string(reply.ID) == string(noID) { // no request to answer: the body was not one
		w.WriteHeader(http.StatusBadRequest)
	}
	_, _ = w.Write(encode(reply))
}

// sameOrigin is false for a page of another site, which MCP requires refusing
// against DNS rebinding. Clients that are not browsers send no Origin.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}
