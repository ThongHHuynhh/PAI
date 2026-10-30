// Package identity is how a worker or connector knows its PAI server: by the
// server key its token carries, at the URL the token or login carries. The
// URL is never trusted: the server at the URL proves the key before any
// secret goes there.
//
//	GET <url>/id?nonce=<n>   {url, key, sig,   sig over "id <url> <n>"
//	                          noise}           noise over "noise <coordinator's Noise key>"
package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// A server answers in well under this; one that has not is down, and a
// worker should try again rather than hang.
var client = http.Client{Timeout: 10 * time.Second}

type record struct {
	URL   string `json:"url"`
	Key   string `json:"key"`
	Sig   string `json:"sig"`
	Noise string `json:"noise,omitempty"`
}

// known is each server key's URL, as the tokens and logins of this process
// carry it. A URL is a hint: Find proves the key there.
var known sync.Map

// Remember notes url as where the server with key is.
func Remember(key, url string) {
	if key != "" && url != "" {
		known.Store(key, url)
	}
}

// Key is the server key kept in file, made there the first time. Only its
// owner may read it: whoever has it can move the server's workers.
func Key(file string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		_, k, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		return k, os.WriteFile(file, k.Seed(), 0o600)
	}
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a server key", file)
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// Encode is k's public half as tokens carry it.
func Encode(k ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func sign(k ed25519.PrivateKey, msg string) string {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, []byte(msg)))
}

func decode(key string) (ed25519.PublicKey, error) {
	pub, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%q is not a server key", key)
	}
	return pub, nil
}

func verify(key, msg, sig string) error {
	pub, err := decode(key)
	if err != nil {
		return err
	}
	s, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, []byte(msg), s) {
		return errors.New("the signature does not verify against the server key")
	}
	return nil
}

// idMsg is what a server signs to prove its key.
func idMsg(url, nonce string) string { return "id " + url + " " + nonce }

// noiseMsg is what a server signs to vouch for its coordinator's Noise key.
// It needs no nonce or URL: only the holder of the Noise key's private half
// can finish a Noise handshake, so a replayed signature gains nothing.
func noiseMsg(noise string) string { return "noise " + noise }

var errNotFound = errors.New("404 Not Found")

// Find is where the server with key is: cached, else the URL a token or
// login of this process carried, once the server proves its key there.
func Find(ctx context.Context, key, cached string) (string, error) {
	if _, err := decode(key); err != nil {
		return "", err
	}
	u := cached
	if u == "" {
		v, ok := known.Load(key)
		if !ok {
			return "", errors.New("no URL is known for this PAI server: log in or join again with its URL")
		}
		u = v.(string)
	}
	return u, why(Prove(ctx, key, u))
}

// Discover is the key the server at url says it has. It is a claim only: a
// device code exchange with a wrong key fails.
func Discover(ctx context.Context, url string) (string, error) {
	var r record
	if err := get(ctx, url+"/id?nonce="+rand.Text(), &r); err != nil {
		return "", why(err)
	}
	if _, err := decode(r.Key); err != nil {
		return "", err
	}
	return r.Key, nil
}

// why names what most likely failed at the server URL.
func why(err error) error {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var st Status
	var op *net.OpError
	var ne net.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err // slow, which says nothing of the URL
	case errors.As(err, &dns):
		return fmt.Errorf("this machine cannot resolve the PAI server URL: %w", err)
	case errors.As(err, &cert):
		return fmt.Errorf("the PAI server URL's certificate is not trusted here; it needs one every device trusts, such as Let's Encrypt's: %w", err)
	case errors.As(err, &st) && st == 530:
		return fmt.Errorf("the PAI server's Cloudflare tunnel is not connected: %w", err)
	case errors.As(err, &st) && st >= 500:
		return fmt.Errorf("the proxy or tunnel at the PAI server URL cannot reach the server on its loopback port: %w", err)
	case errors.As(err, &op), errors.As(err, &ne) && ne.Timeout():
		return fmt.Errorf("nothing answers at the PAI server URL; is the server, or its proxy or tunnel, running? %w", err)
	}
	return err
}

// Prove is nil when the server at url holds key. It signs a fresh nonce with
// the URL it knows itself by, which must be url: a host that relays another
// server's answer fails. On loopback the server is on this machine.
func Prove(ctx context.Context, key, url string) error {
	_, err := prove(ctx, key, url)
	return err
}

// ProveNoise is nil when the server at url holds key and vouches for noise
// as its coordinator's Noise key.
func ProveNoise(ctx context.Context, key, url, noise string) error {
	r, err := prove(ctx, key, url)
	if err != nil {
		return err
	}
	if err := verify(key, noiseMsg(noise), r.Noise); err != nil {
		return fmt.Errorf("%s: Noise key %s: %w", url, noise, err)
	}
	return nil
}

func prove(ctx context.Context, key, url string) (record, error) {
	nonce := rand.Text()
	var r record
	if err := get(ctx, url+"/id?nonce="+nonce, &r); err != nil {
		return r, err
	}
	if r.URL != url && !loopback(url) {
		return r, fmt.Errorf("%s says it is %s", url, r.URL)
	}
	if err := verify(key, idMsg(r.URL, nonce), r.Sig); err != nil {
		return r, fmt.Errorf("%s: %w", url, err)
	}
	return r, nil
}

// ID answers a proof for the server with k, now reached at url(), whose
// coordinator has the Noise key noise.
func ID(k ed25519.PrivateKey, url func() string, noise string) http.Handler {
	ns, key := sign(k, noiseMsg(noise)), Encode(k)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := url()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(record{URL: u, Key: key, Sig: sign(k, idMsg(u, r.URL.Query().Get("nonce"))), Noise: ns})
	})
}

func get(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("GET %s: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %w: %s", url, Status(resp.StatusCode), bytes.TrimSpace(b))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Status is an HTTP status a GET got in place of 200.
type Status int

func (s Status) Error() string { return fmt.Sprintf("%d %s", s, http.StatusText(int(s))) }
