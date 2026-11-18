package release

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// A release's metadata from the GitHub API names its assets; a server asks
// once per version, not once per file. Five minutes covers one round of
// workers updating, and a new release is seen soon after.
const assetsFresh = 5 * time.Minute

// What a source may send, so a broken or hostile one cannot exhaust memory
// before a check runs: SHA256SUMS, its signature, VERSION and API replies are
// kilobytes; a binary is tens of megabytes.
const (
	maxSmall = 1 << 20
	maxFile  = 1 << 30
)

// githubAPI is GitHub's REST API; tests point it at a fake.
var githubAPI = "https://api.github.com"

// Source is where a server fetches release files from. Its zero value is the
// place built into this binary: Base, or Repo's public GitHub releases.
// GitHub reads owner/name through the GitHub API, with Token for a private
// repository; URL is a download server laid out as <URL>/<version>/<file>,
// with <URL>/latest/VERSION. Whatever the source, a file counts only when it
// matches a SHA256SUMS signed by Key, so a source is trusted for
// availability, never for content.
type Source struct {
	GitHub string `json:"github,omitempty"`
	Token  string `json:"-"`
	URL    string `json:"url,omitempty"`
}

// String names s for a person: the repository or the address files come
// from, never the token or a password in a URL.
func (s Source) String() string {
	switch {
	case s.GitHub != "" && s.Token != "":
		return "github " + s.GitHub + " (with a token)"
	case s.GitHub != "":
		return "github " + s.GitHub
	case s.URL != "":
		return redact(s.URL)
	}
	return "built in: " + redact(strings.TrimSuffix(dl("v"), "v/"))
}

var (
	versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	fileRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)*$`)
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	// names Windows keeps for devices, which a file name must not be
	deviceRe = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\.|$)`)
)

// ValidVersion is whether v is a release tag such as v0.5.0 or v0.5.0-beta.3:
// what goes into a URL or a cache path, so a request cannot name another.
func ValidVersion(v string) bool { return versionRe.MatchString(v) && semver.IsValid(v) }

// ValidFile is whether name is a plain file name a release can hold: dotted
// parts, none empty, no Windows device name.
func ValidFile(name string) bool { return fileRe.MatchString(name) && !deviceRe.MatchString(name) }

// ValidRepo is whether r is a GitHub owner/name.
func ValidRepo(r string) bool {
	_, n, _ := strings.Cut(r, "/")
	return repoRe.MatchString(r) && n != "." && n != ".."
}

// Older is whether release a comes before release b; false unless both are
// releases.
func Older(a, b string) bool {
	return semver.IsValid(a) && semver.IsValid(b) && semver.Compare(a, b) < 0
}

func (s Source) check(v, file string) error {
	switch {
	case !ValidVersion(v):
		return fmt.Errorf("%q is not a release version", v)
	case !ValidFile(file):
		return fmt.Errorf("%q is not a release file", file)
	case s.GitHub != "" && !ValidRepo(s.GitHub):
		return fmt.Errorf("%q is not a GitHub owner/name", s.GitHub)
	}
	return nil
}

// fetch is file of release v from s, unchecked: Sums and File check it.
func (s Source) fetch(ctx context.Context, v, file string, limit int64) ([]byte, error) {
	if err := s.check(v, file); err != nil {
		return nil, err
	}
	switch {
	case s.GitHub != "":
		return s.asset(ctx, v, file, limit)
	case s.URL != "":
		return get(ctx, strings.TrimRight(s.URL, "/")+"/"+v+"/"+file, nil, limit)
	}
	return get(ctx, dl(v)+file, nil, limit)
}

// Latest is the newest stable release s offers.
func (s Source) Latest(ctx context.Context) (string, error) {
	var b []byte
	var err error
	switch {
	case s.GitHub != "":
		if !ValidRepo(s.GitHub) {
			return "", fmt.Errorf("%q is not a GitHub owner/name", s.GitHub)
		}
		var r struct {
			Tag string `json:"tag_name"`
		}
		if b, err = get(ctx, githubAPI+"/repos/"+s.GitHub+"/releases/latest", s.headers("application/vnd.github+json"), maxSmall); err == nil {
			err = json.Unmarshal(b, &r)
		}
		b = []byte(r.Tag)
	case s.URL != "":
		b, err = get(ctx, strings.TrimRight(s.URL, "/")+"/latest/VERSION", nil, maxSmall)
	default:
		b, err = get(ctx, dl("latest")+"VERSION", nil, maxSmall)
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if !ValidVersion(v) {
		return "", fmt.Errorf("%s names no release as latest", s)
	}
	return v, nil
}

// Sums is release v's SHA256SUMS and its signature from s, once the
// signature is checked against Key.
func (s Source) Sums(ctx context.Context, v string) (sums, sig []byte, err error) {
	k, err := key()
	if err != nil {
		return nil, nil, err
	}
	if sums, err = s.fetch(ctx, v, "SHA256SUMS", maxSmall); err != nil {
		return nil, nil, err
	}
	if sig, err = s.fetch(ctx, v, "SHA256SUMS.sig", maxSmall); err != nil {
		return nil, nil, err
	}
	if !ed25519.Verify(k, sums, sig) {
		return nil, nil, fmt.Errorf("release %s: SHA256SUMS is not signed by the release key", v)
	}
	return sums, sig, nil
}

// File is file of release v from s, once it matches sums, which Sums checked.
func (s Source) File(ctx context.Context, v, file string, sums []byte) ([]byte, error) {
	want := Sum(sums, file)
	if want == "" {
		return nil, fmt.Errorf("release %s has no %s", v, file)
	}
	b, err := s.fetch(ctx, v, file, maxFile)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(b); hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("release %s: %s does not match SHA256SUMS", v, file)
	}
	return b, nil
}

