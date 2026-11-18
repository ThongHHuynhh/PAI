// Package release is how a binary becomes another release's. Every file comes
// from the GitHub Releases of Repo and counts only when its hash is in a
// SHA256SUMS the release key signed. A caller names a version, never a place
// or bytes, so whoever picks the version, a server for its workers or a person
// for the server, cannot make root run a binary that was not released. The
// private half of the key is the RELEASE_SIGNING_KEY secret that release.yml
// signs SHA256SUMS with; the public half is built in from the
// RELEASE_PUBLIC_KEY variable. `go run ./pkg/release/keygen` makes a pair.
// A download gets minutes: a binary is megabytes on any link.
package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pai/pkg/version"
)

// Repo, Key and Base are stamped by a release build:
//
//	-ldflags "-X .../pkg/release.Repo=owner/PAI -X .../pkg/release.Key=<base64 ed25519 public key> -X .../pkg/release.Base=https://host/pai"
//
// A build with no key updates nothing. Base, when set, is a download server
// laid out as <Base>/<version>/<file>, with <Base>/latest/ holding the newest
// stable release; empty, releases come from Repo's GitHub releases. Either
// way every file is checked against the signed SHA256SUMS, so Base is trusted
// for availability only.
var (
	Repo = "ThongHHuynhh/PAI"
	Key  = ""
	Base = ""
)

const timeout = 5 * time.Minute

// dl is where release v's files are; "latest" is the newest stable release.
func dl(v string) string {
	if b := strings.TrimRight(Base, "/"); b != "" {
		return b + "/" + v + "/"
	}
	if v == "latest" {
		return "https://github.com/" + Repo + "/releases/latest/download/"
	}
	return "https://github.com/" + Repo + "/releases/download/" + v + "/"
}

// InstallLines run the latest install script with args: on Linux and macOS,
// then on Windows.
func InstallLines(args string) (unix, windows string) {
	return "curl -fsSL " + dl("latest") + "install.sh | sh -s -- " + args, "& ([scriptblock]::Create((irm " + dl("latest") + "install.ps1))) " + args
}

// InstallLinesAt is the install lines for a machine joining the server at url,
// with args: from that server itself, which installs its own release and
// needs nothing else, when this is a signed release reached over https; else
// the latest install script from the place built in, as before.
func InstallLinesAt(url, args string) (unix, windows string) {
	if strings.HasPrefix(url, "https://") && HasKey() && ValidVersion(version.V) {
		return ServerInstallLines(url, args)
	}
	return InstallLines(args)
}

// ServerInstallLines run the install script a PAI server at url serves, which
// installs that server's own release from it, with args.
func ServerInstallLines(url, args string) (unix, windows string) {
	u := strings.TrimRight(url, "/")
	return "curl -fsSL " + u + "/install.sh | sh -s -- " + args, "& ([scriptblock]::Create((irm " + u + "/install.ps1))) " + args
}

// Latest is the newest stable release: each release carries its own version
// in VERSION, and GitHub serves the newest stable one's at latest.
func Latest() (string, error) { return Source{}.Latest(context.Background()) }

// Get is file of release v, checked against v's signed SHA256SUMS. Each of
// from, a release server laid out as <from>/<version>/<file> such as a PAI
// server's /releases, is tried in turn before the place built in; a failure
// there, of the network or of a check, moves on, since every file is checked
// whatever its source.
func Get(v, file string, from ...string) ([]byte, error) {
	var errs []error
	for _, f := range from {
		if f == "" {
			continue
		}
		b, err := Source{URL: f}.Checked(context.Background(), v, file)
		if err == nil {
			return b, nil
		}
		errs = append(errs, fmt.Errorf("from %s: %w", redact(f), err))
	}
	b, err := Source{}.Checked(context.Background(), v, file)
	if err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	return b, nil
}

// Install puts release v's build of cmd for this machine at path, from the
// first of from that serves it or else the place built in (see Get). The new
// binary must say it is v. The old one moves aside: see Aside.
func Install(path, cmd, v string, from ...string) error {
	// Update the executable behind a compatibility link. Replacing the link
	// itself would move an NVMe installation back onto the system drive.
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	} else {
		if st, statErr := os.Lstat(path); statErr == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("installed binary link cannot be resolved: %w", err)
		}
		if !os.IsNotExist(err) {
			return err
		}
	}
	name := cmd + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	b, err := Get(v, name, from...)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".new", b, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(path+".new", 0o755); err != nil {
		return err
	}
	out, err := exec.Command(path+".new", "version").Output()
	if err != nil {
		return fmt.Errorf("release %s: %s does not run: %w", v, name, err)
	}
	if got := string(bytes.TrimSpace(out)); got != v {
		return fmt.Errorf("release %s: %s says it is %s", v, name, got)
	}
	old, err := Aside(path)
	if err != nil {
		return err
	}
	if err := os.Rename(path+".new", path); err != nil {
		return errors.Join(err, os.Rename(old, path))
	}
	return nil
}

// Aside renames path to a name of its own beside it and returns that name, so
// a new binary can take path. Windows renames a running binary but will not
// replace or delete it, and a TUI may run any old binary for days, so each
// gets a new name and the asides from before go only once nothing runs them.
func Aside(path string) (string, error) {
	for _, f := range Asides(path) {
		_ = os.Remove(f) // one still running stays for a later Aside
	}
	old := fmt.Sprintf("%s.old-%d", path, time.Now().UnixNano())
	return old, os.Rename(path, old)
}

// Asides are the old binaries Aside left beside path, and the .old that
// releases before it left. A directory that cannot be read has none: Aside's
// rename and uninstall's removals then fail on it by name.
func Asides(path string) []string {
	dir, base := filepath.Dir(path), filepath.Base(path)
	es, _ := os.ReadDir(dir)
	var m []string
	for _, e := range es {
		if n := e.Name(); n == base+".old" || strings.HasPrefix(n, base+".old-") {
			m = append(m, filepath.Join(dir, n))
		}
	}
	return m
}
