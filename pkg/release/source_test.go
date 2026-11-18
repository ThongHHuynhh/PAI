package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// signedRelease is a release's files, SHA256SUMS signed by priv.
func signedRelease(priv ed25519.PrivateKey, files map[string]string) map[string][]byte {
	var sums strings.Builder
	out := map[string][]byte{}
	for name, body := range files {
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256([]byte(body)), name)
		out[name] = []byte(body)
	}
	out["SHA256SUMS"] = []byte(sums.String())
	out["SHA256SUMS.sig"] = ed25519.Sign(priv, out["SHA256SUMS"])
	return out
}

// withKey makes pub the built-in key for the test.
func withKey(t *testing.T) ed25519.PrivateKey {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := Key
	Key = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { Key = old })
	return priv
}

// dirServer serves releases laid out as /<version>/<file>, with /latest/VERSION.
func dirServer(t *testing.T, rel map[string]map[string][]byte, latest string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, f, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if v == "latest" && f == "VERSION" {
			_, _ = w.Write([]byte(latest + "\n"))
			return
		}
		b, ok := rel[v][f]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestURLSourceChecksSignatureAndHash(t *testing.T) {
	priv := withKey(t)
	good := signedRelease(priv, map[string]string{"pai-linux-amd64": "binary"})
	s := dirServer(t, map[string]map[string][]byte{"v1.0.0": good}, "v1.0.0")
	src := Source{URL: s.URL}
	b, err := src.Checked(context.Background(), "v1.0.0", "pai-linux-amd64")
	if err != nil || string(b) != "binary" {
		t.Fatalf("got %q, %v", b, err)
	}
	if v, err := src.Latest(context.Background()); err != nil || v != "v1.0.0" {
		t.Fatalf("latest %q, %v", v, err)
	}
	// a file the sums do not name, and a file that does not match them
	if _, err := src.Checked(context.Background(), "v1.0.0", "other"); err == nil {
		t.Fatal("a file outside SHA256SUMS was accepted")
	}
	good["pai-linux-amd64"] = []byte("tampered")
	if _, err := src.Checked(context.Background(), "v1.0.0", "pai-linux-amd64"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a changed file was accepted: %v", err)
	}
	// sums signed by another key
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	s2 := dirServer(t, map[string]map[string][]byte{"v1.0.0": signedRelease(other, map[string]string{"x": "y"})}, "")
	if _, err := (Source{URL: s2.URL}).Checked(context.Background(), "v1.0.0", "x"); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("a release signed by another key was accepted: %v", err)
	}
}

// A mirror that serves a bad file is passed over for the place built in, and
// a good mirror is used without touching the place built in.
func TestGetTriesMirrorsThenBuiltIn(t *testing.T) {
	priv := withKey(t)
	good := signedRelease(priv, map[string]string{"f": "real"})
	builtIn := dirServer(t, map[string]map[string][]byte{"v2.0.0": good}, "")
	oldBase := Base
	Base = builtIn.URL
	t.Cleanup(func() { Base = oldBase })

	bad := signedRelease(priv, map[string]string{"f": "real"})
	bad["f"] = []byte("evil")
	mirror := dirServer(t, map[string]map[string][]byte{"v2.0.0": bad}, "")
	b, err := Get("v2.0.0", "f", mirror.URL)
	if err != nil || string(b) != "real" {
		t.Fatalf("got %q, %v; want the built-in place's file", b, err)
	}

	hits := 0
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; http.NotFound(w, r) }))
	t.Cleanup(counting.Close)
	Base = counting.URL
	goodMirror := dirServer(t, map[string]map[string][]byte{"v2.0.0": good}, "")
	if b, err := Get("v2.0.0", "f", "", goodMirror.URL); err != nil || string(b) != "real" {
		t.Fatalf("got %q, %v from a good mirror", b, err)
	}
	if hits != 0 {
		t.Fatalf("the place built in was asked %d times though the mirror served", hits)
	}
	// nothing anywhere: both failures are named
	if _, err := Get("v9.9.9", "f", goodMirror.URL); err == nil || !strings.Contains(err.Error(), "from "+goodMirror.URL) {
		t.Fatalf("want the mirror's failure named, got %v", err)
	}
}

