package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"blogcms/internal/auth"
	"blogcms/internal/httpx"
)

// refuseOrigin answers a mutation whose Origin is not in PUBLIC_ORIGIN.
//
// ARCHITECTURE.md ID-22: the response names the variable, because "request origin is
// not allowed" on a site that renders perfectly sends the operator looking at the CMS
// instead of at their own configuration. The log line is the other half: it records
// the Origin that was actually sent and the Host the request arrived on, so the
// address to allowlist is a fact rather than a guess — which is the whole difference
// between a two-minute fix and an afternoon of guessing at addresses.
//
// Nothing sensitive is logged or returned. The allowed set is operator configuration
// rather than a secret, but the comment form reaches this path with no session, so
// the response body stays generic.
func (d Deps) refuseOrigin(w http.ResponseWriter, r *http.Request) {
	slog.Log(r.Context(), slog.LevelWarn, "origin_not_allowed",
		"request_id", httpx.RequestIDFrom(r.Context()),
		"method", r.Method,
		"path", r.URL.Path,
		"origin", r.Header.Get("Origin"),
		"host", r.Host,
		"remote_ip", httpx.ClientIP(r),
		"allowed", strings.Join(d.Cfg.PublicOrigins, ","),
	)
	httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
		"request origin is not allowed; add the address you are browsing from to PUBLIC_ORIGIN"))
}

// sessionKey is the request context key for a resolved admin session.
type sessionKey struct{}

// SessionFrom returns the authenticated session on this request, or nil.
func SessionFrom(r *http.Request) *auth.Session {
	if s, ok := r.Context().Value(sessionKey{}).(*auth.Session); ok {
		return s
	}
	return nil
}

func withSession(r *http.Request, s *auth.Session) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sessionKey{}, s))
}

// requireSession wraps a handler so only an authenticated admin reaches it.
//
// Two independent checks run on every admin request:
//
//  1. the session cookie resolves to a live session, and
//  2. for mutations, Origin matches PUBLIC_ORIGIN *and* the CSRF header matches
//     the session's csrf_secret.
//
// A forged Origin or a missing CSRF token is a 403, never a 401, so a valid
// cookie from a foreign site cannot even probe the endpoint.
func (d Deps) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookieName)
		if err != nil || cookie.Value == "" {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusUnauthorized, httpx.CodeUnauthorized,
				"authentication required"))
			return
		}

		session, err := d.Auth.LookupSession(r.Context(), cookie.Value)
		if err != nil {
			d.Auth.ClearSessionCookie(w)
			httpx.WriteError(w, r, httpx.Errorf(http.StatusUnauthorized, httpx.CodeUnauthorized,
				"authentication required"))
			return
		}

		if isMutation(r.Method) {
			if !d.Auth.VerifyOrigin(r) {
				httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
					"request origin is not allowed; add the address you are browsing from to PUBLIC_ORIGIN"))
				return
			}
			if !auth.VerifyCSRF(session, r.Header.Get(auth.CSRFHeaderName)) {
				httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
					"missing or invalid CSRF token"))
				return
			}
		}

		next(w, withSession(r, session))
	}
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// requireSetup gates the login endpoint until an administrator exists, so the
// first-run setup flow cannot be replayed by a second visitor.
func (d Deps) requireSetup(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := d.Admins.Count(r.Context())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if n > 0 {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
				"an administrator already exists; setup is closed"))
			return
		}
		next(w, r)
	}
}

// contentTypeJSON rejects bodies that are not JSON, so a form post cannot reach
// the login handler through a browser.
func requireJSONContentType(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		if strings.TrimSpace(ct) != "application/json" {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Content-Type must be application/json"))
			return
		}
		next(w, r)
	}
}
