package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"pai/pkg/connector"
)

type doerFunc func() (*http.Response, error)

func (f doerFunc) Do(context.Context, string, string, string, io.Reader) (*http.Response, error) {
	return f()
}

func reply(code int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func ok() (*http.Response, error) { return reply(http.StatusOK, `{"ok":true}`) }

var (
	dialFailed = fmt.Errorf("cannot reach: %w", &net.OpError{Op: "dial", Err: errors.New("refused")})
	cutOff     = fmt.Errorf("cannot reach: %w", &net.OpError{Op: "read", Err: errors.New("reset")})
)

func loads(d doer, err error, count *int) func(context.Context) (doer, error) {
	return func(context.Context) (doer, error) {
		*count++
		return d, err
	}
}

func TestPostReadsTheReply(t *testing.T) {
	loaded := 0
	b := &bridge{load: loads(doerFunc(ok), nil, &loaded)}
	got, err := b.post(context.Background(), []byte("{}"))
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := b.post(context.Background(), []byte("{}")); err != nil || loaded != 1 {
		t.Errorf("found again without need: loaded %d, %v", loaded, err)
	}
}

func TestPostAcceptedIsNoReply(t *testing.T) {
	b := &bridge{to: doerFunc(func() (*http.Response, error) { return reply(http.StatusAccepted, "") })}
	if got, err := b.post(context.Background(), []byte("{}")); got != nil || err != nil {
		t.Errorf("%q %v", got, err)
	}
}

func TestPostFindsAServerThatWasDownAtStart(t *testing.T) {
	loaded := 0
	b := &bridge{load: loads(nil, errors.New("does not answer"), &loaded)}
	if _, err := b.post(context.Background(), []byte("{}")); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("got %v", err)
	}
	b.load = loads(doerFunc(ok), nil, &loaded)
	if got, err := b.post(context.Background(), []byte("{}")); err != nil || string(got) != `{"ok":true}` {
		t.Errorf("%q %v", got, err)
	}
}

func TestPostRetriesOnANewRouteOnlyIfNeverSent(t *testing.T) {
	loaded, sent := 0, 0
	gone := doerFunc(func() (*http.Response, error) { sent++; return nil, dialFailed })
	b := &bridge{to: gone, load: loads(doerFunc(ok), nil, &loaded)}
	if got, err := b.post(context.Background(), []byte("{}")); err != nil || string(got) != `{"ok":true}` || loaded != 1 || sent != 1 {
		t.Errorf("dial failure: %q %v loaded %d sent %d", got, err, loaded, sent)
	}

	// once the request may have arrived, a write must not be sent twice
	loaded, sent = 0, 0
	b = &bridge{to: doerFunc(func() (*http.Response, error) { sent++; return nil, cutOff }), load: loads(doerFunc(ok), nil, &loaded)}
	if _, err := b.post(context.Background(), []byte("{}")); err == nil || !strings.Contains(err.Error(), "unreachable") || loaded != 0 || sent != 1 {
		t.Errorf("cut off: %v loaded %d sent %d", err, loaded, sent)
	}
}

func TestPostNamesARefusal(t *testing.T) {
	refused := fmt.Errorf("POST /mcp: %w", &connector.StatusError{Code: 401, Text: "api key refused"})
	b := &bridge{to: doerFunc(func() (*http.Response, error) { return nil, refused })}
	_, err := b.post(context.Background(), []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "refused the call") || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "unreachable") {
		t.Errorf("got %v", err)
	}
}
