// Package localembed installs and runs an optional private Ollama runtime.
// Executables, models, caches and logs all live under the PAI data directory.
package localembed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Version and archive hashes pin the optional runtime to official release
// assets. A new upstream version requires reviewing these together.
const Version = "0.40.14"

// installTime allows a multi-gigabyte runtime and the embedding model to
// download on a slow connection. maxExtract bounds a decompression bomb.
const (
	installTime = 30 * time.Minute
	maxExtract  = int64(16 << 30)
)

var archiveHashes = map[string]string{
	"ollama-linux-amd64.tar.zst":          "993e83d08c3d13d17c55009a3e2664c8a25445e644aec3a68bef5abb6abce67d",
	"ollama-linux-arm64.tar.zst":          "39bb333b7b4c0bafcdd7fad35eab1ae9aaf4518ea0f2c731de4376e3a29c94d0",
	"ollama-linux-arm64-jetpack5.tar.zst": "7454281d9a061b5a46d6e3cad21f363ee9f00ebc47ffa82dcfc7f5ecc001e6dd",
	"ollama-linux-arm64-jetpack6.tar.zst": "b9641c00bd111229949f6db18d2d6d345ca39dd132a6d1974f6f38f55e9ccde8",
	"ollama-darwin.tgz":                   "f3bf9a528e908da0da5a2b3a4ad3252a369d62186501dc4880261e8d9023bff5",
	"ollama-windows-amd64.zip":            "65d68cade6cf6375f398b518224f8c8a15e062387f38cf477abfc848de7e7e29",
	"ollama-windows-arm64.zip":            "df8a4e954587ac65ba52ec26b453490a21cd58f80b55f75cf0d706f1506764a8",
}

func archiveName(goos, arch string) (string, error) {
	var name string
	switch goos {
	case "linux":
		name = "ollama-linux-" + arch + ".tar.zst"
	case "darwin":
		name = "ollama-darwin.tgz"
	case "windows":
		name = "ollama-windows-" + arch + ".zip"
	}
	if _, ok := archiveHashes[name]; !ok || (arch != "amd64" && arch != "arm64") {
		return "", fmt.Errorf("local embeddings: unsupported platform %s/%s", goos, arch)
	}
	return name, nil
}

// Install downloads only when a private runtime is absent, then pulls and
// warms the model using a temporary private listener. It never invokes the
// upstream global installer or modifies an existing system Ollama service.
func Install(ctx context.Context, dir, model string, progress io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, installTime)
	defer cancel()
	name, err := archiveName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if _, err := executable(dir); err != nil {
		base := filepath.Join(dir, "runtime")
		if err := os.MkdirAll(base, 0o700); err != nil {
			return err
		}
		stage, err := os.MkdirTemp(base, ".ollama-install-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(stage) }()
		names := []string{name}
		if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
			if b, _ := os.ReadFile("/etc/nv_tegra_release"); strings.Contains(string(b), "R35") {
				names = append(names, "ollama-linux-arm64-jetpack5.tar.zst")
			} else if strings.Contains(string(b), "R36") {
				names = append(names, "ollama-linux-arm64-jetpack6.tar.zst")
			}
		}
		unpack := filepath.Join(stage, "unpacked")
		if err := os.Mkdir(unpack, 0o700); err != nil {
			return err
		}
		for _, name := range names {
			_, _ = fmt.Fprintf(progress, "Downloading private Ollama %s (%s)...\n", Version, name)
			path := filepath.Join(stage, name)
			if err := download(ctx, "https://github.com/ollama/ollama/releases/download/v"+Version+"/"+name, path, archiveHashes[name]); err != nil {
				return err
			}
			if err := extract(path, unpack); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		if _, err := findExecutable(unpack); err != nil {
			return err
		}
		if err := os.Rename(unpack, filepath.Join(base, "ollama")); err != nil {
			return err
		}
	}
	// Installation's listener uses a free port so choosing PAI's private
	// runtime does not borrow or interfere with an existing Ollama instance.
	endpoint, err := freeEndpoint()
	if err != nil {
		return err
	}
	stop, err := Start(ctx, dir, endpoint)
	if err != nil {
		return err
	}
	defer stop()
	present, err := modelPresent(ctx, endpoint, model)
	if err != nil {
		return err
	}
	if !present {
		_, _ = fmt.Fprintf(progress, "Downloading embedding model %s into %s...\n", model, filepath.Join(dir, "embeddings", "models"))
		if err := pull(ctx, endpoint, model, progress); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(progress, "Using existing private embedding model %s.\n", model)
	}
	return Warm(ctx, endpoint, model)
}

