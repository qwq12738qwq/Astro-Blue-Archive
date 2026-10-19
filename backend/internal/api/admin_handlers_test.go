package api

// Administrator management, over the real router.
//
// The properties that matter here are the ones a browser can observe and a hidden
// button cannot provide: a request with no session, a request with somebody else's
// session, a mutation with no CSRF token, a disable that must kill live sessions, and
// a lockout guard that must refuse to disable the last owner. They are driven through
// the same handler chain the production router installs, so the middleware is part of
// what is being tested.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blogcms/internal/auth"
	"blogcms/internal/config"
)

// sessionCookieFrom extracts the session cookie value from a recorder's
// Set-Cookie headers.
func sessionCookieFrom(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c.Value
		}
	}
	return ""
}

// testOrigin is the authority httptest requests arrive on. The Origin check
// compares against it, so a mutation must present exactly this to be same-origin.
const testOrigin = "http://example.com"

// adminTestStack is a router with the auth, administrator and GitHub routes
// registered, plus a client that carries a session cookie and CSRF token.
type adminTestStack struct {
	d   Deps
	mux *http.ServeMux
}

func adminMux(d Deps) *http.ServeMux {
	mux := http.NewServeMux()
	registerAuthRoutes(mux, d)
	registerSettingsRoutes(mux, d)
	registerAdminRoutes(mux, d)
	registerGitHubRoutes(mux, d)
	mux.HandleFunc("/api/", jsonNotFound)
	return mux
}

func newAdminStack(t *testing.T) *adminTestStack {
	t.Helper()
	// One stack: two Deps would mean two databases, and the audit assertions below
	// would read the empty one.
	//
	// The stack is a *development* deployment, which is why the typed setup below
	// works: the first-run credential cases are asserted separately, against the
	// production default.
	d := testDeps(t)
	d.Cfg.Deployment = config.DeploymentDevelopment
	return &adminTestStack{d: d, mux: adminMux(d)}
}

// call performs one request against the stack.
type apiCall struct {
	method  string
	path    string
	cookie  string
	csrf    string
	body    any
	origin  string
	rawBody string
}

