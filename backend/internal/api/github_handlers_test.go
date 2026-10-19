package api

// GitHub account binding, over the real router.
//
// The interesting assertions are about what a browser is *not* allowed to learn and
// what a callback is *not* allowed to accept: the responses carry no token, the
// callback without a state cannot bind, a cancelled authorization lands somewhere
// honest, and unbinding removes the credential without touching the administrator or
// the local backup repository.
//
// GitHub is a local server. No test here reaches the public internet, holds a real
// client secret, or uses a real account.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"blogcms/internal/config"
	"blogcms/internal/github"
)

const (
	testGitHubUserID  = int64(4242)
	testGitHubLogin   = "octo-admin"
	testGitHubToken   = "ghu_api-level-test-token"
	testGitHubKeyFile = "test-credential-encryption-key-0123456789abcdef"
	// The callback must be an absolute origin for the redirect target to be built.
	testGitHubCallback = "http://example.com/api/v1/admin/github/callback"
)

// mockGitHub is the local stand-in for GitHub's endpoints.
type mockGitHub struct {
	server *httptest.Server
	// authorizePath records what the authorize URL contained.
	lastAuthorize atomicString
	tokenCalls    atomicInt
	verifier      atomicString
	// userStatus lets a test refuse to name the user.
	userStatus int
}

type atomicInt struct{ n int }

func (a *atomicInt) add()     { a.n++ }
func (a *atomicInt) get() int { return a.n }

type atomicString struct{ v string }

func (a *atomicString) set(s string) { a.v = s }

