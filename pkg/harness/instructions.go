package harness

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"pai/pkg/statefile"
)

// InstructionFile is a worker observation, not a claim that a file is still
// present or that its instructions are currently in a model's context.
type InstructionFile struct {
	Worker    string    `json:"worker"`
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Content   string    `json:"content"`
	Truncated bool      `json:"truncated,omitempty"`
	Session   string    `json:"session"`
	N         int       `json:"n"`
	Time      time.Time `json:"time"`
}

// instructionKind deliberately recognizes conventions, not arbitrary Markdown
// or shell output. Worker paths can use Windows separators on a Unix server.
func instructionKind(p string) string {
	p = strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	if path.Ext(p) != ".md" {
		return ""
	}
	switch path.Base(p) {
	case "agents.md":
		return "agents"
	case "skill.md":
		return "skill"
	case "instructions.md", "rules.md", "claude.md", "copilot-instructions.md":
		return "instruction"
	}
	for _, part := range strings.Split(path.Dir(p), "/") {
		switch part {
		case "skills", ".skills":
			return "skill"
		case "instructions", ".instructions", "rules", ".rules":
			return "instruction"
		}
	}
	return ""
}

func instructionRecord(worker, p string, content []byte) (statefile.Record, bool) {
	kind := instructionKind(p)
	if kind == "" || worker == "" {
		return statefile.Record{}, false
	}
	kept, cut := clip(content, false)
	body, _ := json.Marshal(InstructionFile{Worker: worker, Path: p, Kind: kind, Status: "observed", Content: string(kept), Truncated: cut > 0})
	return statefile.Record{Header: statefile.Header{Kind: statefile.Instruction, Type: "application/json"}, Body: body}, true
}

// instructionPathKey is independent of the server OS: workers may use Windows
// paths on a Unix server. Only drive paths are case folded; Unix paths retain case.
func instructionPathKey(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		p = strings.ToLower(p)
	}
	return path.Clean(p)
}

// Instructions projects immutable observations from sessions. It never contacts
// workers or opens a caller-supplied path. Latest observed content wins per
// worker/path; earlier snapshots remain in their original session records.
func (h *Harness) Instructions() ([]InstructionFile, error) {
	ids, err := h.Sessions.List()
	if err != nil {
		return nil, err
	}
	latest := map[string]InstructionFile{}
	for _, id := range ids {
		s := h.session(id)
		s.mu.Lock()
		recs, err := h.Sessions.Load(id)
		s.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("instructions in session %s: %w", id, err)
		}
		for _, r := range recs {
			if r.Kind != statefile.Instruction {
				continue
			}
			var item InstructionFile
			if err := json.Unmarshal(r.Body, &item); err != nil {
				return nil, fmt.Errorf("instruction record %s/%d: %w", id, r.N, err)
			}
			if item.Worker == "" || instructionKind(item.Path) == "" {
				continue
			}
			item.Session, item.N, item.Time = id, r.N, r.Time
			key := item.Worker + "\x00" + instructionPathKey(item.Path)
			old, exists := latest[key]
			if !exists || item.Time.After(old.Time) || (item.Time.Equal(old.Time) && item.Session == old.Session && item.N > old.N) {
				latest[key] = item
			}
		}
	}
	items := make([]InstructionFile, 0, len(latest))
	for _, item := range latest {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Worker != items[j].Worker {
			return items[i].Worker < items[j].Worker
		}
		return items[i].Path < items[j].Path
	})
	return items, nil
}