// Do not repull an installed tag during reinstall: different weights under
// the same name would leave previously stored vectors in a different space.
func modelPresent(ctx context.Context, endpoint, model string) (bool, error) {
	resp, err := post(ctx, endpoint, "/api/show", map[string]any{"model": model})
	if err == nil {
		_ = resp.Body.Close()
		return true, nil
	}
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func executable(dir string) (string, error) {
	base := filepath.Join(dir, "runtime", "ollama")
	return findExecutable(base)
}

func findExecutable(base string) (string, error) {
	for _, rel := range []string{filepath.Join("bin", "ollama"), "ollama", "ollama.exe"} {
		p := filepath.Join(base, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("local embeddings: runtime missing under %s; run terminal setup to download it", base)
}

func download(ctx context.Context, url, path, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local embeddings download: %s", resp.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("local embeddings: archive checksum mismatch")
	}
	return nil
}

func localPath(root, name string) (string, error) {
	name = filepath.FromSlash(strings.TrimPrefix(name, "./"))
	if name == "." {
		return root, nil
	}
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("local embeddings: unsafe archive path %q", name)
	}
	return filepath.Join(root, name), nil
}

// Links are materialized as regular files after extraction. This also keeps
// server install's existing no-symlinks rule for its credential directory.
type archiveLink struct{ path, target string }

func extract(path, root string) error {
	var links []archiveLink
	var total int64
	write := func(name string, mode os.FileMode, size int64, r io.Reader) error {
		p, err := localPath(root, name)
		if err != nil {
			return err
		}
		if mode.IsDir() {
			return os.MkdirAll(p, 0o755)
		}
		if size < 0 || size > maxExtract-total {
			return fmt.Errorf("local embeddings: archive exceeds extraction limit")
		}
		total += size
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if mode&os.ModeSymlink != 0 {
			b, err := io.ReadAll(io.LimitReader(r, 4096))
			if err != nil {
				return err
			}
			target := filepath.Clean(filepath.Join(filepath.Dir(p), filepath.FromSlash(string(b))))
			rel, err := filepath.Rel(root, target)
			if err != nil || !filepath.IsLocal(rel) {
				return fmt.Errorf("local embeddings: archive link escapes runtime")
			}
			links = append(links, archiveLink{p, target})
			return nil
		}
		if !mode.IsRegular() {
			return fmt.Errorf("local embeddings: unsupported archive entry %q", name)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm()|0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(f, r, size)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	if strings.HasSuffix(path, ".zip") {
		z, err := zip.OpenReader(path)
		if err != nil {
			return err
		}
		defer func() { _ = z.Close() }()
		for _, entry := range z.File {
			r, err := entry.Open()
			if err != nil {
				return err
			}
			err = write(entry.Name, entry.Mode(), int64(entry.UncompressedSize64), r)
			_ = r.Close()
			if err != nil {
				return err
			}
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		var r io.Reader
		if strings.HasSuffix(path, ".zst") {
			z, err := zstd.NewReader(f)
			if err != nil {
				return err
			}
			defer z.Close()
			r = z
		} else {
			z, err := gzip.NewReader(f)
			if err != nil {
				return err
			}
			defer func() { _ = z.Close() }()
			r = z
		}
		t := tar.NewReader(r)
		for {
			h, err := t.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			mode := h.FileInfo().Mode()
			body := io.Reader(t)
			size := h.Size
			if h.Typeflag == tar.TypeSymlink {
				body = strings.NewReader(h.Linkname)
				size = int64(len(h.Linkname))
			}
			if h.Typeflag == tar.TypeLink {
				p, err := localPath(root, h.Name)
				if err != nil {
					return err
				}
				target, err := localPath(root, h.Linkname)
				if err != nil {
					return err
				}
				links = append(links, archiveLink{p, target})
				continue
			}
			if err := write(h.Name, mode, size, body); err != nil {
				return err
			}
		}
	}
	for len(links) > 0 {
		pending := links[:0]
		written := 0
		for _, link := range links {
			f, err := os.Open(link.target)
			if os.IsNotExist(err) {
				pending = append(pending, link)
				continue
			}
			if err != nil {
				return err
			}
			st, err := f.Stat()
			if err != nil {
				_ = f.Close()
				return err
			}
			if !st.Mode().IsRegular() {
				_ = f.Close()
				return fmt.Errorf("local embeddings: archive link target is not a regular file")
			}
			rel, err := filepath.Rel(root, link.path)
			if err == nil {
				err = write(filepath.ToSlash(rel), st.Mode(), st.Size(), f)
			}
			_ = f.Close()
			if err != nil {
				return err
			}
			written++
		}
		if written == 0 {
			return fmt.Errorf("local embeddings: unresolved archive links")
		}
		links = pending
	}
	return nil
}

func pull(ctx context.Context, endpoint, model string, progress io.Writer) error {
	resp, err := post(ctx, endpoint, "/api/pull", map[string]any{"model": model})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	dec := json.NewDecoder(resp.Body)
	last := ""
	for {
		var event struct{ Status, Error string }
		if err := dec.Decode(&event); err == io.EOF {
			if last == "success" {
				return nil
			}
			return fmt.Errorf("local embeddings: model download ended before success")
		} else if err != nil {
			return err
		}
		if event.Error != "" {
			return fmt.Errorf("local embeddings: %s", event.Error)
		}
		if event.Status != last {
			_, _ = fmt.Fprintln(progress, event.Status)
			last = event.Status
		}
	}
}
