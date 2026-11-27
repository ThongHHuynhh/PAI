package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
	"pai/pkg/worker"
)

var credentialTool = provider.Tool{Name: "credential", Description: "The hidden credential section of memory: addresses, logins, SSH details, keys and passwords the person wants agents to use. list shows names and descriptions. save stores a value under a name with a description of what it is for. Use a value as {{cred:name}} in run code, write content, edit text or a path: the harness fills it in on the way to the worker and masks it in results, so you never see it. get returns the value itself; call it only when a placeholder cannot work, and say why in reason. forget erases a value, with a reason", Schema: schema(`{"type":"object","properties":{"action":{"type":"string","enum":["list","save","get","forget"]},"name":{"type":"string","description":"lower case, such as jetson-ssh or nas-password"},"value":{"type":"string"},"description":{"type":"string","description":"what it is for, never the secret itself"},"scope":{"type":"string"},"reason":{"type":"string"}},"required":["action"]}`)}

func (h *Harness) credentialCall(ctx context.Context, id string, call statefile.Record, c provider.Call) statefile.Record {
	if h.Memory == nil {
		return result(call.ID, "error: this server has no memory store")
	}
	var a struct{ Action, Name, Value, Description, Scope, Reason string }
	if err := json.Unmarshal(c.Arguments, &a); err != nil {
		return result(call.ID, "error: "+err.Error())
	}
	switch a.Action {
	case "list":
		cs, err := h.Memory.Credentials(ctx)
		if err != nil {
			return result(call.ID, "error: "+err.Error())
		}
		if len(cs) == 0 {
			return result(call.ID, "no credentials stored")
		}
		var b strings.Builder
		for _, c := range cs {
			fmt.Fprintf(&b, "%s  %s  (%s, use %s)\n", c.Name, c.Description, c.Scope, memory.Placeholder(c.Name))
		}
		return result(call.ID, b.String())
	case "save":
		if err := h.Memory.SaveCredential(ctx, memory.Credential{Name: a.Name, Value: a.Value, Description: a.Description, Scope: a.Scope, Source: "credential tool", Session: id}); err != nil {
			return result(call.ID, "error: "+err.Error())
		}
		return result(call.ID, "saved; use "+memory.Placeholder(a.Name))
	case "get":
		if strings.TrimSpace(a.Reason) == "" {
			return result(call.ID, "error: get needs a reason: why "+memory.Placeholder(a.Name)+" cannot work")
		}
		v, err := h.Memory.CredentialValue(ctx, a.Name)
		if err != nil {
			return result(call.ID, "error: "+err.Error())
		}
		return result(call.ID, v)
	case "forget":
		if err := h.Memory.ForgetCredential(ctx, a.Name, a.Reason); err != nil {
			return result(call.ID, "error: "+err.Error())
		}
		return result(call.ID, "forgotten; its value is erased")
	}
	return result(call.ID, fmt.Sprintf("error: unknown action %q: list, save, get or forget", a.Action))
}

// fill replaces each {{cred:name}} in a worker call's text arguments with
// the value, so the model never holds it. An unknown name fails the call.
func (h *Harness) fill(ctx context.Context, a *args) error {
	fields := []*string{&a.Code, &a.Content, &a.Old, &a.New, &a.Input, &a.Path}
	if a.Plan != nil {
		fields = append(fields, &a.Plan.Content, &a.Plan.Path)
	}
	for _, p := range fields {
		if !strings.Contains(*p, "{{cred:") {
			continue
		}
		if h.Memory == nil {
			return fmt.Errorf("this server has no memory store for credentials")
		}
		var err error
		*p = memory.PlaceholderPattern.ReplaceAllStringFunc(*p, func(m string) string {
			name := memory.PlaceholderPattern.FindStringSubmatch(m)[1]
			v, verr := h.Memory.CredentialValue(ctx, name)
			if verr != nil && err == nil {
				err = verr
			}
			return v
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// withheld stands in for text the credential store could not check, so a
// value never reaches a record because masking failed.
const withheld = "[withheld: credentials could not be checked: %v]"

// hide is b masked, or withheld when the store cannot be read. It uses its
// own context: a record must be masked even when its call was cancelled.
func (h *Harness) hide(b []byte) []byte {
	out, err := h.Memory.Mask(context.Background(), b)
	if err != nil {
		return fmt.Appendf(nil, withheld, err)
	}
	return out
}

// unfill puts the placeholders back in a frame before its execution record
// is written: fill sent the values to the worker, and the session file, its
// .log copies on workers and every reader of its records must not hold them.
// Only the fields fill writes or a worker echoes are masked.
func (h *Harness) unfill(f worker.Frame) worker.Frame {
	if h.Memory == nil {
		return f
	}
	f.Body = h.hide(f.Body)
	f.Path = string(h.hide([]byte(f.Path)))
	f.Full = string(h.hide([]byte(f.Full)))
	return f
}

// mask hides credential values in a record before it is written: text and
// JSON bodies (instruction snapshots and structured results carry file
// content), and the path of a result's full output. Media is whole or not
// at all, so it is left.
func (h *Harness) mask(_ context.Context, r statefile.Record) statefile.Record {
	if h.Memory == nil {
		return r
	}
	switch {
	case r.Type == "application/json":
		out, err := h.Memory.Mask(context.Background(), r.Body)
		if err != nil { // still JSON, so a projection such as Instructions skips it and goes on
			out, _ = json.Marshal(map[string]string{"withheld": fmt.Sprintf(withheld, err)})
		}
		r.Body = out
	case r.Type == "" || strings.HasPrefix(r.Type, "text/"):
		r.Body = h.hide(r.Body)
	}
	if r.FullOutput != "" {
		r.FullOutput = string(h.hide([]byte(r.FullOutput)))
	}
	return r
}
