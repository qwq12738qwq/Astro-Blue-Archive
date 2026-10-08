package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxLog
)

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RequestID returns a per-request correlation ID.
func RequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-fallback"
	}
	return hex.EncodeToString(b[:])
}

// WithRequestID stores an ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// RequestIDFrom reads the request ID from the context.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// RequestIDMiddleware assigns or propagates a request ID and echoes it back so
// the Astro frontend can log it alongside its own errors.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id = RequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// RecoverMiddleware turns a panic into a 500 instead of dropping the
// connection, and logs it with the request ID.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("panic recovered",
					"request_id", RequestIDFrom(r.Context()),
					"method", r.Method, "path", r.URL.Path, "panic", p)
				WriteError(w, r, Errorf(http.StatusInternalServerError, CodeInternal,
					"internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// LoggingMiddleware emits one structured line per request with timestamp,
// level, request id, method, path, status and duration (ARCHITECTURE.md §30).
//
// It never logs bodies, passwords, session tokens or comment content.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}

		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}

		level := slog.LevelInfo
		switch {
		case sw.status >= 500:
			level = slog.LevelError
		case sw.status >= 400:
			level = slog.LevelWarn
		}
		slog.Log(r.Context(), level, "http",
			"request_id", RequestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_ip", clientIP(r),
		)
	})
}

// MaxHeaderBytes caps request headers (ARCHITECTURE.md §8 #15).
const MaxHeaderBytes = 1 << 20

// NewServer builds an http.Server with hardened transport limits.
//
// ARCHITECTURE.md §1: this server serves JSON only.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}

// ClientIPHeader carries the real peer address from the single origin.
//
// Astro is the only listening process and reads it off the TCP socket, not off a
// client-supplied header, so it cannot be forged by the party being limited. Go
// still verifies the sender: the value is honoured only when the request actually
// arrived over loopback, which is the only way anything can reach this process.
// X-Forwarded-For is never read, for the original reason it was excluded: it is
// client-controlled.
const ClientIPHeader = "X-Client-IP"

// clientIP extracts the peer address for rate limiting and audit records.
//
// Without this, every request looks like 127.0.0.1 because Astro proxies them,
// so all clients shared one bucket: five bad logins from anyone locked the admin
// out of their own blog, repeatable every minute, and one spammer could silence
// every commenter.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	// Only a loopback peer may speak for another address.
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}

	forwarded := strings.TrimSpace(r.Header.Get(ClientIPHeader))
	if forwarded == "" {
		return host
	}
	// A single address, not a chain: one hop, so no "first entry wins" parsing to
	// get wrong.
	if ip := net.ParseIP(forwarded); ip != nil {
		return ip.String()
	}
	return host
}

// ForwardedHostHeader carries the authority the browser dialled, forwarded by the
// single origin.
//
// ARCHITECTURE.md ID-11/ID-24: Go never sees the browser's Host, because Astro
// proxies every /api/* call over loopback. That made the CSRF Origin check compare
// the visitor's origin against Go's own address, so no browser was ever same-origin
// and only an explicitly listed address could post anything. Astro reads the real
// authority off the socket and states it here; Go honours it under the same
// condition as ClientIPHeader, and never from a non-loopback peer.
const ForwardedHostHeader = "X-Forwarded-Host"

// RequestAuthority is the scheme://authority a request really arrived on, preferring
// what the single origin forwarded and falling back to this process's own Host when
// the caller is not the single origin.
func RequestAuthority(r *http.Request, scheme string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return scheme + "://" + r.Host
	}
	if fwd := strings.TrimSpace(r.Header.Get(ForwardedHostHeader)); fwd != "" {
		return scheme + "://" + fwd
	}
	return scheme + "://" + r.Host
}

// ClientIP exposes the untrusted peer IP for hashing.
func ClientIP(r *http.Request) string { return clientIP(r) }
