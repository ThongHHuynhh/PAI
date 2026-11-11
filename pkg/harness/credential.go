package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"pai/pkg/memory"
	"pai/pkg/provider"
	"pai/pkg/statefile"
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

// mask hides credential values in a result before it is recorded.
func (h *Harness) mask(ctx context.Context, r statefile.Record) statefile.Record {
	if h.Memory != nil && (r.Type == "" || strings.HasPrefix(r.Type, "text/")) {
		r.Body = h.Memory.Mask(ctx, r.Body)
	}
	return r
}
