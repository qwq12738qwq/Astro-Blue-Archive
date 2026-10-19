package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"blogcms/internal/auth"
	"blogcms/internal/config"
	"blogcms/internal/httpx"
)

// Login request bounds. Generous enough for a long passphrase, tight enough to
// keep Argon2 cost predictable.
const (
	minPasswordLen = 8
	maxPasswordLen = 200
	maxUsernameLen = 64
	// minSetupPasswordLen is the floor on the *development* first-run form only.
	// A generated password is 20 characters, so production never sees this number.
	minSetupPasswordLen = 4
)

// secondsString formats a duration for the Retry-After header.
func secondsString(d time.Duration) string {
	return strconv.Itoa(int(d.Seconds()))
}

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

	admin, err := d.Admins.GetByUsername(ctx, req.Username)
	verifyErr := error(nil)

	switch {
	case errors.Is(err, auth.ErrNotFound):
		// No administrator with this name exists. Burn equivalent CPU so that
		// an unknown username and a wrong password are not trivially
		// distinguishable.
		auth.VerifyDummy(req.Password)
		verifyErr = auth.ErrMismatch
	case err != nil:
		httpx.WriteError(w, r, err)
		return
	default:
		// The dummy verification runs first so a disabled account costs the same
		// as a wrong password, and the failure below it is identical too — same
		// status, same code, same message — so this cannot be used to discover
		// which usernames exist (§7). A disabled account is refused before a new
		// session is minted, and its existing sessions were deleted when it was
		// disabled.
		if admin.Status != auth.StatusActive {
			auth.VerifyDummy(req.Password)
			verifyErr = auth.ErrMismatch
		} else {
			verifyErr = auth.VerifyPassword(req.Password, admin.PasswordHash)
		}
	}

	// Uniform failure: identical status, code and message for an unknown user,
	// a wrong password, a wrong username and a disabled account.
	if verifyErr != nil {
		_ = d.Throttle.Record(ctx, key, false)
		httpx.WriteError(w, r, httpx.Errorf(http.StatusUnauthorized, httpx.CodeUnauthorized,
			"invalid credentials"))
		return
	}

	// Upgrade a weakened hash opportunistically on a real login.
	if auth.NeedsRehash(admin.PasswordHash) {
		if newHash, err := auth.HashPassword(req.Password); err == nil {
			_ = d.Admins.UpdatePassword(ctx, admin.ID, newHash)
		}
	}
	_ = d.Admins.RecordLogin(ctx, admin.ID)

	// Rotate on login (§12).
	//
	// Every pre-existing session of *this* account is revoked, not everyone's.
	// For the single-administrator case this is the same behaviour as before: it
	// closes the window in which a token stolen earlier would still authenticate
	// after the owner logs in again, and it makes a login a definitive boundary.
	// With several administrators, revoking everyone's sessions would make one
	// person's sign-in sign another out, which is not a security property but a
	// denial of service.
	if err := d.Auth.DestroySessionsForAdmin(ctx, admin.ID, ""); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	rawToken, csrf, expiresAt, err := d.Auth.CreateSession(ctx, admin.ID, httpx.ClientIP(r), r.UserAgent())
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

// setupRequest is the first-run request.
//
// Every field is optional, and that is the point: an empty body means "generate
// the credentials", which is what production does. A body carrying a username and
// a password is the *development* path, and it is refused everywhere else
// (ARCHITECTURE.md ID-71).
type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// setupResponse is returned once. On the generated path it carries the password —
// the only time anything outside the operator's head ever sees it.
type setupResponse struct {
	Username  string `json:"username"`
	Password  string `json:"password,omitempty"`
	Generated bool   `json:"generated"`
	CSRFToken string `json:"csrfToken"`
	ExpiresAt string `json:"expiresAt"`
}

type setupStatusResponse struct {
	SetupRequired bool   `json:"setupRequired"`
	Username      string `json:"username,omitempty"`
	// TypedSetup is true when this deployment also accepts a first-run form that
	// creates an account from credentials the operator types. It is a capability,
	// not a secret, and the login screen uses it to decide which form to show.
	TypedSetup bool `json:"typedSetup"`
}