func newMockGitHub(t *testing.T) *mockGitHub {
	t.Helper()
	m := &mockGitHub{userStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		m.lastAuthorize.set(r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("authorize"))
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		m.tokenCalls.add()
		_ = r.ParseForm()
		m.verifier.set(r.Form.Get("code_verifier"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  testGitHubToken,
			"refresh_token": "ghr_refresh-value",
			"token_type":    "bearer",
			"scope":         "read:user",
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testGitHubToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if m.userStatus != http.StatusOK {
			w.WriteHeader(m.userStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         testGitHubUserID,
			"login":      testGitHubLogin,
			"name":       "Octo Admin",
			"avatar_url": "https://example.test/octo.png",
		})
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// githubTestStack is an admin stack whose GitHub binding is configured against the
// local stand-in.
type githubTestStack struct {
	*adminTestStack
	mock *mockGitHub
}

func newGitHubStack(t *testing.T) *githubTestStack {
	t.Helper()
	mock := newMockGitHub(t)
	d := testDeps(t)
	// A development deployment, so the typed first-run sign-in below is the one
	// that works; the production first-run shape is asserted in auth_handlers_test.go.
	d.Cfg.Deployment = config.DeploymentDevelopment
	d.Cfg.GitHubEnabled = true
	d.Cfg.GitHubClientID = "Iv1.testclientid"
	d.Cfg.GitHubClientSecret = "test-client-secret"
	d.Cfg.GitHubCallbackURL = testGitHubCallback
	d.Cfg.GitHubWebBase = mock.server.URL
	d.Cfg.GitHubAPIBase = mock.server.URL
	d.Cfg.GitHubOAuthStateTTL = 10 * time.Minute

	keyring, err := github.NewKeyring([]byte(testGitHubKeyFile))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	svc, err := github.New(github.Options{
		ClientID:     d.Cfg.GitHubClientID,
		ClientSecret: d.Cfg.GitHubClientSecret,
		CallbackURL:  d.Cfg.GitHubCallbackURL,
		WebBase:      mock.server.URL,
		APIBase:      mock.server.URL,
		StateTTL:     d.Cfg.GitHubOAuthStateTTL,
		Store:        d.DB,
		Keyring:      keyring,
	})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	d.GitHub = svc
	return &githubTestStack{adminTestStack: &adminTestStack{d: d, mux: adminMux(d)}, mock: mock}
}

// complete performs a start and then drives the callback the way GitHub would.
func (s *githubTestStack) complete(t *testing.T, cookie, csrf string) (authorizeURL, state string) {
	t.Helper()
	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/github/bind/start", cookie: cookie, csrf: csrf,
		body: map[string]any{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bind start = %d (%s)", rec.Code, rec.Body.String())
	}
	var body githubBindStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.AuthorizeURL, authorizeQuery(t, body.AuthorizeURL).Get("state")
}

func TestGitHubConfigReportsWhetherItIsConfigured(t *testing.T) {
	// Unconfigured: the default testDeps stack has no GitHub service, so the
	// endpoint must say so and every other CMS feature keeps working.
	plain := newAdminStack(t)
	cookie, _ := plain.signedIn(t, "owner", "a-good-password")
	rec := plain.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/config", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("config = %d", rec.Code)
	}
	var cfg githubConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Configured {
		t.Fatal("an unconfigured deployment reports itself as configured")
	}

	// Configured: the screen can then say what to register with GitHub.
	s := newGitHubStack(t)
	cookie, _ = s.signedIn(t, "owner", "a-good-password")
	rec = s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/config", cookie: cookie})
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Configured {
		t.Fatal("a configured deployment reports itself as unconfigured")
	}
	if cfg.CallbackURL != testGitHubCallback {
		t.Errorf("callback url = %q", cfg.CallbackURL)
	}
}

func TestBindStartRefusesWhenGitHubIsNotConfigured(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/github/bind/start", cookie: cookie, csrf: csrf,
		body: map[string]any{},
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bind start without configuration = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "github_not_configured") {
		t.Errorf("code = %s", rec.Body.String())
	}
}

func TestBindStartBuildsTheAuthorizeURL(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	authorize, state := s.complete(t, cookie, csrf)
	if !strings.HasPrefix(authorize, s.mock.server.URL) {
		t.Fatalf("authorize url = %q, want the configured web base", authorize)
	}
	q := authorizeQuery(t, authorize)
	if q.Get("state") != state {
		t.Errorf("the authorize url did not carry the state that was stored")
	}
	if q.Get("code_challenge") == "" {
		t.Error("no PKCE code challenge in the authorize url")
	}
	if q.Get("redirect_uri") != testGitHubCallback {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}

	// The audit records that a binding was started, under the acting username.
	if got := actorOf(t, s.d, "github_binding.started"); got != "owner" {
		t.Errorf("actor = %q, want owner", got)
	}
}

func TestCallbackBindsAndRedirectsToTheResultPage(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	_, state := s.complete(t, cookie, csrf)

	rec := s.call(t, apiCall{
		method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code",
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d (%s)", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	landing, parseErr := url.Parse(location)
	if parseErr != nil || landing.Scheme != "http" || landing.Host != "example.com" {
		t.Fatalf("redirect target = %q; it must stay on the configured origin", location)
	}
	// The landing page is the administrator the transaction was for: the state
	// row's id, never a request value.
	ownerID := queryOne(t, s.d, "SELECT CAST(id AS TEXT) FROM admin_user WHERE username = ?", "owner")
	if landing.Path != "/admin/admins/"+ownerID {
		t.Fatalf("redirect target = %q; want the detail page of administrator %s", location, ownerID)
	}
	if landing.Query().Get("result") != "ok" {
		t.Errorf("the redirect does not say it worked: %q", location)
	}

	// The binding exists and is listed.
	list := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/identities", cookie: cookie})
	if list.Code != http.StatusOK {
		t.Fatalf("identities = %d (%s)", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if !strings.Contains(body, testGitHubLogin) {
		t.Errorf("the binding does not name the account: %s", body)
	}
	if !strings.Contains(body, "4242") {
		t.Errorf("the binding does not name the immutable id: %s", body)
	}
	// And it carries no credential.
	for _, forbidden := range []string{"accessToken", "refreshToken", testGitHubToken, "ghu_", "ghr_", "access_token"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the response carries a credential: %s contains %q", body, forbidden)
		}
	}

	// The credential in the database is sealed, not raw.
	if got := queryOne(t, s.d, `SELECT access_token_ct FROM github_credential`); strings.Contains(got, testGitHubToken) {
		t.Error("the access token is stored in plaintext")
	}

	if got := actorOf(t, s.d, "github_binding.created"); got != "owner" {
		t.Errorf("audit actor = %q, want owner", got)
	}
}

func TestCallbackRefusesAMissingOrUnknownState(t *testing.T) {
	s := newGitHubStack(t)
	s.signedIn(t, "owner", "a-good-password")

	for _, target := range []string{
		"/api/v1/admin/github/callback",
		"/api/v1/admin/github/callback?state=never-issued&code=the-code",
		"/api/v1/admin/github/callback?state=&code=the-code",
	} {
		rec := s.call(t, apiCall{method: "GET", path: target})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s = %d, want a redirect to the result page", target, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Location"), "result=error") {
			t.Errorf("%s did not report a failure: %q", target, rec.Header().Get("Location"))
		}
	}
	if s.mock.tokenCalls.get() != 0 {
		t.Error("the code was exchanged despite an invalid state")
	}
	if got := queryOne(t, s.d, `SELECT count(*) FROM github_identity`); got != "0" {
		t.Errorf("bindings = %s, want 0", got)
	}
}

func TestCallbackIsNotReplayable(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	_, state := s.complete(t, cookie, csrf)

	first := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code"})
	if first.Code != http.StatusSeeOther || !strings.Contains(first.Header().Get("Location"), "result=ok") {
		t.Fatalf("first callback = %d %q", first.Code, first.Header().Get("Location"))
	}

	second := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code"})
	if !strings.Contains(second.Header().Get("Location"), "result=error") {
		t.Errorf("a replayed callback succeeded: %q", second.Header().Get("Location"))
	}
	if got := queryOne(t, s.d, `SELECT count(*) FROM github_identity`); got != "1" {
		t.Errorf("bindings = %s, want 1", got)
	}
}

func TestCallbackHandlesACancelledAuthorization(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	_, state := s.complete(t, cookie, csrf)

	rec := s.call(t, apiCall{
		method: "GET",
		path:   "/api/v1/admin/github/callback?state=" + state + "&error=access_denied&error_description=the+user+cancelled",
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "result=cancelled") {
		t.Errorf("a cancelled authorization is not reported as one: %q", rec.Header().Get("Location"))
	}
	if got := queryOne(t, s.d, `SELECT count(*) FROM github_identity`); got != "0" {
		t.Errorf("bindings = %s, want 0", got)
	}
	// The transaction is consumed, so it cannot be completed afterwards.
	replay := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code"})
	if !strings.Contains(replay.Header().Get("Location"), "result=error") {
		t.Error("a cancelled state was still usable")
	}
}

func TestUnbindRemovesTheCredentialAndKeepsTheAccount(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	_, state := s.complete(t, cookie, csrf)
	s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code"})

	// Find the binding id.
	list := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/identities", cookie: cookie})
	var body githubBindingsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Bindings) != 1 {
		t.Fatalf("bindings = %d", len(body.Bindings))
	}
	id := body.Bindings[0].ID

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/github/identities/" + itoa(id) + "/unbind",
		cookie: cookie, csrf: csrf,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind = %d (%s)", rec.Code, rec.Body.String())
	}

	if got := queryOne(t, s.d, `SELECT count(*) FROM github_identity`); got != "0" {
		t.Errorf("the identity survived: %s", got)
	}
	if got := queryOne(t, s.d, `SELECT count(*) FROM github_credential`); got != "0" {
		t.Errorf("the credential survived: %s", got)
	}
	if got := queryOne(t, s.d, `SELECT username FROM admin_user WHERE id = 1`); got != "owner" {
		t.Errorf("the administrator was removed: %q", got)
	}
	if got := actorOf(t, s.d, "github_binding.deleted"); got != "owner" {
		t.Errorf("audit actor = %q", got)
	}
}

func TestUnbindRefusesAnotherAdministratorsBinding(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	// The owner binds their own account, then hands the editor's session a request
	// to unbind it.
	_, state := s.complete(t, cookie, csrf)
	s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code"})
	list := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/identities", cookie: cookie})
	var body githubBindingsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	id := body.Bindings[0].ID

	editor := s.login(t, "editor", "a-second-password")
	editorCookie := sessionCookieFrom(editor)
	editorCSRF := csrfFromLogin(t, editor)

	rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/github/identities/" + itoa(id) + "/unbind",
		cookie: editorCookie, csrf: editorCSRF,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an ordinary admin unbinding someone else's account = %d, want 403", rec.Code)
	}
	if got := queryOne(t, s.d, `SELECT count(*) FROM github_identity`); got != "1" {
		t.Errorf("the binding was removed anyway: %s", got)
	}
}

func TestIdentitiesDefaultToTheCallersOwnBindings(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	s.createAdmin(t, cookie, csrf, "editor", "a-second-password", "")

	_, state := s.complete(t, cookie, csrf)
	s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code"})

	editor := s.login(t, "editor", "a-second-password")
	editorCookie := sessionCookieFrom(editor)

	// The editor sees none of the owner's bindings.
	rec := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/identities", cookie: editorCookie})
	var body githubBindingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Bindings) != 0 {
		t.Fatalf("the editor saw %d bindings", len(body.Bindings))
	}

	// The owner sees one.
	rec = s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/identities", cookie: cookie})
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Bindings) != 1 {
		t.Fatalf("the owner saw %d bindings", len(body.Bindings))
	}
	// And the binding reports that a credential exists without carrying it.
	if !body.Bindings[0].HasCredential {
		t.Error("a bound account reports no credential")
	}
}