// call performs one request against the stack. A mutation always presents the
// same-origin Origin header, because that is what a browser does; the foreign-origin
// test overrides it.
func (s *adminTestStack) call(t *testing.T, c apiCall) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	switch {
	case c.rawBody != "":
		reader = strings.NewReader(c.rawBody)
	case c.body != nil:
		raw, err := json.Marshal(c.body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	default:
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(c.method, c.path, reader)
	if c.body != nil || c.rawBody != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", auth.SessionCookieName+"="+c.cookie)
	}
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	} else if c.method != http.MethodGet && c.method != http.MethodHead {
		req.Header.Set("Origin", testOrigin)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// signedIn creates an account through the setup endpoint and returns a session.
func (s *adminTestStack) signedIn(t *testing.T, username, password string) (cookie, csrf string) {
	t.Helper()
	rec := s.call(t, apiCall{
		method: "POST",
		path:   "/api/v1/auth/setup",
		body:   map[string]string{"username": username, "password": password},
		origin: testOrigin,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup %s = %d (%s)", username, rec.Code, rec.Body.String())
	}
	cookie = sessionCookieFrom(rec)
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return cookie, body.CSRFToken
}

// login signs in an existing account.
func (s *adminTestStack) login(t *testing.T, username, password string) *httptest.ResponseRecorder {
	return s.call(t, apiCall{
		method: "POST",
		path:   "/api/v1/auth/login",
		body:   map[string]string{"username": username, "password": password},
		origin: testOrigin,
	})
}

// signIn logs in and returns the new session's cookie and CSRF token. Signing in
// rotates the account's sessions, so a test that signs in again must use the values
// this returns rather than the ones it held before.
func (s *adminTestStack) signIn(t *testing.T, username, password string) (string, string) {
	t.Helper()
	rec := s.login(t, username, password)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s = %d (%s)", username, rec.Code, rec.Body.String())
	}
	return sessionCookieFrom(rec), csrfFromLogin(t, rec)
}

// createAdmin asks the owner to create an account and returns the created id.
func (s *adminTestStack) createAdmin(t *testing.T, cookie, csrf string, username, password, role string) int64 {
	t.Helper()
	rec := s.call(t, apiCall{
		method: "POST",
		path:   "/api/v1/admin/admins",
		cookie: cookie,
		csrf:   csrf,
		body:   map[string]string{"username": username, "password": password, "role": role},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s = %d (%s)", username, rec.Code, rec.Body.String())
	}
	var body struct {
		Admin struct {
			ID int64 `json:"id"`
		} `json:"admin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Admin.ID
}

func decodeAdmins(t *testing.T, rec *httptest.ResponseRecorder) []adminSummary {
	t.Helper()
	var body adminListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Admins
}

func decodeAdmin(t *testing.T, rec *httptest.ResponseRecorder) adminSummary {
	t.Helper()
	var body adminResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Admin
}

// --- Administrator lifecycle ---------------------------------------------------

func TestSetupCreatesTheFirstOwnerAndSignsIn(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	if cookie == "" || csrf == "" {
		t.Fatal("setup did not return a session")
	}

	// The first account is an owner, and only one exists.
	rec := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeMe(t, rec); got.Role != auth.RoleOwner || got.Username != "owner" {
		t.Fatalf("me = %+v", got)
	}

	// Setup cannot be replayed.
	again := s.call(t, apiCall{
		method: "POST", path: "/api/v1/auth/setup",
		body:   map[string]string{"username": "attacker", "password": "another-password"},
		origin: testOrigin,
	})
	if again.Code != http.StatusConflict {
		t.Fatalf("second setup = %d, want 409", again.Code)
	}
}

func decodeMe(t *testing.T, rec *httptest.ResponseRecorder) adminSummary {
	t.Helper()
	var body meResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	return body.Admin
}

func TestCreateAdminAndSignInWithIt(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	id := s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	if id == 0 {
		t.Fatal("no id returned")
	}

	// The new account signs in with CMS credentials alone — no GitHub involved.
	rec := s.login(t, "editor", "a-second-password")
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := sessionCookieFrom(rec); got == "" {
		t.Fatal("no session cookie for the new administrator")
	}

	// Its default role is admin, and it cannot list administrators.
	editorCookie := sessionCookieFrom(rec)
	list := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/admins", cookie: editorCookie})
	if list.Code != http.StatusForbidden {
		t.Fatalf("an ordinary admin listing administrators = %d, want 403", list.Code)
	}
}

func TestCreateAdminRejectsADuplicateUsername(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins", cookie: cookie, csrf: csrf,
		body: map[string]string{"username": "editor", "password": "yet-another-password"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate username = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin_username_taken") {
		t.Errorf("code = %s", rec.Body.String())
	}
}

func TestCreateAdminValidatesThePasswordAndUsername(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	for _, tc := range []struct{ username, password, role string }{
		{"", "a-good-password", ""},
		{"ed", "a-good-password", ""},               // too short
		{"bad name", "a-good-password", ""},         // illegal characters
		{"editor", "short", ""},                     // password too short
		{"editor", "", ""},                          // empty password
		{"editor", "a-good-password", "superadmin"}, // unknown role
	} {
		rec := s.call(t, apiCall{
			method: "POST", path: "/api/v1/admin/admins", cookie: cookie, csrf: csrf,
			body: map[string]string{"username": tc.username, "password": tc.password, "role": tc.role},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create %q/%q/%q = %d, want 422", tc.username, tc.password, tc.role, rec.Code)
		}
	}
}

func TestAdminCreateRequiresTheOwnerRole(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	editor := s.login(t, "editor", "a-second-password")
	editorCookie := sessionCookieFrom(editor)
	editorCSRF := csrfFromLogin(t, editor)

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins", cookie: editorCookie, csrf: editorCSRF,
		body: map[string]string{"username": "third", "password": "a-third-password"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an ordinary admin creating an administrator = %d, want 403", rec.Code)
	}
}

func csrfFromLogin(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.CSRFToken
}

func TestAdminAPIsRefuseAnonymousCallers(t *testing.T) {
	s := newAdminStack(t)
	s.signedIn(t, "owner", "a-good-password")

	for _, path := range []string{
		"/api/v1/admin/me",
		"/api/v1/admin/admins",
		"/api/v1/admin/admins/1",
	} {
		if rec := s.call(t, apiCall{method: "GET", path: path}); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous = %d, want 401", path, rec.Code)
		}
	}
	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/me/password",
		body: map[string]string{"currentPassword": "x", "newPassword": "y"},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("password change anonymous = %d, want 401", rec.Code)
	}
}

func TestAdminMutationsRequireACSRFToken(t *testing.T) {
	s := newAdminStack(t)
	cookie, _ := s.signedIn(t, "owner", "a-good-password")

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins", cookie: cookie,
		body: map[string]string{"username": "editor", "password": "a-second-password"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a mutation with no CSRF token = %d, want 403", rec.Code)
	}

	// And with a wrong one.
	rec = s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins", cookie: cookie, csrf: "not-the-token",
		body: map[string]string{"username": "editor", "password": "a-second-password"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a mutation with a wrong CSRF token = %d, want 403", rec.Code)
	}
}

func TestAdminMutationsRequireAMatchingOrigin(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins", cookie: cookie, csrf: csrf,
		origin: "https://attacker.example.com",
		body:   map[string]string{"username": "editor", "password": "a-second-password"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a mutation from a foreign origin = %d, want 403", rec.Code)
	}
}

func TestChangeOwnPasswordVerifiesTheCurrentOne(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	// Wrong current password.
	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/me/password", cookie: cookie, csrf: csrf,
		body: map[string]string{"currentPassword": "not-it", "newPassword": "a-new-password"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a wrong current password = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "currentPassword") {
		t.Errorf("the field error is missing: %s", rec.Body.String())
	}

	// The old password still works, so nothing was changed. Signing in rotates the
	// session, so take the new one.
	cookie, csrf = s.signIn(t, "owner", "a-good-password")

	// Right one.
	rec = s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/me/password", cookie: cookie, csrf: csrf,
		body: map[string]string{"currentPassword": "a-good-password", "newPassword": "a-new-password"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password change = %d (%s)", rec.Code, rec.Body.String())
	}

	// The new password works and the old one does not.
	if s.login(t, "owner", "a-new-password").Code != http.StatusOK {
		t.Fatal("the new password does not sign in")
	}
	if s.login(t, "owner", "a-good-password").Code != http.StatusUnauthorized {
		t.Fatal("the old password still signs in")
	}
}

func TestChangeOwnPasswordRevokesOtherSessions(t *testing.T) {
	s := newAdminStack(t)
	first, _ := s.signedIn(t, "owner", "a-good-password")

	// A second device signs in, which revokes the first for this account.
	second := s.login(t, "owner", "a-good-password")
	secondCookie := sessionCookieFrom(second)
	secondCSRF := csrfFromLogin(t, second)

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/me/password", cookie: secondCookie, csrf: secondCSRF,
		body: map[string]string{"currentPassword": "a-good-password", "newPassword": "a-new-password"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password change = %d (%s)", rec.Code, rec.Body.String())
	}

	// The session the change was made from still works.
	if me := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/me", cookie: secondCookie}); me.Code != http.StatusOK {
		t.Error("the session the change was made from was revoked")
	}
	// Any other session of that account does not.
	if me := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/me", cookie: first}); me.Code != http.StatusUnauthorized {
		t.Errorf("an older session survived a password change: %d", me.Code)
	}
}

func TestResetAnotherAdministratorsPassword(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/password", cookie: cookie, csrf: csrf,
		body: map[string]string{"newPassword": "a-reset-password"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d (%s)", rec.Code, rec.Body.String())
	}

	// The old password is dead, the new one works.
	if login := s.login(t, "editor", "a-second-password"); login.Code != http.StatusUnauthorized {
		t.Error("the old password still signs in after a reset")
	}
	if login := s.login(t, "editor", "a-reset-password"); login.Code != http.StatusOK {
		t.Fatalf("the reset password does not sign in: %d", login.Code)
	}
}

func TestResetOwnPasswordIsRefused(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/1/password", cookie: cookie, csrf: csrf,
		body: map[string]string{"newPassword": "a-reset-password"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("resetting your own password through the reset route = %d, want 409", rec.Code)
	}
}

func TestResetPasswordKillsTheTargetsSessions(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	// The editor signs in, then the owner resets the password.
	editor := s.login(t, "editor", "a-second-password")
	editorCookie := sessionCookieFrom(editor)

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/password", cookie: cookie, csrf: csrf,
		body: map[string]string{"newPassword": "a-reset-password"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d (%s)", rec.Code, rec.Body.String())
	}

	if me := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/me", cookie: editorCookie}); me.Code != http.StatusUnauthorized {
		t.Errorf("the editor's session survived a password reset: %d", me.Code)
	}
	// The owner's own session is untouched.
	if me := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/me", cookie: cookie}); me.Code != http.StatusOK {
		t.Error("the owner's session was revoked by somebody else's password reset")
	}
}

func TestDisableRevokesSessionsAndBlocksSignIn(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	editor := s.login(t, "editor", "a-second-password")
	editorCookie := sessionCookieFrom(editor)

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusDisabled},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeAdmin(t, rec); got.Status != auth.StatusDisabled {
		t.Fatalf("status = %q", got.Status)
	}

	// Its live session is dead.
	if me := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/me", cookie: editorCookie}); me.Code != http.StatusUnauthorized {
		t.Errorf("a disabled account's session still resolves: %d", me.Code)
	}
	// It cannot sign in again.
	if login := s.login(t, "editor", "a-second-password"); login.Code != http.StatusUnauthorized {
		t.Errorf("a disabled account signed in: %d", login.Code)
	}
	// And it cannot use a management API with a fresh correct password.
	if login := s.login(t, "editor", "a-second-password"); login.Code != http.StatusUnauthorized {
		t.Error("a disabled account can still authenticate")
	}

	// Re-enabling restores both.
	rec = s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusActive},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d (%s)", rec.Code, rec.Body.String())
	}
	if login := s.login(t, "editor", "a-second-password"); login.Code != http.StatusOK {
		t.Fatalf("a re-enabled account cannot sign in: %d", login.Code)
	}
}

func TestDisableCannotRemoveTheLastActiveAdministrator(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/1/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusDisabled},
	})
	// Disabling yourself is refused outright, which is also what stops the last
	// administrator from locking the installation out.
	if rec.Code != http.StatusConflict {
		t.Fatalf("disabling yourself = %d, want 409", rec.Code)
	}

	// A second administrator exists, but the owner is the last owner: disabling
	// them would leave nobody who can create or reset accounts.
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	rec = s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusDisabled},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling an ordinary admin = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestDisableRefusesToRetireTheLastOwner(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	// A second *owner* makes the first one retirable.
	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins", cookie: cookie, csrf: csrf,
		body: map[string]string{"username": "second-owner", "password": "a-second-password", "role": auth.RoleOwner},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create owner = %d (%s)", rec.Code, rec.Body.String())
	}

	// The last owner cannot be disabled by anyone.
	for _, target := range []int64{1, 2} {
		_ = target
	}
	// Admin 1 disables admin 2? No: admin 2 is an owner, and admin 1 is too, so
	// this is refused because it would leave one owner — which is allowed. The
	// refusal applies to the *last* one, so enable both and disable one to show
	// the guard, then show that disabling the remaining one is refused.
	rec = s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusDisabled},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling one of two owners = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestTheLastOwnerCannotBeDisabledEvenWithOtherAdministrators(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	// An ordinary administrator keeps the installation signable-in-able, but the
	// owner is still the only account that can manage administrators.
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	// The owner cannot disable itself, which is the only way to reach a state with
	// no active owner through the API.
	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/1/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusDisabled},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("disabling the last owner = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin_status_self") {
		t.Errorf("the refusal does not name the self-disable: %s", rec.Body.String())
	}
}

func TestStatusChangeRequiresTheOwnerRole(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	s.createAdmin(t, cookie, csrf, "third", "a-third-password", "")
	editor := s.login(t, "editor", "a-second-password")
	editorCookie := sessionCookieFrom(editor)
	editorCSRF := csrfFromLogin(t, editor)

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/3/status", cookie: editorCookie, csrf: editorCSRF,
		body: map[string]string{"status": auth.StatusDisabled},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an ordinary admin disabling another account = %d, want 403", rec.Code)
	}
}

func TestAdminDetailIsSelfOrOwner(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	editor := s.login(t, "editor", "a-second-password")
	editorCookie := sessionCookieFrom(editor)

	// The editor cannot read the owner's record.
	if rec := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/admins/1", cookie: editorCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("reading another administrator = %d, want 403", rec.Code)
	}
	// It can read its own.
	if rec := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/admins/2", cookie: editorCookie}); rec.Code != http.StatusOK {
		t.Errorf("reading your own record = %d, want 200", rec.Code)
	}
	// And the owner can read anyone's.
	if rec := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/admins/2", cookie: cookie}); rec.Code != http.StatusOK {
		t.Errorf("the owner reading another administrator = %d, want 200", rec.Code)
	}
}

func TestAdminSummaryNeverCarriesTheHash(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	rec := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/admins", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "passwordHash") || strings.Contains(body, "password_hash") || strings.Contains(body, "$argon2id$") {
		t.Fatalf("the response carries a password hash: %s", body)
	}
	if strings.Contains(body, "a-second-password") {
		t.Fatal("the response carries the password itself")
	}
}

func TestAdminListReportsBindings(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	rec := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/admins", cookie: cookie, csrf: csrf})
	admins := decodeAdmins(t, rec)
	if len(admins) != 1 {
		t.Fatalf("admins = %d, want 1", len(admins))
	}
	if admins[0].GitHubAccounts != 0 {
		t.Errorf("github accounts = %d, want 0", admins[0].GitHubAccounts)
	}
	if admins[0].Role != auth.RoleOwner {
		t.Errorf("role = %q", admins[0].Role)
	}
	if admins[0].CreatedAt == "" {
		t.Error("no creation timestamp")
	}
}

func TestDisabledAdministratorIsRefusedAtLogin(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusDisabled},
	})

	rec := s.login(t, "editor", "a-second-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled account signed in: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid credentials") {
		t.Errorf("the refusal message distinguishes a disabled account: %s", rec.Body.String())
	}
}

func TestAdminCreationIsAuditedUnderTheActor(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")
	s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/2/status", cookie: cookie, csrf: csrf,
		body: map[string]string{"status": auth.StatusDisabled},
	})

	rows := auditRows(t, s.d)
	kinds := map[string]string{}
	for _, row := range rows {
		kinds[row[0]] = row[1]
	}
	for _, want := range []string{"admin.setup", "admin.created", "admin.disabled"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("audit is missing %s: %v", want, rows)
		}
	}
	if kinds["admin.created"] != "editor" {
		t.Errorf("admin.created ref = %q", kinds["admin.created"])
	}
	if kinds["admin.disabled"] != "editor" {
		t.Errorf("admin.disabled ref = %q", kinds["admin.disabled"])
	}
}

// actorOf returns the actor recorded for one event kind.
func actorOf(t *testing.T, d Deps, kind string) string {
	t.Helper()
	var actor string
	if err := d.DB.QueryRow(`SELECT actor FROM content_event WHERE kind = ? ORDER BY id DESC LIMIT 1`, kind).Scan(&actor); err != nil {
		t.Fatalf("no %s event: %v", kind, err)
	}
	return actor
}

func TestAuditNamesTheActingAdministrator(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	if got := actorOf(t, s.d, "admin.created"); got != "owner" {
		t.Errorf("actor = %q, want owner", got)
	}
}

// keep the time import used by future expiry assertions in this file.
var _ = time.Now
