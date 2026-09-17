package logging

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type contextKey string

const requestIDKey contextKey = "request_id"

// RequestIDFromContext extracts the request ID from context.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// RequestID returns middleware that injects a request ID and logs requests.
func RequestID(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := generateID()
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			r = r.WithContext(ctx)

			rw := &responseWriter{ResponseWriter: w, status: 200}
			start := time.Now()

			next.ServeHTTP(rw, r)

			path := r.URL.Path
			if r.URL.RawQuery != "" {
				path = path + "?" + r.URL.RawQuery
			}
			logger.Info("request",
				"request_id", id,
				"remote_ip", r.RemoteAddr,
				"method", r.Method,
				"path", path,
				"status", rw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

// Preserve ResponseController deadlines/flush through request logging.
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker, required for WebSocket upgrades.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