// Sum is the hash sums gives file, or "" when it names none. A name may carry
// sha256sum's binary-mode '*'.
func Sum(sums []byte, file string) string {
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			want = f[0]
		}
	}
	return want
}

// Checked is file of release v from s: its signed SHA256SUMS, then the file,
// all within one download's time, so a stalling source gives way to the next.
func (s Source) Checked(ctx context.Context, v, file string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sums, _, err := s.Sums(ctx, v)
	if err != nil {
		return nil, err
	}
	return s.File(ctx, v, file, sums)
}

func key() (ed25519.PublicKey, error) {
	k, _ := base64.StdEncoding.DecodeString(Key)
	if len(k) != ed25519.PublicKeySize {
		return nil, errors.New("this build has no release key, so it installs no release: build from source, or install a release")
	}
	return k, nil
}

// HasKey is whether this build can check releases, and so install or serve them.
func HasKey() bool { _, err := key(); return err == nil }

func (s Source) headers(accept string) http.Header {
	h := http.Header{"Accept": {accept}, "X-GitHub-Api-Version": {"2022-11-28"}}
	if s.Token != "" {
		h.Set("Authorization", "Bearer "+s.Token)
	}
	return h
}

type assets struct {
	at  time.Time
	url map[string]string // file name: its API URL
}

var assetCache = struct {
	sync.Mutex
	m map[string]assets
}{m: map[string]assets{}}

// asset downloads file of release v through the GitHub API, which serves a
// private repository's assets to a token that can read it. GitHub answers
// with a redirect to its storage on another host; Go drops the
// Authorization header on such a redirect, so the token stays with GitHub.
// A list without the file, or an asset gone, is fetched once more: the list
// may predate the last upload, or an asset may be uploaded again.
func (s Source) asset(ctx context.Context, v, file string, limit int64) ([]byte, error) {
	k := s.GitHub + "@" + v
	for try := 0; ; try++ {
		a, err := s.assets(ctx, k, v, try > 0)
		if err != nil {
			return nil, err
		}
		u, ok := a.url[file]
		if !ok {
			if try == 0 {
				continue
			}
			return nil, fmt.Errorf("release %s on GitHub %s has no %s", v, s.GitHub, file)
		}
		// the token goes only to this repository's assets in the API
		if !strings.HasPrefix(u, githubAPI+"/repos/"+s.GitHub+"/releases/assets/") {
			return nil, fmt.Errorf("release %s: GitHub names %s at an unexpected address", v, file)
		}
		b, err := get(ctx, u, s.headers("application/octet-stream"), limit)
		if err != nil && try == 0 && strings.Contains(err.Error(), "404") {
			continue
		}
		return b, err
	}
}

func (s Source) assets(ctx context.Context, k, v string, renew bool) (assets, error) {
	assetCache.Lock()
	a, ok := assetCache.m[k]
	assetCache.Unlock()
	if ok && !renew && time.Since(a.at) <= assetsFresh {
		return a, nil
	}
	var r struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	b, err := get(ctx, githubAPI+"/repos/"+s.GitHub+"/releases/tags/"+v, s.headers("application/vnd.github+json"), maxSmall)
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return a, fmt.Errorf("release %s: %w", v, err)
	}
	a = assets{at: time.Now(), url: map[string]string{}}
	for _, x := range r.Assets {
		a.url[x.Name] = x.URL
	}
	assetCache.Lock()
	assetCache.m[k] = a
	assetCache.Unlock()
	return a, nil
}

// get is url's body, with headers, at most limit bytes. A failure names the
// URL, its password hidden, so a wrong address or a missing release reads as
// such.
func get(ctx context.Context, url string, h http.Header, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue *neturl.Error
		if errors.As(err, &ue) {
			ue.URL = redact(ue.URL)
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", redact(url), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", redact(url), limit)
	}
	return b, err
}

// redact is u with any password in it hidden.
func redact(u string) string {
	if p, err := neturl.Parse(u); err == nil {
		return p.Redacted()
	}
	return u
}
