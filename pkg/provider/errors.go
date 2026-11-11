package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

type ErrorKind string

const (
	ErrorConfig      ErrorKind = "configuration"
	ErrorAuth        ErrorKind = "authentication"
	ErrorRateLimit   ErrorKind = "rate_limit"
	ErrorUnavailable ErrorKind = "unavailable"
	ErrorRequest     ErrorKind = "request"
	ErrorTransport   ErrorKind = "transport"
	ErrorStream      ErrorKind = "stream"
	ErrorCanceled    ErrorKind = "canceled"
)

// Error describes recovery facts, not an interpretation of the model's answer.
// Partial forbids replay or account failover after any visible output.
type Error struct {
	Kind       ErrorKind
	Status     int
	RetryAfter time.Duration
	Attempts   int
	Partial    bool
	Cause      error
}

func (e *Error) Error() string { return e.Cause.Error() }
func (e *Error) Unwrap() error { return e.Cause }
func (e *Error) Retryable() bool {
	return !e.Partial && (e.Kind == ErrorRateLimit || e.Kind == ErrorUnavailable || e.Kind == ErrorTransport || e.Kind == ErrorStream)
}

func classify(err error, attempts int, partial bool) *Error {
	var previous *Error
	if errors.As(err, &previous) {
		copy := *previous
		copy.Attempts, copy.Partial = attempts, partial || previous.Partial
		return &copy
	}
	e := &Error{Kind: ErrorStream, Cause: err, Attempts: attempts, Partial: partial}
	var he *httpError
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		e.Kind = ErrorCanceled
	case errors.As(err, new(configError)):
		e.Kind = ErrorConfig
	case errors.As(err, &he):
		e.Status, e.RetryAfter = he.code, min(max(he.after, 0), retryCap)
		switch {
		case he.code == http.StatusUnauthorized || he.code == http.StatusForbidden:
			e.Kind = ErrorAuth
		case he.code == http.StatusTooManyRequests || he.code == 529:
			e.Kind = ErrorRateLimit
		case he.code >= 500:
			e.Kind = ErrorUnavailable
		default:
			e.Kind = ErrorRequest
		}
	case errors.As(err, &ne):
		e.Kind = ErrorTransport
	}
	return e
}

// CanFailover is intentionally stricter than Exhausted's legacy CLI messages.
func CanFailover(err error) bool {
	var e *Error
	return errors.As(err, &e) && !e.Partial && e.Kind != ErrorCanceled && Exhausted(err)
}
