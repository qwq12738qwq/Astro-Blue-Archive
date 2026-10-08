package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"time"

	"blogcms/internal/httpx"
	"blogcms/internal/store"
)

// SessionCookieName must match SESSION_COOKIE in astro/src/lib/api.ts.
// The integration tests assert both sides agree.
const SessionCookieName = "blog_session"

// CSRFHeaderName is the double-submit header the Astro admin client must send
// on every mutating request (ARCHITECTURE.md §13).
const CSRFHeaderName = "X-CSRF-Token"

// ErrUnauthenticated is returned for any missing, unknown or expired session.
var ErrUnauthenticated = errors.New("unauthenticated")

// Session is a resolved, authenticated admin session.
type Session struct {
	ID        string
	Username  string
	CSRFToken string
	ExpiresAt time.Time
}

// Manager owns session lifecycle and login throttling.
type Manager struct {
	db      *store.DB
	ttl     time.Duration
	idleTTL time.Duration

	// origins are the rules a mutation's Origin is checked against.
	origins []originRule
	// secureCookie mirrors SECURE_COOKIES.
	secureCookie bool
}

// originRule is one entry of the Origin allowlist.
//
// An entry with a port is compared whole. An entry without one is compared as
// scheme://host and therefore accepts any port — which is what a derived allowlist
// needs, because the backend is told its own port and never the one Astro serves
// (ARCHITECTURE.md ID-23). The distinction is decided once, here, rather than on
// every request.
type originRule struct {
	full   string // "https://blog.example.com:8443"
	scheme string // "http" or "https", empty for a full rule
	host   string // "blog.example.com", empty for a full rule
}

// NewManager builds a session manager.
//
// The rules are compiled once: a caller mutating its slice afterwards cannot widen
// the allowlist, and no parsing happens on the request path.
func NewManager(db *store.DB, ttl, idleTTL time.Duration, origins []string, secureCookie bool) *Manager {
	rules := make([]originRule, 0, len(origins))
	for _, origin := range origins {
		rules = append(rules, compileOrigin(origin))
	}
	return &Manager{
		db:           db,
		ttl:          ttl,
		idleTTL:      idleTTL,
		origins:      rules,
		secureCookie: secureCookie,
	}
}

// compileOrigin turns one allowlist entry into a comparable rule.
//
// A malformed entry matches nothing: it is compared as a full string, so it can only
// ever match an Origin header that is byte-identical to itself.
func compileOrigin(origin string) originRule {
	if u, err := url.Parse(origin); err == nil && u.Port() == "" && u.Scheme != "" && u.Host != "" {
		return originRule{scheme: u.Scheme, host: u.Host}
	}
	return originRule{full: origin}
}

// tokenBytes is the entropy of a session token and of a CSRF secret.
const tokenBytes = 32

// randomToken returns the raw token (sent to the client once) and the SHA-256
// of that same string (the only form persisted).
//
// The hash is taken over the *encoded* token so that lookup can recompute it
// from the cookie value alone.
func randomToken() (raw string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashIP returns a stable, non-reversible identifier for an IP address.
//
// Raw IPs are not stored: they are personal data, they are not needed, and a
// keyed hash is enough to rate-limit and audit.
func (m *Manager) HashIP(ip string) []byte {
	sum := sha256.Sum256([]byte("blogcms-ip:" + ip))
	return sum[:16]
}

// CreateSession issues a new session. The raw token is returned once and never
// stored; the database only receives its SHA-256 (§12).
func (m *Manager) CreateSession(ctx context.Context, ip string, userAgent string) (rawToken string, csrfSecret string, expiresAt time.Time, err error) {
	rawToken, tokenHash, err := randomToken()
	if err != nil {
		return "", "", time.Time{}, err
	}

	var (
		sessionID string
		csrfB     []byte
	)
	sessionID, err = randomHex(16)
	if err != nil {
		return "", "", time.Time{}, err
	}
	csrfB, err = randomCSRF()
	if err != nil {
		return "", "", time.Time{}, err
	}
	csrfSecret = base64.RawURLEncoding.EncodeToString(csrfB)

	now := time.Now().UTC()
	expiresAt = now.Add(m.ttl)

	_, err = m.db.ExecContext(ctx,
		`INSERT INTO session (id, token_hash, csrf_secret, created_at, expires_at, last_seen_at, ip_hash, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, tokenHash, csrfB,
		now.Format(time.RFC3339), expiresAt.Format(time.RFC3339), now.Format(time.RFC3339),
		m.HashIP(ip), truncate(userAgent, 255),
	)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return rawToken, csrfSecret, expiresAt, nil
}

func randomCSRF() ([]byte, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// LookupSession resolves a raw cookie token to a live session and slides its
// idle timeout forward.
func (m *Manager) LookupSession(ctx context.Context, rawToken string) (*Session, error) {
	if rawToken == "" {
		return nil, ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(rawToken))

	now := time.Now().UTC()

	var (
		id        string
		csrf      []byte
		expiresAt string
		lastSeen  string
		username  string
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT s.id, s.csrf_secret, s.expires_at, s.last_seen_at, u.username
		   FROM session s JOIN admin_user u ON u.id = 1
		  WHERE s.token_hash = ?`, sum[:],
	).Scan(&id, &csrf, &expiresAt, &lastSeen, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}

	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	seen, err := time.Parse(time.RFC3339, lastSeen)
	if err != nil {
		return nil, ErrUnauthenticated
	}

	// Absolute expiry and idle timeout are both enforced.
	if now.After(exp) || now.Sub(seen) > m.idleTTL {
		_, _ = m.db.ExecContext(ctx, `DELETE FROM session WHERE id = ?`, id)
		return nil, ErrUnauthenticated
	}

	// Slide the idle window. Updating on every request keeps the write count
	// equal to the request count for a single-admin blog.
	if _, err := m.db.ExecContext(ctx,
		`UPDATE session SET last_seen_at = ? WHERE id = ?`, now.Format(time.RFC3339), id); err != nil {
		return nil, err
	}

	return &Session{
		ID:        id,
		Username:  username,
		CSRFToken: base64.RawURLEncoding.EncodeToString(csrf),
		ExpiresAt: exp,
	}, nil
}

// DestroySession revokes a session immediately (§12 logout).
func (m *Manager) DestroySession(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(rawToken))
	_, err := m.db.ExecContext(ctx, `DELETE FROM session WHERE token_hash = ?`, sum[:])
	return err
}

