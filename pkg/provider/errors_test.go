package provider

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestErrorClassificationAndPartialReplay(t *testing.T) {
	for _, tc := range []struct {
		err   error
		kind  ErrorKind
		retry bool
	}{{context.Canceled, ErrorCanceled, false}, {context.DeadlineExceeded, ErrorCanceled, false}, {&httpError{code: http.StatusUnauthorized}, ErrorAuth, false}, {&httpError{code: 429}, ErrorRateLimit, true}, {&httpError{code: 503}, ErrorUnavailable, true}} {
		e := classify(tc.err, 2, false)
		if e.Kind != tc.kind || e.Retryable() != tc.retry {
			t.Fatalf("%+v", e)
		}
		if classify(tc.err, 2, true).Retryable() {
			t.Fatal("partial output replay allowed")
		}
	}
	e := classify(errors.New("quota"), 1, true)
	if CanFailover(e) {
		t.Fatal("partial failover allowed")
	}
	if got := backoff(0, &httpError{code: 429, after: 24 * time.Hour}); got > retryCap {
		t.Fatal("unbounded retry-after")
	}
}