// A private repository: the release's assets through the API, the token on
// every API call, and the latest stable tag.
func TestGitHubSourceUsesTokenAndAssets(t *testing.T) {
	priv := withKey(t)
	files := signedRelease(priv, map[string]string{"pai-linux-amd64": "bin"})
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cr3t-9z" {
			http.Error(w, "Not Found", http.StatusNotFound) // GitHub hides a private repo
			return
		}
		switch {
		case r.URL.Path == "/repos/o/r/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v3.0.0"}`))
		case r.URL.Path == "/repos/o/r/releases/tags/v3.0.0":
			var b strings.Builder
			b.WriteString(`{"assets":[`)
			i := 0
			for name := range files {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"name":%q,"url":%q}`, name, api.URL+"/repos/o/r/releases/assets/"+name)
				i++
			}
			b.WriteString(`]}`)
			_, _ = w.Write([]byte(b.String()))
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/releases/assets/"):
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "json", http.StatusUnsupportedMediaType)
				return
			}
			_, _ = w.Write(files[strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/assets/")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	old := githubAPI
	githubAPI = api.URL
	t.Cleanup(func() { githubAPI = old })

	src := Source{GitHub: "o/r", Token: "s3cr3t-9z"}
	if v, err := src.Latest(context.Background()); err != nil || v != "v3.0.0" {
		t.Fatalf("latest %q, %v", v, err)
	}
	if b, err := src.Checked(context.Background(), "v3.0.0", "pai-linux-amd64"); err != nil || string(b) != "bin" {
		t.Fatalf("got %q, %v", b, err)
	}
	assetCache.Lock()
	assetCache.m = map[string]assets{}
	assetCache.Unlock()
	if _, err := (Source{GitHub: "o/r"}).Checked(context.Background(), "v3.0.0", "pai-linux-amd64"); err == nil {
		t.Fatal("a private release was read without a token")
	}
	if strings.Contains(src.String(), "s3cr3t") {
		t.Fatal("String shows the token")
	}
}

func TestValidAndOlder(t *testing.T) {
	for v, ok := range map[string]bool{"v0.5.0": true, "v0.5.0-beta.3": true, "v1.2.3-rc.1": true, "latest": false, "v1.2": false, "../v1.0.0": false, "v1.0.0/../x": false, "1.0.0": false} {
		if ValidVersion(v) != ok {
			t.Errorf("ValidVersion(%q) = %v", v, !ok)
		}
	}
	for f, ok := range map[string]bool{"pai-linux-amd64": true, "SHA256SUMS.sig": true, "install.ps1": true, "../x": false, ".hidden": false, "a/b": false, "a..b": false} {
		if ValidFile(f) != ok {
			t.Errorf("ValidFile(%q) = %v", f, !ok)
		}
	}
	if !Older("v0.5.0-beta.3", "v0.5.0") || !Older("v0.5.0-beta.2", "v0.5.0-beta.10") || Older("v0.5.0", "v0.4.6") || Older("v1.0.0", "v1.0.0") {
		t.Fatal("Older orders releases wrongly")
	}
	if !ValidRepo("Abi-ltd/ABI.PAI") || ValidRepo("a/b/c") || ValidRepo("../x") {
		t.Fatal("ValidRepo")
	}
}

func TestServerInstallLines(t *testing.T) {
	u, w := ServerInstallLines("https://pai.example.ts.net/", "https://pai.example.ts.net CODE-1")
	if u != "curl -fsSL https://pai.example.ts.net/install.sh | sh -s -- https://pai.example.ts.net CODE-1" ||
		w != "& ([scriptblock]::Create((irm https://pai.example.ts.net/install.ps1))) https://pai.example.ts.net CODE-1" {
		t.Fatalf("%s | %s", u, w)
	}
}

// GitHub sends an asset download to its storage on another host: the token
// must not follow. A list that predates an upload is fetched again.
func TestGitHubTokenStaysOnRedirectAndListRenews(t *testing.T) {
	priv := withKey(t)
	files := signedRelease(priv, map[string]string{"bin": "data"})
	var storageAuth []string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = append(storageAuth, r.Header.Get("Authorization"))
		_, _ = w.Write(files[strings.TrimPrefix(r.URL.Path, "/")])
	}))
	t.Cleanup(storage.Close)
	listed := []string{"SHA256SUMS", "SHA256SUMS.sig"} // "bin" not uploaded yet
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/repos/o/r/releases/tags/v1.0.0" {
			var parts []string
			for _, n := range listed {
				parts = append(parts, fmt.Sprintf(`{"name":%q,"url":%q}`, n, api.URL+"/repos/o/r/releases/assets/"+n))
			}
			_, _ = w.Write([]byte(`{"assets":[` + strings.Join(parts, ",") + `]}`))
			listed = append(listed, "bin") // uploaded after the first list
			return
		}
		if n, ok := strings.CutPrefix(r.URL.Path, "/repos/o/r/releases/assets/"); ok {
			http.Redirect(w, r, strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)+"/"+n, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	old := githubAPI
	githubAPI = api.URL
	t.Cleanup(func() { githubAPI = old })
	assetCache.Lock()
	assetCache.m = map[string]assets{}
	assetCache.Unlock()

	b, err := (Source{GitHub: "o/r", Token: "k"}).Checked(context.Background(), "v1.0.0", "bin")
	if err != nil || string(b) != "data" {
		t.Fatalf("got %q, %v", b, err)
	}
	// storage is reached as localhost, the API as 127.0.0.1: another host
	for _, a := range storageAuth {
		if a != "" {
			t.Fatalf("the token reached the storage host: %q", a)
		}
	}
	if len(storageAuth) == 0 {
		t.Fatal("storage was never asked")
	}
}

