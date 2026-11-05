package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"pai/pkg/server"
)

// New terminal installs ask before downloading a runtime or model. Choosing
// no (or installing unattended without an explicit config) needs no service.
func memorySetup(in io.Reader, out io.Writer) (*server.MemoryConfig, error) {
	r := bufio.NewReader(in)
	_, _ = fmt.Fprintln(out, "PAI can use semantic embeddings to find memories with different wording.")
	_, _ = fmt.Fprintln(out, "Ollama and the embedding model need a download of several GB. They stay inside PAI's data folder; no global Ollama installation is changed.")
	_, _ = fmt.Fprintf(out, "Runtime, models and caches will be stored under %s.\n", server.Dir)
	for {
		_, _ = fmt.Fprint(out, "Download private Ollama and enable local embeddings? [y/N]: ")
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return &server.MemoryConfig{Mode: "semantic", Local: true}, nil
		case "", "n", "no":
			return &server.MemoryConfig{Mode: "hash"}, nil
		default:
			_, _ = fmt.Fprintln(out, "Enter yes or no.")
		}
	}
}
