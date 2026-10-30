// Package tunnel is one front door a PAI server may have: a cloudflared child
// that carries the user's own Cloudflare tunnel, and its URL, to the server's
// loopback port, so the server opens no inbound port.
package tunnel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// version is the cloudflared release every harness runs. A bump is a PR
	// that changes it and every sum in assets.
	version  = "2026.9.3"
	releases = "https://github.com/cloudflare/cloudflared/releases/download/"
	// cloudflared rides out network loss on its own and exits only when it
	// cannot go on. Starting again at once would spin.
	restartWait = 5 * time.Second
)

// assets are the release file for each platform and the sha256 of the
// cloudflared binary in it: GitHub states it for a bare binary, and for a
// .tgz it is the sum of the file in it. Cloudflare ships no Windows arm64
// build; the amd64 one runs there under emulation.
var assets = map[string][2]string{
	"linux/amd64":   {"cloudflared-linux-amd64", "77e26d8d900e0b8469f416239d14b5f296525fdf79fee6f511ef55609e3fbac2"},
	"linux/arm64":   {"cloudflared-linux-arm64", "aaeb2d7d0da3614634c7e03ab13487a1522c2e79165ed2929cfe23d5e95b326d"},
	"darwin/amd64":  {"cloudflared-darwin-amd64.tgz", "ab588b3b4db9cdb4476c30a3db2a72635b1d8327d44741fee6799a0f37b0ec07"},
	"darwin/arm64":  {"cloudflared-darwin-arm64.tgz", "5472c1a01c84bc31b3021056a73b4e5774ddddefc572124ea8fdf6c340639f32"},
	"windows/amd64": {"cloudflared-windows-amd64.exe", "f096265ec2fcbe9bb6e2d64268db167ced3fcbb83d894bdb9e2fcdb26f2ea7e2"},
	"windows/arm64": {"cloudflared-windows-amd64.exe", "f096265ec2fcbe9bb6e2d64268db167ced3fcbb83d894bdb9e2fcdb26f2ea7e2"},
}

// Own runs the user's own tunnel, by the token Cloudflare gave for it, until
// ctx ends, and calls up with url each time cloudflared starts. The user
// routes url to the tunnel and the tunnel to origin in Cloudflare.
func Own(ctx context.Context, dir, url, token, origin string, up func(url string)) error {
	bin, conf, err := prepare(dir)
	if err != nil {
		return err
	}
	for {
		up(url)
		// in the environment, since any local user can read a command line
		if err := pause(ctx, run(ctx, bin, conf, origin, []string{"TUNNEL_TOKEN=" + token})); err != nil {
			return err
		}
	}
}

// prepare fetches cloudflared into dir and writes its config there: bin and
// conf. cloudflared also reads a config at its default paths, where a user's
// own tunnel may route every host elsewhere. Its own file keeps that out.
func prepare(dir string) (bin, conf string, err error) {
	if bin, err = fetch(dir); err != nil {
		return "", "", err
	}
	conf = filepath.Join(dir, "cloudflared.yml")
	return bin, conf, os.WriteFile(conf, []byte("no-autoupdate: true\n"), 0o644)
}

// run is cloudflared at bin carrying a tunnel to origin until it exits or
// ctx ends, with env added and args after run.
func run(ctx context.Context, bin, conf, origin string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, append([]string{"tunnel", "--config", conf, "--loglevel", "warn", "--url", origin, "run"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return fmt.Errorf("cloudflared: %w", cmd.Run())
}

// pause logs why cloudflared stopped and waits restartWait, unless ctx ends
// first; then it is ctx's error.
func pause(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	log.Printf("tunnel: %v; again in %s", err, restartWait)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(restartWait):
		return nil
	}
}

// fetch is the path of this platform's cloudflared in dir, checked against
// its sum at every start, and downloaded when it is missing or differs.
func fetch(dir string) (string, error) {
	a, ok := assets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no cloudflared for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	bin := filepath.Join(dir, "cloudflared-"+version)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if b, err := os.ReadFile(bin); err == nil {
		if sum(b) == a[1] {
			return bin, nil
		}
		log.Printf("tunnel: %s does not match its sha256", bin)
	}
	url := releases + version + "/" + a[0]
	log.Printf("tunnel: downloading %s", url)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(a[0], ".tgz") {
		if b, err = untar(b); err != nil {
			return "", fmt.Errorf("%s: %s: %w", url, resp.Status, err)
		}
	}
	if s := sum(b); s != a[1] {
		return "", fmt.Errorf("%s: %s, cloudflared sha256 %s, not %s", url, resp.Status, s, a[1])
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(bin+".new", b, 0o755); err != nil {
		return "", err
	}
	return bin, os.Rename(bin+".new", bin)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// untar is the cloudflared file in a gzipped tar.
func untar(b []byte) ([]byte, error) {
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	t := tar.NewReader(z)
	for {
		h, err := t.Next()
		if err != nil {
			return nil, fmt.Errorf("no cloudflared in the archive: %w", err)
		}
		if filepath.Base(h.Name) == "cloudflared" {
			return io.ReadAll(t)
		}
	}
}
