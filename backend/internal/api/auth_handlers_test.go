package api

// The first-run account.
//
// ARCHITECTURE.md ID-71: production has no default username and no default
// password. The backend generates both, returns the plaintext exactly once, and
// never writes it to the database, the log or the audit row. The typed form is a
// development convenience and is refused everywhere else.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"blogcms/internal/auth"
	"blogcms/internal/config"
)

// generatedStack is a stack with no administrator yet, in the given deployment
// mode.
func generatedStack(t *testing.T, deployment string) (*adminTestStack, *bytes.Buffer) {
	t.Helper()
	d := testDeps(t)
	d.Cfg.Deployment = deployment
	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &adminTestStack{d: d, mux: adminMux(d)}, logs
}

func TestSetupGeneratesCredentialsInProduction(t *testing.T) {
	s, logs := generatedStack(t, config.DeploymentProduction)

	// No username and no password: the generated path.
	rec := s.call(t, apiCall{
		method: "POST",
		path:   "/api/v1/auth/setup",
		body:   map[string]string{},
		origin: testOrigin,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d (%s)", rec.Code, rec.Body.String())
	}
	var body setupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Generated {
		t.Fatal("the response does not say the account was generated")
	}

	// A recognisable but non-default username: a fixed prefix a human can
	// recognise, and a random suffix so there is nothing to assume.
	if !regexp.MustCompile(`^owner-[a-zA-Z2-9]{6}$`).MatchString(body.Username) {
		t.Errorf("username = %q; a first-run username must not be a default", body.Username)
	}
	// And a password long enough to survive the API's own policy if the operator
	// hands it anywhere else.
	if len(body.Password) < 20 {
		t.Errorf("password length = %d, want at least 20", len(body.Password))
	}

	// It is a real account, and the stored value is a hash of the returned
	// password rather than the password itself.
	stored, err := s.d.Admins.GetByUsername(context.Background(), body.Username)
	if err != nil {
		t.Fatalf("the generated account does not exist: %v", err)
	}
	if stored.Role != auth.RoleOwner {
		t.Errorf("role = %q, want owner", stored.Role)
	}
	if err := auth.VerifyPassword(body.Password, stored.PasswordHash); err != nil {
		t.Fatal("the generated password does not verify against the stored hash")
	}
	if strings.Contains(stored.PasswordHash, body.Password) {
		t.Fatal("the password is stored in the clear")
	}

	// It signs in, with no GitHub account involved.
	login := s.login(t, body.Username, body.Password)
	if login.Code != http.StatusOK {
		t.Fatalf("the generated credentials sign in = %d (%s)", login.Code, login.Body.String())
	}

	// Nothing wrote the password anywhere: not the log, not the audit row.
	for _, where := range []string{logs.String(), auditPayload(t, s.d)} {
		if strings.Contains(where, body.Password) {
			t.Fatalf("the generated password reached a place it must not: %s", where)
		}
	}

	// Two setups do not produce the same credentials.
	again, _ := generatedStack(t, config.DeploymentProduction)
	second := again.call(t, apiCall{
		method: "POST", path: "/api/v1/auth/setup", body: map[string]string{}, origin: testOrigin,
	})
	var secondBody setupResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if secondBody.Password == body.Password || secondBody.Username == body.Username {
		t.Error("two first runs produced the same credentials")
	}
}

// auditPayload is every audit row as one string, which is what a "did it leak"
// assertion reads.
func auditPayload(t *testing.T, d Deps) string {
	t.Helper()
	var payload string
	if err := d.DB.QueryRow(`SELECT group_concat(kind || '|' || ref || '|' || actor) FROM content_event`).Scan(&payload); err != nil {
		// No audit rows at all is a legitimate state for some of these.
		return ""
	}
	return payload
}

func TestSetupRefusesTypedCredentialsInProduction(t *testing.T) {
	s, _ := generatedStack(t, config.DeploymentProduction)

	rec := s.call(t, apiCall{
		method: "POST",
		path:   "/api/v1/auth/setup",
		body:   map[string]string{"username": "owner", "password": "12345"},
		origin: testOrigin,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a typed setup in production = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "setup_typed_not_allowed") {
		t.Errorf("the refusal does not name the reason: %s", rec.Body.String())
	}

	// And nothing was created, so the generated path is still available.
	if n, err := s.d.Admins.Count(context.Background()); err != nil || n != 0 {
		t.Fatalf("administrators = %d after a refused typed setup", n)
	}
	generate := s.call(t, apiCall{
		method: "POST", path: "/api/v1/auth/setup", body: map[string]string{}, origin: testOrigin,
	})
	if generate.Code != http.StatusCreated {
		t.Fatalf("the generated path after a refusal = %d", generate.Code)
	}
}

func TestSetupStatusReportsWhetherTypingIsAllowed(t *testing.T) {
	production, _ := generatedStack(t, config.DeploymentProduction)
	status := production.call(t, apiCall{method: "GET", path: "/api/v1/auth/setup"})
	var body setupStatusResponse
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.SetupRequired {
		t.Fatal("a fresh stack should report that setup is required")
	}
	if body.TypedSetup {
		t.Error("a production deployment reports that a typed first-run form is allowed")
	}

	development := newAdminStack(t)
	status = development.call(t, apiCall{method: "GET", path: "/api/v1/auth/setup"})
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.TypedSetup {
		t.Error("a development deployment does not report that a typed first-run form is allowed")
	}
}

func TestSetupAcceptsTypedCredentialsInDevelopment(t *testing.T) {
	s := newAdminStack(t)

	// The development convenience: the operator's own account, exactly as typed.
	rec := s.call(t, apiCall{
		method: "POST",
		path:   "/api/v1/auth/setup",
		body:   map[string]string{"username": "owner", "password": "12345"},
		origin: testOrigin,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a typed setup in development = %d (%s)", rec.Code, rec.Body.String())
	}
	var body setupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Generated {
		t.Error("a typed setup reports itself as generated")
	}
	if body.Username != "owner" {
		t.Errorf("username = %q", body.Username)
	}
	if body.Password != "12345" {
		t.Errorf("password = %q; the response should report what was set", body.Password)
	}
	if login := s.login(t, "owner", "12345"); login.Code != http.StatusOK {
		t.Fatalf("the typed credentials sign in = %d", login.Code)
	}
}

func TestSetupIsNotReplayable(t *testing.T) {
	for _, deployment := range []string{config.DeploymentProduction, config.DeploymentDevelopment} {
		t.Run(deployment, func(t *testing.T) {
			d := testDeps(t)
			d.Cfg.Deployment = deployment
			s := &adminTestStack{d: d, mux: adminMux(d)}
			if deployment == config.DeploymentDevelopment {
				s.signedIn(t, "owner", "a-good-password")
			} else {
				// Production has no typed form, so the first run is the generated
				// one and the replay has to be a repeat of that.
				first := s.call(t, apiCall{
					method: "POST", path: "/api/v1/auth/setup",
					body: map[string]string{}, origin: testOrigin,
				})
				if first.Code != http.StatusCreated {
					t.Fatalf("generated setup = %d (%s)", first.Code, first.Body.String())
				}
			}

			again := s.call(t, apiCall{
				method: "POST",
				path:   "/api/v1/auth/setup",
				body:   map[string]string{},
				origin: testOrigin,
			})
			if again.Code != http.StatusConflict {
				t.Fatalf("a second setup = %d, want 409", again.Code)
			}
			if n, err := s.d.Admins.Count(context.Background()); err != nil || n != 1 {
				t.Fatalf("administrators = %d after a replayed setup", n)
			}
		})
	}
}

// --- The site's own origin allowlist ------------------------------------------
//
// ARCHITECTURE.md ID-24. Two properties, and the second is the one that matters:
// a saved origin admits the browser address the operator added, and an *empty*
// allowlist is not an open one.
func TestAllowedOriginsSettingsTakeEffectWithoutARestart(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	// A deployment with nothing configured: the browser address is accepted
	// (same-origin) and a foreign one is not.
	other := originFor(t, testOrigin, "blog.example.com:9900")
	if rec := s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/admins/me/password", cookie: cookie, csrf: csrf,
		origin: other, body: map[string]string{"currentPassword": "a-good-password", "newPassword": "x"},
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("a mutation from a foreign origin with no allowlist = %d, want 403", rec.Code)
	}

	// Save the allowlist through the settings endpoint, which is itself a mutation
	// from the same origin.
	save := s.call(t, apiCall{
		method: "PUT", path: "/api/v1/admin/settings", cookie: cookie, csrf: csrf, origin: testOrigin,
		body: map[string]any{
			"allowedOrigins": []string{other},
			"themeId":        "bluearchive",
		},
	})
	if save.Code != http.StatusOK {
		t.Fatalf("saving the allowlist = %d (%s)", save.Code, save.Body.String())
	}

	// No restart, no re-login: the next request already honours it.
	rec := s.call(t, apiCall{
		method: "GET", path: "/api/v1/admin/me", cookie: cookie, origin: other,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("a read from the saved origin = %d, want 200", rec.Code)
	}
	// It is a mutation that is origin-checked, so that is the real assertion.
	rec = s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/me/password", cookie: cookie, csrf: csrf,
		origin: other, body: map[string]string{"currentPassword": "a-good-password", "newPassword": "x"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a mutation from the saved origin = %d, want it to be accepted (%s)", rec.Code, rec.Body.String())
	}

	// Clearing it puts the deployment back on the same-origin default — not on
	// "allow everything".
	clear := s.call(t, apiCall{
		method: "PUT", path: "/api/v1/admin/settings", cookie: cookie, csrf: csrf, origin: testOrigin,
		body: map[string]any{"allowedOrigins": []string{}, "themeId": "bluearchive"},
	})
	if clear.Code != http.StatusOK {
		t.Fatalf("clearing the allowlist = %d (%s)", clear.Code, clear.Body.String())
	}
	rec = s.call(t, apiCall{
		method: "POST", path: "/api/v1/admin/me/password", cookie: cookie, csrf: csrf,
		origin: other, body: map[string]string{"currentPassword": "a-good-password", "newPassword": "x"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a cleared allowlist accepted a foreign origin: %d", rec.Code)
	}
}

func TestAllowedOriginsRejectsAWildcardThroughTheSettingsAPI(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")

	// The save is refused, which is the only place this can be decided: a row
	// holding `*` would disable the Origin check for every site on the internet.
	rec := s.call(t, apiCall{
		method: "PUT", path: "/api/v1/admin/settings", cookie: cookie, csrf: csrf, origin: testOrigin,
		body: map[string]any{
			"allowedOrigins": []string{"*"},
			"themeId":        "bluearchive",
		},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a wildcard origin = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "allowedOrigins") {
		t.Errorf("the refusal does not name the field: %s", rec.Body.String())
	}

	// And the stored value is unchanged: nothing was written on a refused save.
	var stored string
	if err := s.d.DB.QueryRow(`SELECT count(*) FROM setting WHERE key = 'allowedOrigins'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "0" {
		t.Errorf("a refused save wrote a row anyway: %s", stored)
	}
}

// originFor builds a second absolute origin on the same host shape, so the test
// can present a browser address other than the one the stack serves on.
func originFor(t *testing.T, from, host string) string {
	t.Helper()
	u, err := url.Parse(from)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = host
	u.Path = ""
	return u.String()
}

// A stored allowlist is read by the check that enforces it, in the same shape the
// writer stored it — a JSON array, not the text the screen accepted.
//
// This is the property a text/JSON mismatch would break silently: the save would
// succeed, the screen would show the address, and every mutation from it would be
// refused, which reads as a broken check rather than a malformed row.
func TestAllowedOriginsIsStoredAndReadInOneShape(t *testing.T) {
	s := newAdminStack(t)
	cookie, csrf := s.signedIn(t, "owner", "a-good-password")
	const bound = "http://blog.example.com:9900"

	save := s.call(t, apiCall{
		method: "PUT", path: "/api/v1/admin/settings", cookie: cookie, csrf: csrf, origin: testOrigin,
		body: map[string]any{"allowedOrigins": []string{bound}, "themeId": "bluearchive"},
	})
	if save.Code != http.StatusOK {
		t.Fatalf("saving the allowlist = %d (%s)", save.Code, save.Body.String())
	}
	var stored string
	if err := s.d.DB.QueryRow(
		`SELECT value FROM setting WHERE key = 'allowedOrigins'`).Scan(&stored); err != nil {
		t.Fatalf("the row was not stored: %v", err)
	}
	if !strings.HasPrefix(stored, "[") {
		t.Errorf("the row is not a JSON array: %s", stored)
	}
	if got := DecodeAllowedOrigins(stored); len(got) != 1 || got[0] != bound {
		t.Fatalf("decoded = %v", got)
	}
}
