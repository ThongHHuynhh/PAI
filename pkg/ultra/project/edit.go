package project

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pai/pkg/ultra/internal/atomicfile"
)

// WriteSkill replaces an installed skill's SKILL.md in its own library: the
// change reaches every project, and every agent that reads that folder. It
// is the person's explicit choice; a project-only change is an override.
// base is the content the edit started from: if the file changed since, it
// refuses rather than overwrite. The new content must still be a valid skill.
func (p *Project) WriteSkill(pk Package, base, content []byte) error {
	if pk.Kind != KindSkill {
		return errors.New("only a skill's SKILL.md is edited in place")
	}
	var lib *Library
	for _, l := range p.Libraries() {
		if l.Name == pk.Library {
			lib = &l
			break
		}
	}
	if lib == nil {
		return fmt.Errorf("%s is not in a library this project uses", pk.Path)
	}
	dir := expandPath(p.Root, lib.Path)
	if rel, err := filepath.Rel(dir, pk.Path); err != nil || strings.HasPrefix(rel, "..") || filepath.Base(pk.Path) != skillFile {
		return fmt.Errorf("%w: %s is not a SKILL.md in library %s", ErrReadOnly, pk.Path, lib.Name)
	}
	cur, err := os.ReadFile(pk.Path)
	if err != nil {
		return err
	}
	if !bytes.Equal(cur, base) {
		return fmt.Errorf("%w: %s was changed since it was opened", ErrConflict, pk.Path)
	}
	if _, ps := parseRaw(KindSkill, pk.Library, pk.Path, content); Blocking(ps) {
		return &InvalidError{ps}
	}
	return atomicfile.Write(pk.Path, content, 0o644)
}