func TestBindingsAreAuditedWithoutAnyCredential(t *testing.T) {
	s := newGitHubStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	_, state := s.complete(t, cookie, csrf)
	s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/callback?state=" + state + "&code=the-code"})

	var payload string
	if err := s.d.DB.QueryRow(`SELECT group_concat(kind || '|' || ref || '|' || actor) FROM content_event`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload == "" {
		t.Fatal("no audit rows at all")
	}
	for _, forbidden := range []string{testGitHubToken, "ghu_", "ghr_", testGitHubKeyFile, "the-code", state} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("the audit log leaked %q: %s", forbidden, payload)
		}
	}
}

// --- helpers ------------------------------------------------------------------

// authorizeQuery parses an authorize URL and returns its query.
func authorizeQuery(t *testing.T, rawURL string) url.Values {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Query()
}

// mockQuery parses the raw query the mock GitHub recorded.
func mockQuery(rawQuery string) url.Values {
	v, err := url.ParseQuery(rawQuery)
	if err != nil {
		return url.Values{}
	}
	return v
}

func queryOne(t *testing.T, d Deps, query string, args ...any) string {
	t.Helper()
	var out string
	if err := d.DB.QueryRow(query, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// --- The credential key is missing (ARCHITECTURE.md ID-73) --------------------
//
// A deployment whose key file was lost after a restore must refuse to *create* a
// credential rather than generate a new key and orphan every binding. That is a
// state the backend reaches, so it is asserted at the API layer: the bind is
// refused with a code, and reading the bindings still works, because the site
// itself must not stop functioning over a missing key file.
func TestBindingRefusesWhenTheCredentialKeyIsMissing(t *testing.T) {
	m := newMockGitHub(t)
	d := testDeps(t)
	d.Cfg.Deployment = config.DeploymentDevelopment
	d.Cfg.GitHubEnabled = true
	d.Cfg.GitHubClientID = "Iv1.testclientid"
	d.Cfg.GitHubClientSecret = "test-client-secret"
	d.Cfg.GitHubCallbackURL = testGitHubCallback
	d.Cfg.GitHubWebBase = m.server.URL
	d.Cfg.GitHubAPIBase = m.server.URL
	d.Cfg.GitHubOAuthStateTTL = 10 * time.Minute

	// A configured service with no keyring — exactly what main.go builds when the
	// key file is gone, because it logs the error and carries on rather than
	// refusing to start.
	svc, err := github.New(github.Options{
		ClientID:     d.Cfg.GitHubClientID,
		ClientSecret: d.Cfg.GitHubClientSecret,
		CallbackURL:  d.Cfg.GitHubCallbackURL,
		WebBase:      m.server.URL,
		APIBase:      m.server.URL,
		StateTTL:     d.Cfg.GitHubOAuthStateTTL,
		Store:        d.DB,
		Keyring:      nil,
	})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	if !svc.Enabled() {
		t.Fatal("a configured service reports itself as disabled")
	}
	d.GitHub = svc

	s := &adminTestStack{d: d, mux: adminMux(d)}
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	bind := s.call(t, apiCall{
		method: "POST",
		path:   "/api/v1/admin/github/bind/start",
		cookie: cookie,
		csrf:   csrf,
		body:   map[string]any{},
	})
	if bind.Code != http.StatusServiceUnavailable {
		t.Fatalf("starting a binding without the credential key = %d, want 503 (%s)", bind.Code, bind.Body.String())
	}
	bound := bind.Body.String()
	if !strings.Contains(bound, "github_not_configured") && !strings.Contains(bound, "credential") {
		t.Errorf("the refusal does not say what is missing: %s", bound)
	}

	// The screen still works: a deployment answer about its own configuration.
	cfg := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/config", cookie: cookie})
	if cfg.Code != http.StatusOK {
		t.Errorf("the screen still answers = %d, want 200", cfg.Code)
	}

	// And a binding that exists reports its credential as unusable rather than as
	// "authorised", because a row that looks healthy while its credential cannot
	// be opened is the symptom this rule exists to make visible (ID-73).
	res, err := s.d.DB.Exec(`INSERT INTO github_identity (admin_id, github_user_id, login, created_at, updated_at)
		VALUES (1, 42, 'octo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	identityID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := githubTestKeyringSeal(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.DB.Exec(`INSERT INTO github_credential (identity_id, access_token_ct, token_type, created_at, updated_at)
		VALUES (?, ?, 'bearer', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, identityID, sealed); err != nil {
		t.Fatal(err)
	}

	list := s.call(t, apiCall{method: "GET", path: "/api/v1/admin/github/identities", cookie: cookie})
	var payload githubBindingsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(payload.Bindings))
	}
	if !payload.Bindings[0].HasCredential {
		t.Error("the binding should report that a credential row exists")
	}
	if !payload.Bindings[0].CredentialUnavailable {
		t.Error("a deployment with no key must report the credential as unusable")
	}
}

// githubTestKeyring builds a keyring for a test that needs a sealed row without
// sharing the deployment's key.
func githubTestKeyring() (*github.Keyring, error) {
	return github.NewKeyring([]byte(testGitHubKeyFile))
}

// githubTestKeyringSeal seals a placeholder credential for such a row.
func githubTestKeyringSeal(t *testing.T) (string, error) {
	t.Helper()
	keyring, err := githubTestKeyring()
	if err != nil {
		return "", err
	}
	return keyring.Seal("ghu-a-credential-the-database-cannot-open")
}
