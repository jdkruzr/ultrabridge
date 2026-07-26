package processor

import (
	"errors"
	"net/http"
)

// TransientError marks a failure where the request never got a verdict from
// the model: the backend was unreachable, timed out, was overloaded, or blew
// up server-side. Retrying later can plausibly succeed.
//
// The distinction matters because the callers' queues treat the two classes
// differently — a transient failure is requeued with backoff, a permanent one
// is failed terminally. A 4xx (malformed request, bad credentials, a model
// name the server rejects outright) IS a verdict, and will be identical on
// every retry, so it is deliberately NOT transient.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Transient wraps err as retryable. Returns nil for a nil error so it can be
// applied inline at a return site.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return &TransientError{Err: err}
}

// IsTransient reports whether err, or anything it wraps, is retryable. Works
// through the callers' own %w wrapping (e.g. "ocr page 3: %w").
func IsTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t)
}

// transientHTTPStatus reports whether an HTTP response code is worth a retry.
// 5xx is a server-side fault, 429 is explicit backpressure, and 408 is the
// server reporting its own timeout — all say "try again", unlike other 4xx.
func transientHTTPStatus(code int) bool {
	return code >= 500 ||
		code == http.StatusTooManyRequests ||
		code == http.StatusRequestTimeout
}
