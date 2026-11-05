package localembed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// startupTime allows GPU discovery; warmTime allows first-use CUDA compilation.
const (
	startupTime = 30 * time.Second
	warmTime    = 5 * time.Minute
)

func address(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(u.Port())
	if u.Scheme != "http" || portErr != nil || port < 1 || port > 65535 || (host != "localhost" && (ip == nil || !ip.IsLoopback())) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/v1" {
		return "", fmt.Errorf("managed embeddings require an http loopback URL with port ending in /v1")
	}
	return u.Host, nil
}

// Validate checks an explicitly managed endpoint before any download begins.
func Validate(endpoint string) error { _, err := address(endpoint); return err }

func freeEndpoint() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	a := l.Addr().String()
	_ = l.Close()
	return "http://" + a + "/v1", nil
}

// Start owns a child runtime until stop or ctx cancellation. It refuses an
// occupied port rather than treating someone else's runtime as PAI's child.
func Start(ctx context.Context, dir, endpoint string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	addr, err := address(endpoint)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("private embedding listener %s unavailable: %w", addr, err)
	}
	_ = l.Close()
	exe, err := executable(dir)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{
		"HOME": filepath.Join(dir, "embeddings", "home"), "OLLAMA_MODELS": filepath.Join(dir, "embeddings", "models"),
		"XDG_CACHE_HOME": filepath.Join(dir, "cache", "ollama"), "TMPDIR": filepath.Join(dir, "tmp", "ollama"),
	}
	paths["CUDA_CACHE_PATH"] = filepath.Join(dir, "cache", "ollama", "cuda")
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
	}
	paths["TEMP"], paths["TMP"] = paths["TMPDIR"], paths["TMPDIR"]
	if runtime.GOOS == "windows" {
		paths["USERPROFILE"] = paths["HOME"]
		paths["LOCALAPPDATA"] = paths["XDG_CACHE_HOME"]
		paths["APPDATA"] = paths["HOME"]
	}
	paths["OLLAMA_HOST"] = addr
	paths["OLLAMA_NO_CLOUD"] = "1"
	paths["OLLAMA_KEEP_ALIVE"] = "-1"
	paths["OLLAMA_MAX_LOADED_MODELS"] = "1"
	paths["OLLAMA_NUM_PARALLEL"] = "1"
	paths["OLLAMA_DEBUG_LOG_REQUESTS"] = "false"
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "logs", "embeddings.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "serve")
	cmd.Dir = filepath.Join(dir, "embeddings")
	cmd.Stdout, cmd.Stderr = log, log
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if _, replaced := paths[key]; !replaced {
			cmd.Env = append(cmd.Env, v)
		}
	}
	for k, v := range paths {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	processGroup(cmd)
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			kill(cmd, false)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				kill(cmd, true)
				<-done
			}
			_ = log.Close()
		})
	}
	watch := context.AfterFunc(ctx, stop)
	closeRuntime := func() { watch(); stop() }
	ready, cancel := context.WithTimeout(ctx, startupTime)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			closeRuntime()
			return nil, fmt.Errorf("private embedding runtime exited: %v; see %s", waitErr, log.Name())
		case <-ready.Done():
			closeRuntime()
			return nil, fmt.Errorf("private embedding runtime: %w", ready.Err())
		case <-ticker.C:
			u, _ := url.Parse(endpoint)
			u.Path = "/api/version"
			req, _ := http.NewRequestWithContext(ready, http.MethodGet, u.String(), nil)
			resp, err := (&http.Client{Timeout: time.Second}).Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return closeRuntime, nil
				}
			}
		}
	}
}

func post(ctx context.Context, endpoint, path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("private embeddings: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// Warm verifies the downloaded model before PAI accepts requests.
func Warm(ctx context.Context, endpoint, model string) error {
	ctx, cancel := context.WithTimeout(ctx, warmTime)
	defer cancel()
	resp, err := post(ctx, endpoint, "/api/embed", map[string]any{"model": model, "input": "PAI embedding readiness", "keep_alive": -1})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if len(body.Embeddings) != 1 || len(body.Embeddings[0]) == 0 {
		return fmt.Errorf("private embedding warmup returned no vector")
	}
	return nil
}
