package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"blogcms/internal/auth"
	"blogcms/internal/httpx"
)

// Login request bounds. Generous enough for a long passphrase, tight enough to
// keep Argon2 cost predictable.
const (
	minPasswordLen = 8
	maxPasswordLen = 200
	maxUsernameLen = 64
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Username  string `json:"username"`
	CSRFToken string `json:"csrfToken"`
	ExpiresAt string `json:"expiresAt"`
}

type sessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
	CSRFToken     string `json:"csrfToken,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
}

func registerAuthRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/auth/session", d.getSession)
	mux.HandleFunc("POST /api/v1/auth/login", d.requireOrigin(requireJSONContentType(d.login)))
	mux.HandleFunc("POST /api/v1/auth/logout", d.logout)
	mux.HandleFunc("POST /api/v1/auth/setup", d.requireSetup(requireJSONContentType(d.setup)))
	mux.HandleFunc("GET /api/v1/auth/setup", d.setupStatus)
}

// requireOrigin applies the Origin check to unauthenticated mutations too.
// Login and logout have no session yet, so they cannot use the CSRF token and
// must rely on the Origin header plus SameSite=Strict (§13).
func (d Deps) requireOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.Auth.VerifyOrigin(r) {
			d.refuseOrigin(w, r)
			return
		}
		next(w, r)
	}
}

func (d Deps) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	fields := map[string]string{}
	if req.Username == "" {
		fields["username"] = "required"
	} else if utf8.RuneCountInString(req.Username) > maxUsernameLen {
		fields["username"] = "too long"
	}
	if req.Password == "" {
		fields["password"] = "required"
	} else if len(req.Password) > maxPasswordLen {
		fields["password"] = "too long"
	}
	if len(fields) > 0 {
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}

	ctx := r.Context()
	key := auth.Key{Username: req.Username, IPHash: d.Auth.HashIP(httpx.ClientIP(r))}

	// Throttle before doing any password work.
	decision, err := d.Throttle.Check(ctx, key)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !decision.Allowed {
		w.Header().Set("Retry-After", secondsString(decision.RetryIn))
		_ = d.Throttle.Record(ctx, key, false)
		httpx.WriteError(w, r, httpx.Errorf(http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many login attempts; try again later"))
		return
	}

	admin, err := d.Admins.Get(ctx)
	verifyErr := error(nil)

	switch {
	case errors.Is(err, auth.ErrNotFound):
		// No administrator exists yet. Burn equivalent CPU so that "setup
		// pending" and "wrong password" are not trivially distinguishable.
		auth.VerifyDummy(req.Password)
		verifyErr = auth.ErrMismatch
	case err != nil:
		httpx.WriteError(w, r, err)
		return
	case admin.Username != req.Username:
		// There is one administrator (D7), and this store fetches it without regard to
		// which name was asked for. So the submitted username has to be compared here,
		// or the field is accepted and thrown away: `{"username":"nobody"}` plus the
		// right password would return a session for whoever the account actually is,
		// and a typo'd username would silently sign in as a different name than the
		// caller asked for.
		//
		// The dummy verification runs first so this costs the same as a wrong password.
		// The failure below it is identical too — same status, same code, same message —
		// so this cannot be used to discover whether a username exists (§7).
		auth.VerifyDummy(req.Password)
		verifyErr = auth.ErrMismatch
	default:
		verifyErr = auth.VerifyPassword(req.Password, admin.PasswordHash)
	}

	// Uniform failure: identical status, code and message for an unknown user,
	// a wrong password and a wrong username.
	if verifyErr != nil {
		_ = d.Throttle.Record(ctx, key, false)
		httpx.WriteError(w, r, httpx.Errorf(http.StatusUnauthorized, httpx.CodeUnauthorized,
			"invalid credentials"))
		return
	}

	// Upgrade a weakened hash opportunistically on a real login.
	if auth.NeedsRehash(admin.PasswordHash) {
		if newHash, err := auth.HashPassword(req.Password); err == nil {
			_ = d.Admins.UpdatePassword(ctx, newHash)
		}
	}

	// Rotate on login (§12).
	//
	// Every pre-existing session is revoked, not just the caller's. For a
	// single-administrator blog this closes the window in which a token stolen
	// earlier would still authenticate after the owner logs in again, and it
	// makes a login a definitive boundary. The trade-off is that signing in from
	// a second device signs the first one out, which is the right default here.
	if err := d.Auth.DestroyAllSessions(ctx); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	rawToken, csrf, expiresAt, err := d.Auth.CreateSession(ctx, httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	_ = d.Throttle.Reset(ctx, key)
	_ = d.Throttle.Record(ctx, key, true)
	d.Auth.SetSessionCookie(w, rawToken, expiresAt)

	httpx.WriteJSON(w, http.StatusOK, loginResponse{
		Username:  admin.Username,
		CSRFToken: csrf,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

func (d Deps) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
		// Logout is idempotent even without a CSRF token: revoking a session
		// the caller already holds cannot be used to attack anyone.
		_ = d.Auth.DestroySession(ctx, cookie.Value)
	}
	d.Auth.ClearSessionCookie(w)

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (d Deps) getSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		httpx.WriteJSON(w, http.StatusOK, sessionResponse{Authenticated: false})
		return
	}
	session, err := d.Auth.LookupSession(r.Context(), cookie.Value)
	if err != nil {
		d.Auth.ClearSessionCookie(w)
		httpx.WriteJSON(w, http.StatusOK, sessionResponse{Authenticated: false})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sessionResponse{
		Authenticated: true,
		Username:      session.Username,
		CSRFToken:     session.CSRFToken,
		ExpiresAt:     session.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type setupStatusResponse struct {
	SetupRequired bool   `json:"setupRequired"`
	Username      string `json:"username,omitempty"`
}

// setupStatus lets the login page tell "first run" from "locked out".
func (d Deps) setupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := d.Admins.Count(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if n == 0 {
		httpx.WriteJSON(w, http.StatusOK, setupStatusResponse{SetupRequired: true})
		return
	}
	admin, err := d.Admins.Get(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, setupStatusResponse{SetupRequired: false, Username: admin.Username})
}

// setupRequestPasswords are the minimum requirements for the single admin.
func validateSetup(req setupRequest) map[string]string {
	fields := map[string]string{}

	username := strings.TrimSpace(req.Username)
	switch {
	case username == "":
		fields["username"] = "required"
	case len(username) > maxUsernameLen:
		fields["username"] = "too long"
	case !validUsername(username):
		fields["username"] = "use 3-64 characters: letters, digits, dot, dash, underscore"
	}

	switch n := utf8.RuneCountInString(req.Password); {
	case n < minPasswordLen:
		fields["password"] = "must be at least 8 characters"
	case n > maxPasswordLen:
		fields["password"] = "must be at most 200 characters"
	}

	return fields
}

func validUsername(s string) bool {
	if len(s) < 3 || len(s) > maxUsernameLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// setup creates the single administrator. It is only reachable while no admin
// exists, which is enforced both by the route guard and by CHECK (id = 1).
func (d Deps) setup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)

	if fields := validateSetup(req); len(fields) > 0 {
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusUnprocessableEntity, httpx.CodeValidation, "%s", err))
		return
	}
	if err := d.Admins.Create(r.Context(), req.Username, hash); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.Audit(r.Context(), "admin.setup", req.Username)

	// Log the new administrator straight in, so first run is one step.
	rawToken, csrf, expiresAt, err := d.Auth.CreateSession(r.Context(), httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.Auth.SetSessionCookie(w, rawToken, expiresAt)

	httpx.WriteJSON(w, http.StatusCreated, loginResponse{
		Username:  req.Username,
		CSRFToken: csrf,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

func secondsString(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}