// setupStatus lets the login page tell "first run" from "locked out".
func (d Deps) setupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := d.Admins.Count(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// The first administrator is the only owner until another owner exists, so
	// the screen can tell the difference between "no account yet" and "an
	// ordinary account is the only one that exists".
	if n == 0 {
		httpx.WriteJSON(w, http.StatusOK, setupStatusResponse{
			SetupRequired: true,
			TypedSetup:    d.Cfg.Deployment == config.DeploymentDevelopment,
		})
		return
	}
	admins, err := d.Admins.List(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	username := ""
	for _, admin := range admins {
		if admin.Role == auth.RoleOwner {
			username = admin.Username
			break
		}
	}
	if username == "" && len(admins) > 0 {
		username = admins[0].Username
	}
	httpx.WriteJSON(w, http.StatusOK, setupStatusResponse{
		SetupRequired: false,
		Username:      username,
		TypedSetup:    d.Cfg.Deployment == config.DeploymentDevelopment,
	})
}

// validateSetup validates the *development* first-run form, where an
// operator chooses their own credentials.
//
// The password floor is lower here than everywhere else in this API — 4 rather
// than 8 — and that is not a policy decision so much as an unavoidable one: a
// development deployment is exactly the place where somebody wants `12345`. It is
// confined to this path, and to a deployment that declared itself a development
// one, because the credentials a real installation gets come from
// auth.GeneratePassword instead.
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
	case n < minSetupPasswordLen:
		fields["password"] = "must be at least 4 characters"
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

// setup creates the first administrator. It is only reachable while none exists,
// which the requireSetup guard enforces, so an installation cannot be re-initialized
// once it has an owner (ARCHITECTURE.md ID-71).
//
// There are two shapes of request, and the difference is a security boundary
// rather than a convenience:
//
//   - an empty body. The backend generates the username and the password, hashes
//     the password with argon2id, and returns the plaintext exactly once — in
//     this response. Nothing else in this repository ever sees it: not the
//     database, not the audit log, not the server log. That is what "no default
//     credentials" means mechanically.
//   - a body carrying a username and a password. Accepted only on a deployment
//     that declared itself a development one (APP_ENV=development); a production
//     deployment refuses it, because "choose your own first password" is a
//     weaker default than being handed a random one.
//
// The account it creates is an *owner*: the role that may create, disable and
// reset the others. Nothing here asks for a GitHub account — a CMS account is
// CMS-local, and binding one is a separate, later, optional step.
func (d Deps) setup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	// An empty body is the generated path. Reading the body unconditionally would
	// turn "no body" into a 400 on the one request that most needs to work.
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	req.Username = strings.TrimSpace(req.Username)

	generated := req.Username == "" && req.Password == ""
	if !generated && d.Cfg.Deployment != config.DeploymentDevelopment {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, "setup_typed_not_allowed",
			"this deployment only accepts the generated first-run account; submit the form with no username or password"))
		return
	}

	if generated {
		username, err := auth.GenerateUsername()
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		password, err := auth.GeneratePassword()
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		req.Username = username
		req.Password = password
	} else if fields := validateSetup(req); len(fields) > 0 {
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusUnprocessableEntity, httpx.CodeValidation, "%s", err))
		return
	}
	admin, err := d.Admins.Create(r.Context(), req.Username, hash, auth.RoleOwner)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.Audit(r.Context(), "admin.setup", admin.Username)

	// Log the new administrator straight in, so first run is one step.
	rawToken, csrf, expiresAt, err := d.Auth.CreateSession(r.Context(), admin.ID, httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.Auth.SetSessionCookie(w, rawToken, expiresAt)

	// The username and whether it was generated are safe to log; the password is
	// not, and it is not in this call.
	slog.Info("admin_setup", "username", admin.Username, "generated", generated)

	httpx.WriteJSON(w, http.StatusCreated, setupResponse{
		Username:  admin.Username,
		Password:  req.Password,
		Generated: generated,
		CSRFToken: csrf,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}
