package server

import (
	"pai/pkg/memory"
	"testing"
)

func TestMemoryConfig(t *testing.T) {
	var defaults *MemoryConfig
	e, err := defaults.embedder()
	if err != nil || e.Name() != "hash-1024" {
		t.Fatalf("default: %v %v", e, err)
	}
	e, err = (&MemoryConfig{Local: true}).embedder()
	if err != nil || e.Name() != "openai:http://127.0.0.1:11435/v1:embeddinggemma" {
		t.Fatalf("private default: %v %v", e, err)
	}
	e, err = (&MemoryConfig{}).embedder()
	if err != nil || e.Name() != "openai:http://localhost:11434/v1:embeddinggemma" {
		t.Fatalf("external default: %v %v", e, err)
	}
	e, err = (&MemoryConfig{Mode: "hash"}).embedder()
	if _, ok := e.(memory.Hash); err != nil || !ok {
		t.Fatalf("hash: %v %v", e, err)
	}
	for _, c := range []MemoryConfig{
		{Mode: "typo"}, {URL: "http://localhost:11434/v1"}, {Model: "test"},
		{URL: "file:///tmp/model", Model: "test"}, {URL: "$PAI_TEST_UNSET", Model: "test"},
	} {
		if _, err := c.embedder(); err == nil {
			t.Fatalf("accepted invalid config %+v", c)
		}
	}
	t.Setenv("PAI_TEST_EMBED_URL", "http://localhost:1234/v1/")
	e, err = (&MemoryConfig{Mode: "hybrid", URL: "$PAI_TEST_EMBED_URL", Model: "model"}).embedder()
	if err != nil || e.Name() != "openai:http://localhost:1234/v1:model" {
		t.Fatalf("expanded: %v %v", e, err)
	}
}
