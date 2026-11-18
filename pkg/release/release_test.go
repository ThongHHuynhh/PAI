package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fixtureVersion = "v0.0.0-test"

// The signed fixture is this test executable. Install runs its version verb
// just as it does a release binary, without compiling or fetching another one.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(fixtureVersion)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// With no Base, releases come from GitHub exactly as before; with one, every
// file, the latest pointer included, comes from it.
func TestDownloadBase(t *testing.T) {
	oldRepo, oldBase := Repo, Base
	t.Cleanup(func() { Repo, Base = oldRepo, oldBase })
	Repo = "owner/PAI"
	for _, c := range []struct{ base, v, want string }{
		{"", "v1.2.3", "https://github.com/owner/PAI/releases/download/v1.2.3/"},
		{"", "latest", "https://github.com/owner/PAI/releases/latest/download/"},
		{"https://dl.example.ts.net/pai", "v1.2.3", "https://dl.example.ts.net/pai/v1.2.3/"},
		{"https://dl.example.ts.net/pai/", "latest", "https://dl.example.ts.net/pai/latest/"},
	} {
		Base = c.base
		if got := dl(c.v); got != c.want {
			t.Errorf("Base %q, %s: got %s, want %s", c.base, c.v, got, c.want)
		}
	}
	Base = "https://dl.example.ts.net/pai"
	unix, windows := InstallLines("pai_key_x")
	if !strings.Contains(unix, "https://dl.example.ts.net/pai/latest/install.sh") || !strings.Contains(windows, "https://dl.example.ts.net/pai/latest/install.ps1") {
		t.Errorf("install lines ignore Base: %s | %s", unix, windows)
	}
}

type fixtureTransport map[string][]byte

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	b, ok := f[filepath.Base(r.URL.Path)]
	code := http.StatusOK
	if !ok {
		code = http.StatusNotFound
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
}

func TestSignedUpdatePreservesBinaryLocation(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "nvme")
	if err := os.Mkdir(disk, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(disk, "pai")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "pai")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlink permission unavailable: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	name := "pai-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	sums := []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(body), name))
	oldKey, oldRepo, oldTransport := Key, Repo, http.DefaultTransport
	Key = base64.StdEncoding.EncodeToString(pub)
	Repo = "fixture/fixture"
	http.DefaultTransport = fixtureTransport{"SHA256SUMS": sums, "SHA256SUMS.sig": ed25519.Sign(private, sums), name: body}
	t.Cleanup(func() { Key, Repo, http.DefaultTransport = oldKey, oldRepo, oldTransport })
	if err := Install(alias, "pai", fixtureVersion); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(alias)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("compatibility link was replaced: %v %v", st, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("target was not updated")
	}
	if len(Asides(target)) != 1 || len(Asides(alias)) != 0 {
		t.Fatal("old binary escaped installation directory")
	}
	dangling := filepath.Join(dir, "missing-disk")
	if err := os.Symlink(filepath.Join(dir, "unmounted", "pai"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := Install(dangling, "pai", fixtureVersion); err == nil {
		t.Fatal("unmounted target was replaced on the system drive")
	}
}
