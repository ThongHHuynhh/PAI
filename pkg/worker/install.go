package worker

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"pai/pkg/auth"
	"pai/pkg/mesh"
	"pai/pkg/release"
)

// leave logs the service's node out of its harness when uninstall took the
// join token, which it does before it stops the service. The next install of
// this machine has new keys, and with the old node gone it joins under its
// own name, not <name>-2. A stop with the token, as for an update, keeps the
// node.
func leave(m *mesh.Mesh) {
	if _, err := os.Stat(filepath.Join(stateDir, "join")); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	log.Print("uninstall: logging out of the harness")
	if err := m.Logout(); err != nil {
		log.Printf("logout: %v", err)
	}
}

// rejoin drops the mesh keys of a node the harness removed, which it refuses
// for good, or of a node of another harness, which knows none of them, so
// the install joins token's harness as a new node, under its own name.
func rejoin(token string) error {
	dir := filepath.Join(stateDir, "mesh")
	if _, err := os.Stat(filepath.Join(dir, mesh.Removed)); err == nil || !sameHarness(token) {
		return os.RemoveAll(dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// sameHarness is whether the installed join token is for token's harness.
func sameHarness(token string) bool {
	b, err := os.ReadFile(filepath.Join(stateDir, "join"))
	if err != nil {
		return false
	}
	old, _, _, err := auth.Parse(auth.Join, string(b))
	key, _, _, err2 := auth.Parse(auth.Join, token)
	return err == nil && err2 == nil && old == key
}

// place copies this binary to bin, the path the service runs, through a new
// file and renames. The old one moves aside first, since a CLI or file helper
// may run from bin: see release.Aside. Installing from bin itself leaves it.
// The mode is set apart from the write, which the installer's umask would
// narrow, so every user can run the CLI.
func place(bin string) error {
	exe, err := os.Executable()
	if err != nil || exe == bin {
		return err
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(bin+".new", b, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(bin+".new", 0o755); err != nil {
		return err
	}
	old, err := release.Aside(bin)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(bin+".new", bin); err != nil {
		return errors.Join(err, os.Rename(old, bin))
	}
	return nil
}
