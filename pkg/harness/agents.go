package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"pai/pkg/statefile"
)

// agentsDepth bounds the AGENTS.md listing: root/repos/<repo>/AGENTS.md is
// three levels down, and a full walk of a tree holding a node_modules is not.
const agentsDepth = 3

// agentsTimeout bounds each scan, since Create waits for the slowest one. A
// local listing takes milliseconds; a scan still going after this is stuck on
// a worker, and the record says which.
const agentsTimeout = 30 * time.Second

// killTimeout bounds ending a scan's environments: the worker answers a kill
// at once, and one that does not must not hold Create.
const killTimeout = 5 * time.Second

// agents is the body of the agents system record, written once at the start
// of every file: for every worker's scratch, the root AGENTS.md read and the
// ones below it listed. The calls show verbatim with the worker they ran on,
// so the model knows what ran and how to run more. The scans run at once,
// each under agentsTimeout, and print in a fixed order.
func (h *Harness) agents(ctx context.Context, id string, conf statefile.Conf) string {
	out := make([]string, len(conf.Workers))
	var wg sync.WaitGroup
	for i, w := range conf.Workers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, agentsTimeout)
			defer cancel()
			out[i] = h.scratch(ctx, id, w.Name)
		})
	}
	wg.Wait()
	return "The following read and run calls were made automatically\n" + strings.Join(out, "")
}

// scratch is the section of a worker's scratch, which only the worker knows.
func (h *Harness) scratch(ctx context.Context, id, name string) string {
	r, ok := h.Workers.Get(name)
	if !ok {
		return fmt.Sprintf("\nworker %s, scratch: not connected\n", name)
	}
	return scan(ctx, r, id, fmt.Sprintf("worker %s, scratch %s", name, r.Scratch), r.Scratch, listing(r.Interps[0], r.Scratch))
}

// scan is one section: the read of root/AGENTS.md and the listing code,
// both on r.
func scan(ctx context.Context, r *Remote, id, head, root, code string) string {
	var b strings.Builder
	file := root + "/AGENTS.md"
	fmt.Fprintf(&b, "\n%s\nread %s\n", head, file)
	rec, cut := call(ctx, r, id, "read", args{Path: file})
	b.WriteString(text("read", args{Path: file}, rec, cut))
	interp := r.Interps[0]
	fmt.Fprintf(&b, "run %s: %s\n", interp, code)
	rec, cut = call(ctx, r, id, "run", args{Interpreter: interp, Code: code})
	b.WriteString(text("run", args{}, rec, cut))
	return b.String()
}

// call executes one call of a scan. A call over agentsTimeout says so, and a
// run is ended on the worker, else it would hold the session's environment
// there until runTimeout.
func call(ctx context.Context, r *Remote, id, tool string, a args) (statefile.Record, int) {
	rec, cut := execute(ctx, r, id, tool, a, nil)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		rec.Exit, rec.Body = nil, fmt.Appendf(nil, "error: timed out after %s on worker %s, this scan is incomplete", agentsTimeout, r.Name)
		if tool == "run" {
			kill(ctx, r, id)
		}
	}
	return rec, cut
}

// kill ends the session's environments on r, under killTimeout even when ctx
// is done.
func kill(ctx context.Context, r *Remote, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killTimeout)
	defer cancel()
	if err := r.Kill(ctx, id); err != nil {
		log.Printf("session %s: kill on %s: %v", id, r.Name, err)
	}
}

func text(tool string, a args, rec statefile.Record, cut int) string {
	return string(bytes.TrimRight(finish(tool, a, rec, cut).Body, "\n")) + "\n"
}

// listing prints every AGENTS.md under root to agentsDepth, in the
// interpreter's own language so the model can rerun or deepen it.
func listing(interp, root string) string {
	if interp == "pwsh" {
		return fmt.Sprintf("Get-ChildItem -LiteralPath %s -Recurse -Depth %d -Filter AGENTS.md -Name", quote(interp, root), agentsDepth-1)
	}
	return fmt.Sprintf("find %s -maxdepth %d -name AGENTS.md", quote(interp, root), agentsDepth)
}

// quote quotes s for the interpreter's language, so an apostrophe in a path
// stays in the path.
func quote(interp, s string) string {
	if interp == "pwsh" {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