func TestSourceRefusesBadInputAndLargeBodies(t *testing.T) {
	withKey(t)
	if ValidRepo("o/..") || ValidRepo("o/.") || ValidFile("x.") || ValidFile("CON") || ValidFile("nul.txt") {
		t.Fatal("a dangerous name passed")
	}
	if _, err := (Source{GitHub: "o/.."}).Latest(context.Background()); err == nil {
		t.Fatal("a bad repo was asked")
	}
	if _, err := (Source{URL: "http://127.0.0.1:1"}).Checked(context.Background(), "v1/../x", "f"); err == nil || !strings.Contains(err.Error(), "not a release version") {
		t.Fatalf("a bad version reached the network: %v", err)
	}
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/latest/VERSION") {
			_, _ = w.Write([]byte("../../etc"))
			return
		}
		_, _ = w.Write(make([]byte, maxSmall+10)) // an endless SHA256SUMS, cut short
	}))
	t.Cleanup(junk.Close)
	if _, err := (Source{URL: junk.URL}).Latest(context.Background()); err == nil {
		t.Fatal("junk was taken as the latest release")
	}
	if _, _, err := (Source{URL: junk.URL}).Sums(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized SHA256SUMS was read whole: %v", err)
	}
	if Older("latest", "v1.0.0") {
		t.Fatal("Older ranked a non-version")
	}
	if s := (Source{URL: "https://u:pw@h.example/pai"}).String(); strings.Contains(s, "pw") {
		t.Fatalf("String shows a password: %s", s)
	}
}

// A private repository with betas only has no latest release: it still
// answers, so it can be the source.
func TestReachWithoutLatest(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer t9" && r.URL.Path == "/repos/o/r" {
			_, _ = w.Write([]byte(`{"full_name":"o/r"}`))
			return
		}
		http.NotFound(w, r) // /releases/latest: no stable release
	}))
	t.Cleanup(api.Close)
	old := githubAPI
	githubAPI = api.URL
	t.Cleanup(func() { githubAPI = old })
	if err := (Source{GitHub: "o/r", Token: "t9"}).Reach(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := (Source{GitHub: "o/r", Token: "t9"}).Latest(context.Background()); err == nil {
		t.Fatal("a betas-only repository named a latest")
	}
	if err := (Source{GitHub: "o/r"}).Reach(context.Background()); err == nil {
		t.Fatal("a private repository answered without a token")
	}
}
