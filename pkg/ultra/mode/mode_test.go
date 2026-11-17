package mode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pai/pkg/statefile"
)

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(homeEnv, home)
	return home
}

func TestSessionsWithoutModeAreClassic(t *testing.T) {
	// A config written before modes existed has no "mode" key.
	old := []byte(`{"model":"anthropic/claude","tools":"","workers":[],"context_limit":200000}`)
	var c statefile.Conf
	if err := json.Unmarshal(old, &c); err != nil {
		t.Fatal(err)
	}
	if Of(c) != Classic {
		t.Fatalf("old session resolved to %q", Of(c))
	}
	for _, s := range []string{"", "classic", "CLASSIC", "bogus"} {
		if Resolve(s) != Classic {
			t.Fatalf("Resolve(%q) is not classic", s)
		}
	}
	if Resolve("ultra") != Ultra {
		t.Fatal("ultra did not resolve")
	}
	// A Classic config still encodes byte for byte as before: no mode key.
	b, _ := json.Marshal(c)
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	if _, ok := back["mode"]; ok {
		t.Fatal("classic config gained a mode key")
	}
}

func TestParseRejectsTypos(t *testing.T) {
	if _, err := Parse("ulta"); err == nil {
		t.Fatal("typo accepted")
	}
	if m, _ := Parse(" Ultra "); m != Ultra {
		t.Fatal("ultra not parsed")
	}
}

func TestDefaultIsClassicUntilChosen(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	m, err := Default(dir)
	if err != nil || m != Classic {
		t.Fatalf("unmigrated project: %v %v", m, err)
	}
	p, _ := Find(dir)
	if err := p.SetDefault(Ultra); err != nil {
		t.Fatal(err)
	}
	if m, _ := Default(dir); m != Ultra {
		t.Fatal("selection did not persist")
	}
	if err := p.SetDefault(Classic); err != nil {
		t.Fatal(err)
	}
	if m, _ := Default(dir); m != Classic {
		t.Fatal("switch back did not persist")
	}
}

func TestBrokenPrefsFallBackToClassic(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	p, _ := Find(dir)
	if err := os.MkdirAll(p.State, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.State, "mode.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Default(dir)
	if m != Classic || err == nil {
		t.Fatalf("broken prefs gave %v, %v; want classic and the error", m, err)
	}
}

func TestLinksKeepBothEnds(t *testing.T) {
	isolate(t)
	p, _ := Find(t.TempDir())
	if err := p.AddLink(Link{From: "classic:abc", To: "ultra:t1", Reason: "continue"}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDefault(Ultra); err != nil {
		t.Fatal(err)
	}
	pr, _ := p.LoadPrefs()
	if len(pr.LinksOf("classic:abc")) != 1 || len(pr.LinksOf("ultra:t1")) != 1 || pr.Default != Ultra {
		t.Fatalf("links or default lost: %+v", pr)
	}
}

func TestProjectsAreIsolated(t *testing.T) {
	isolate(t)
	a, _ := Find(t.TempDir())
	b, _ := Find(t.TempDir())
	if a.ID == b.ID || a.State == b.State {
		t.Fatal("two projects share state")
	}
	_ = a.SetDefault(Ultra)
	if m, _ := Default(b.Root); m != Classic {
		t.Fatal("one project's default leaked into another")
	}
}
