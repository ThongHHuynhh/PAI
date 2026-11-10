package harness

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"pai/pkg/statefile"
	"pai/pkg/worker"
)

func TestInstructionConventions(t *testing.T) {
	for p, want := range map[string]string{
		`C:\work\AGENTS.md`: "agents", "/a/SKILL.md": "skill",
		".github/instructions/review.instructions.md": "instruction",
		"/a/skills/deploy/reference.md":               "skill", "RULES.md": "instruction",
		"CLAUDE.md": "instruction", "README.md": "", ".env": "",
		"/a/skills/key.pem": "", "/a/skills-extra/readme.md": "",
	} {
		if got := instructionKind(p); got != want {
			t.Errorf("%q: got %q want %q", p, got, want)
		}
	}
	r, ok := instructionRecord("remote", "AGENTS.md", []byte(strings.Repeat("x", worker.MaxBytes+1)))
	var item InstructionFile
	if !ok || json.Unmarshal(r.Body, &item) != nil || !item.Truncated || len(item.Content) > worker.MaxBytes {
		t.Fatal("snapshot not bounded", item.Truncated)
	}
}

func TestInstructionsPersistedProjection(t *testing.T) {
	sessions := statefile.Sessions{Dir: t.TempDir()}
	f, err := sessions.Start("12345678", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ worker, content string }{{"offline-worker", "old"}, {"another-worker", "other"}, {"offline-worker", "new"}} {
		r, _ := instructionRecord(v.worker, "AGENTS.md", []byte(v.content))
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Append(system("agents", "discovered /unread/AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(sessions.Path("12345678"))
	// Reconstruct the harness with no worker pool: offline/restart needs only records.
	h := &Harness{Sessions: sessions}
	items, err := h.Instructions()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].Content != "new" || items[1].Worker != "offline-worker" || items[1].N != 5 || items[1].Session != "12345678" {
		t.Fatalf("observations: %+v", items)
	}
	after, _ := os.ReadFile(sessions.Path("12345678"))
	if string(before) != string(after) {
		t.Fatal("monitoring changed session history")
	}
}

func TestInstructionsPathDeduplication(t *testing.T) {
	sessions := statefile.Sessions{Dir: t.TempDir()}
	f, err := sessions.Start("12345678", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"C:/work/AGENTS.md", `c:\work\.\AGENTS.md`, "/work/AGENTS.md", "/Work/AGENTS.md"}
	for _, p := range paths {
		r, _ := instructionRecord("remote", p, []byte(p))
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	h := &Harness{Sessions: sessions}
	items, err := h.Instructions()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("observations: %+v", items)
	}
	for i, want := range []string{paths[3], paths[2], paths[1]} {
		if items[i].Path != want || items[i].Content != want {
			t.Fatalf("observation %d: %+v, want %q", i, items[i], want)
		}
	}
}
