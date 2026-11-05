package localembed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestArchiveSelectionAndListenerIsolation(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			if _, err := archiveName(goos, arch); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := archiveName("linux", "386"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
	for _, endpoint := range []string{"https://127.0.0.1:11435/v1", "http://0.0.0.0:11435/v1", "http://example.com:11435/v1", "http://127.0.0.1/v1", "http://user@127.0.0.1:11435/v1", "http://127.0.0.1:11435/other"} {
		if Validate(endpoint) == nil {
			t.Fatalf("accepted unsafe private endpoint %s", endpoint)
		}
	}
	if err := Validate("http://127.0.0.1:11435/v1"); err != nil {
		t.Fatal(err)
	}
}

func TestZipAndZstdExtraction(t *testing.T) {
	for _, format := range []string{"zip", "tar.zst"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "runtime."+format)
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if format == "zip" {
				w := zip.NewWriter(f)
				h := &zip.FileHeader{Name: "ollama.exe"}
				h.SetMode(0o755)
				entry, err := w.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = entry.Write([]byte("test"))
				_ = w.Close()
			} else {
				z, err := zstd.NewWriter(f)
				if err != nil {
					t.Fatal(err)
				}
				w := tar.NewWriter(z)
				if err := w.WriteHeader(&tar.Header{Name: "ollama.exe", Mode: 0o755, Typeflag: tar.TypeReg, Size: 4}); err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write([]byte("test"))
				_ = w.Close()
				_ = z.Close()
			}
			_ = f.Close()
			root := filepath.Join(dir, "unpacked")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := extract(path, root); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(root, "ollama.exe"))
			if err != nil || string(b) != "test" {
				t.Fatalf("wrong extracted payload: %s %v", b, err)
			}
		})
	}
}

func TestArchiveRejectsTraversalAndMaterializesLinks(t *testing.T) {
	for _, tc := range []struct {
		name, link string
		bad        bool
	}{{"../escape", "", true}, {"bin/link", "../../escape", true}, {"bin/link", "ollama", false}} {
		t.Run(fmt.Sprint(tc.name, tc.link), func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "runtime.tgz")
			f, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: "bin/ollama", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write([]byte("test"))
			h := &tar.Header{Name: tc.name, Mode: 0o755, Typeflag: tar.TypeReg}
			if tc.link != "" {
				h.Typeflag = tar.TypeSymlink
				h.Linkname = tc.link
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			_ = tw.Close()
			_ = gz.Close()
			_ = f.Close()
			root := filepath.Join(dir, "unpacked")
			_ = os.Mkdir(root, 0o700)
			err = extract(archive, root)
			if (err != nil) != tc.bad {
				t.Fatalf("extract: %v", err)
			}
			if !tc.bad {
				p := filepath.Join(root, "bin/link")
				st, err := os.Lstat(p)
				if err != nil || !st.Mode().IsRegular() {
					t.Fatalf("not a materialized regular file: %v %v", st, err)
				}
				b, _ := os.ReadFile(p)
				if string(b) != "test" {
					t.Fatalf("bad link contents: %s", b)
				}
			}
		})
	}
}

func TestDownloadDigestAndIncompletePull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pull" {
			_, _ = io.WriteString(w, "{\"status\":\"pulling manifest\"}\n")
			return
		}
		_, _ = io.WriteString(w, "archive")
	}))
	defer srv.Close()
	h := sha256.Sum256([]byte("archive"))
	if err := download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "download"), hex.EncodeToString(h[:])); err != nil {
		t.Fatal(err)
	}
	if err := download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "download"), "bad"); err == nil {
		t.Fatal("wrong digest accepted")
	}
	if err := pull(context.Background(), srv.URL+"/v1", "embeddinggemma", io.Discard); err == nil {
		t.Fatal("incomplete model pull accepted")
	}
}

func TestExistingModelIsNotDownloadedAgain(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/show" {
					t.Errorf("unexpected download request %s", r.URL.Path)
				}
				w.WriteHeader(code)
			}))
			defer srv.Close()
			present, err := modelPresent(context.Background(), srv.URL+"/v1", "embeddinggemma")
			if (code == http.StatusOK) != present {
				t.Fatalf("presence=%v for status %d", present, code)
			}
			if (code == http.StatusInternalServerError) != (err != nil) {
				t.Fatalf("error=%v for status %d", err, code)
			}
		})
	}
}
