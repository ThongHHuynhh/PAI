package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pai/pkg/harness"
	"pai/pkg/provider"
)

func TestRemoveProviderCleansReferences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Config{
		HTTP:      "[::1]:0",
		Providers: map[string]Provider{"a": {Shape: "anthropic", Key: "k"}, "b": {Shape: "anthropic", Key: "k"}},
		Fallbacks: map[string]string{"a": "b", "b": "a/claude-x"},
		Balance:   harness.Balance{Mode: "auto", Pool: []string{"a/claude-x", "b/claude-x"}},
	}
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	tokens := providerDir(dir, "a")
	_ = os.MkdirAll(tokens, 0o700)
	var served map[string]provider.Provider
	if err := removeProvider(path, "a", func(ps map[string]provider.Provider) { served = ps }); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Providers["a"]; ok || len(served) != 1 {
		t.Fatalf("still configured or served: %v %v", got.Providers, served)
	}
	if len(got.Fallbacks) != 0 || len(got.Balance.Pool) != 1 || got.Balance.Pool[0] != "b/claude-x" {
		t.Fatalf("references left: %v %v", got.Fallbacks, got.Balance.Pool)
	}
	if _, err := os.Stat(tokens); !os.IsNotExist(err) {
		t.Fatal("sign-in tokens left behind")
	}
	if err := removeProvider(path, "a", func(map[string]provider.Provider) {}); err == nil {
		t.Fatal("removed an unknown provider")
	}
}