// DestroyAllSessions revokes every session. Used after a credential change.
func (m *Manager) DestroyAllSessions(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM session`)
	return err
}

// PurgeExpired removes dead sessions. Called periodically.
func (m *Manager) PurgeExpired(ctx context.Context) (int64, error) {
	res, err := m.db.ExecContext(ctx, `DELETE FROM session WHERE expires_at < ?`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetSessionCookie applies the hardened cookie flags (§12).
//
// HttpOnly keeps the token out of JavaScript, SameSite=Strict blocks CSRF,
// Path=/ lets Astro and the API share it on the single Caddy origin.
func (m *Manager) SetSessionCookie(w http.ResponseWriter, rawToken string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: true,
		Secure:   m.secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie expires the session cookie.
func (m *Manager) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
}

// VerifyOrigin answers "did this mutation come from the address the visitor is
// actually on?"
//
// ARCHITECTURE.md ID-24. It has three accepted answers and refuses everything else:
//
//  1. **Same-origin, and the browser agrees.** The Origin header is exactly the
//     scheme://authority the request arrived on. This is the only form of the
//     question a server can answer without being told the answer first, and it is why
//     the check works behind NAT, behind a port-forward, in a container and on a
//     public IP — cases where "the address the browser used" is not any address the
//     machine itself has.
//  2. **The host is an IP literal.** Required for (1). It closes DNS rebinding: an
//     attacker's name pointing at this server would otherwise make a cross-origin
//     request look same-origin, and a rebound name is always a name, never an IP.
//     A real hostname therefore has to be listed in PUBLIC_ORIGIN — one line, once,
//     rather than a new address every time the network changes.
//  3. **An explicitly configured origin.** Exact match, constant-time, for everything
//     (2) deliberately excludes: a domain, and a site behind TLS termination where
//     Go sees http behind an https front end.
//
// A cross-site request is refused because a browser sets Origin to the *attacker's*
// origin, while the authority is this server's. A non-browser caller can forge both
// headers — and can equally forge the session cookie, which is what it would need to
// act. This check is about CSRF, not about authentication.
//
// Everything else — different host, different scheme without TLS termination,
// subdomain, a path, a query, the list pasted as one string — is refused.
func (m *Manager) VerifyOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// A same-origin form POST from a browser always sends Origin. Absence
		// means a non-browser client, which cannot hold the cookie anyway
		// (HttpOnly + SameSite), but be strict for state-changing requests.
		return false
	}

	// Same-origin: the browser says it is on the address this request arrived on.
	// That is the one comparison that cannot lock the owner out, because it needs
	// no list of addresses to be maintained and no list of addresses the server
	// cannot know — behind NAT, a container port-forward, a reverse proxy or a
	// public IP, the address the browser used is not an address this machine has
	// (ARCHITECTURE.md ID-24).
	if sameOrigin(origin, requestAuthority(r)) {
		return true
	}

	// Anything else has to be configured. The loop is constant-time and does not
	// return early, so the answer leaks neither which entry matched nor how far a
	// guess got.
	matched := 0
	for _, rule := range m.origins {
		if rule.host != "" {
			matched |= subtle.ConstantTimeCompare(
				[]byte(originAuthority(origin)), []byte(rule.scheme+"://"+rule.host))
			continue
		}
		matched |= subtle.ConstantTimeCompare([]byte(origin), []byte(rule.full))
	}
	return matched == 1
}

// requestAuthority is the scheme://authority this request really came from.
//
// Astro is the single origin and proxies every /api/* call, so the request Go sees
// carries Go's own loopback address in Host. The authority the browser dialled
// arrives in X-Forwarded-Host, set by Astro from the socket, and Go honours it only
// when the peer is loopback — the same condition under which it already honours
// X-Client-IP (ARCHITECTURE.md ID-24).
//
// Without that header the comparison would be against 127.0.0.1:9901 and no browser
// could ever be same-origin, which is precisely the state that locked the owner out.
func requestAuthority(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return httpx.RequestAuthority(r, scheme)
}

// sameOrigin reports whether an Origin header is exactly scheme://authority, with no
// path, query, fragment or userinfo, and equals the given authority.
func sameOrigin(origin, authority string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(u.Scheme+"://"+u.Host), []byte(authority)) == 1
}

// originAuthority reduces an Origin header to scheme://host:port, dropping the port
// only when the configured entry has none, and only when the value really is a bare
// origin. Anything else is returned unchanged and therefore cannot equal a rule.
func originAuthority(origin string) string {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return origin
	}
	return u.Scheme + "://" + u.Host
}

// VerifyCSRF performs the double-submit comparison in constant time.
func VerifyCSRF(session *Session, presented string) bool {
	if session == nil || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(presented)) == 1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
