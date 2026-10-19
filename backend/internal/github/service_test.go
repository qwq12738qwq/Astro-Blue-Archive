package github

// The authorization flow, driven against a local server that stands in for GitHub.
//
// Every case here is one an HTTP suite could not honestly produce without a real
// GitHub account and a real client secret: a missing state, a replayed state, an
// expired state, a failed code exchange, a user GitHub will not name, a GitHub
// account already bound to somebody else, and the same account authorized twice.
// The tests therefore use the same injectable HTTP client the production service
// does, pointed at an httptest server.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"blogcms/internal/store"
)

const (
	testClientID     = "Iv1.testclientid"
	testClientSecret = "test-client-secret-value"
	testCallbackURL  = "https://blog.example.com/api/v1/admin/github/callback"
	testKeyFile      = "test-credential-encryption-key-0123456789abcdef"
	testAccessToken  = "ghu_test-access-token-value"
	testRefreshToken = "ghr_test-refresh-token-value"
)

// mockGitHub is a stand-in for GitHub's authorize, token and user endpoints.
type mockGitHub struct {
	server *httptest.Server

	// userID is the immutable identity the user endpoint reports.
	userID int64
	// login is the username the user endpoint reports. Changing it between two
	// calls is how "a GitHub username changed" is simulated.
	login string
	// name and avatarURL are the display fields.
	name      string
	avatarURL string

	// expiresIn is the token lifetime in seconds; 0 means "no expiry field".
	expiresIn int

	// tokenStatus and tokenError control the token endpoint's answer.
	tokenStatus int
	tokenError  string

	// userStatus controls the user endpoint's status.
	userStatus int

	// tokenCalls counts how many times the exchange endpoint was reached, which
	// is how a replayed callback is shown to be refused before the exchange.
	tokenCalls atomic.Int64

	// lastCodeVerifier records what the exchange sent, so the test can prove the
	// PKCE verifier travelled and that the browser never saw it.
	lastCodeVerifier atomic.Value
}

func newMockGitHub(t *testing.T, userID int64, login string) *mockGitHub {
	t.Helper()
	m := &mockGitHub{userID: userID, login: login, name: "Test User", avatarURL: "https://example.test/avatar.png", tokenStatus: http.StatusOK, userStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		// The authorize endpoint is only ever requested by a browser; recording
		// that the flow built a valid URL is enough.
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("authorize"))
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		m.tokenCalls.Add(1)
		_ = r.ParseForm()
		m.lastCodeVerifier.Store(r.Form.Get("code_verifier"))

		if m.tokenStatus != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(m.tokenStatus)
			_ = json.NewEncoder(w).Encode(TokenError{Error: m.tokenError, Description: "refused by the stand-in"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"access_token": testAccessToken, "token_type": "bearer"}
		if testRefreshToken != "" {
			body["refresh_token"] = testRefreshToken
		}
		if m.expiresIn > 0 {
			body["expires_in"] = m.expiresIn
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAccessToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if m.userStatus != http.StatusOK {
			w.WriteHeader(m.userStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(User{ID: m.userID, Login: m.login, Name: m.name, AvatarURL: m.avatarURL})
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// testService builds a service bound to a temporary database and a mock GitHub.
func testService(t *testing.T, m *mockGitHub) (*Service, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "blog.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	keyring, err := NewKeyring([]byte(testKeyFile))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	svc, err := New(Options{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		CallbackURL:  testCallbackURL,
		WebBase:      m.server.URL,
		APIBase:      m.server.URL,
		StateTTL:     10 * time.Minute,
		Store:        db,
		Keyring:      keyring,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, db
}

// insertAdmin creates an administrator row directly: the flow binds to an account
// id, and the account itself is the auth package's business.
func insertAdmin(t *testing.T, db *store.DB, username string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO admin_user (username, password_hash, role, status, created_at, updated_at)
		 VALUES (?, 'x', 'admin', 'active', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		username)
	if err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func TestStartBuildsAnAuthorizeURLWithPKCE(t *testing.T) {
	m := newMockGitHub(t, 101, "octo")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, err := svc.Start(context.Background(), adminID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	u, err := url.Parse(authorize)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if u.Host != strings.TrimPrefix(m.server.URL, "http://") {
		t.Fatalf("authorize url points at %q, want the configured web base", authorize)
	}
	q := u.Query()
	if q.Get("client_id") != testClientID {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != testCallbackURL {
		t.Errorf("redirect_uri = %q; the callback must be the configured one", q.Get("redirect_uri"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" {
		t.Error("no code challenge: PKCE is not being used")
	}
	if strings.Contains(authorize, "scope=") {
		t.Error("the identity binding must request no scopes: the public profile is all it needs (ID-64)")
	}

	state := q.Get("state")
	if state == "" {
		t.Fatal("no state in the authorize url")
	}
	// The state must have been persisted against this administrator.
	record, err := db.GetOAuthState(context.Background(), state)
	if err != nil {
		t.Fatalf("the state was not stored: %v", err)
	}
	if record.AdminID != adminID {
		t.Fatalf("state admin = %d, want %d", record.AdminID, adminID)
	}
	if record.CodeVerifier == "" {
		t.Error("no PKCE verifier stored")
	}
}

func TestCallbackBindsTheAuthorizedAccount(t *testing.T) {
	m := newMockGitHub(t, 202, "octo-user")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, err := svc.Start(context.Background(), adminID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	state := mustQuery(t, authorize).Get("state")

	result, err := svc.Callback(context.Background(), state, "the-authorization-code")
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if result.AdminID != adminID {
		t.Fatalf("result admin = %d, want %d", result.AdminID, adminID)
	}
	if result.Login != "octo-user" {
		t.Errorf("login = %q", result.Login)
	}
	if result.GitHubUserID != 202 {
		t.Errorf("github user id = %d, want 202", result.GitHubUserID)
	}

	// The identity exists, and the credential is sealed rather than stored raw.
	identity, err := db.FindGitHubIdentityByUserID(context.Background(), 202)
	if err != nil || identity == nil {
		t.Fatalf("identity not found: %v", err)
	}
	if identity.AdminID != adminID {
		t.Errorf("identity admin = %d, want %d", identity.AdminID, adminID)
	}
	credential, err := db.GetGitHubCredential(context.Background(), identity.ID)
	if err != nil {
		t.Fatalf("credential not found: %v", err)
	}
	if strings.Contains(credential.AccessTokenCT, testAccessToken) {
		t.Error("the access token is stored in plaintext")
	}
	if !strings.Contains(credential.RefreshTokenCT.String, testRefreshToken) == false {
		// The refresh token is sealed too: its ciphertext must not equal it.
		if credential.RefreshTokenCT.String == testRefreshToken {
			t.Error("the refresh token is stored in plaintext")
		}
	}

	// The verifier was sent to the token endpoint, proving the exchange is
	// server-to-server and completed.
	if got, _ := m.lastCodeVerifier.Load().(string); got == "" {
		t.Error("the PKCE verifier was not sent to the token endpoint")
	}
}

func TestCallbackRefusesAMissingState(t *testing.T) {
	m := newMockGitHub(t, 1, "octo")
	svc, _ := testService(t, m)

	if _, err := svc.Callback(context.Background(), "", "code"); err == nil {
		t.Fatal("an empty state must be refused")
	}
	if _, err := svc.Callback(context.Background(), "not-a-state-we-issued", "code"); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("an unknown state must be refused as invalid, got %v", err)
	}
	if m.tokenCalls.Load() != 0 {
		t.Error("the code was exchanged despite an invalid state")
	}
}

func TestCallbackRefusesAReplayedState(t *testing.T) {
	m := newMockGitHub(t, 303, "octo")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, _ := svc.Start(context.Background(), adminID)
	state := mustQuery(t, authorize).Get("state")

	if _, err := svc.Callback(context.Background(), state, "code-1"); err != nil {
		t.Fatalf("first callback: %v", err)
	}
	if _, err := svc.Callback(context.Background(), state, "code-2"); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("a replayed state must be refused, got %v", err)
	}
	if m.tokenCalls.Load() != 1 {
		t.Errorf("token calls = %d, want 1: the replay reached the exchange", m.tokenCalls.Load())
	}

	// And it must not have produced a second binding or a second credential.
	identities, err := db.ListGitHubIdentitiesByAdmin(context.Background(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 {
		t.Fatalf("bindings = %d, want 1", len(identities))
	}
}

func TestCallbackRefusesAnExpiredState(t *testing.T) {
	m := newMockGitHub(t, 404, "octo")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, _ := svc.Start(context.Background(), adminID)
	state := mustQuery(t, authorize).Get("state")

	// Expire the transaction the way time would.
	if _, err := db.Exec(`UPDATE oauth_state SET expires_at = '2020-01-01T00:00:00Z' WHERE state = ?`, state); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Callback(context.Background(), state, "code"); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("an expired state must be refused, got %v", err)
	}
}

func TestCallbackBindsToTheAdministratorTheStateNames(t *testing.T) {
	m := newMockGitHub(t, 505, "octo")
	svc, db := testService(t, m)
	ownerID := insertAdmin(t, db, "owner")
	otherID := insertAdmin(t, db, "other")

	// The owner starts a transaction; the callback must bind it to the owner,
	// never to whoever happens to be signed in later.
	authorize, _ := svc.Start(context.Background(), ownerID)
	state := mustQuery(t, authorize).Get("state")
	if _, err := svc.Callback(context.Background(), state, "code"); err != nil {
		t.Fatalf("Callback: %v", err)
	}

	identity, err := db.FindGitHubIdentityByUserID(context.Background(), 505)
	if err != nil || identity == nil {
		t.Fatalf("identity not found: %v", err)
	}
	if identity.AdminID != ownerID {
		t.Fatalf("bound to admin %d, want %d", identity.AdminID, ownerID)
	}
	for _, id := range []int64{otherID, ownerID} {
		list, err := db.ListGitHubIdentitiesByAdmin(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if id == otherID && len(list) != 0 {
			t.Fatalf("the other administrator received a binding")
		}
	}
}

func TestCallbackRefusesAGitHubAccountBoundToAnotherAdministrator(t *testing.T) {
	m := newMockGitHub(t, 606, "shared-account")
	svc, db := testService(t, m)
	ownerID := insertAdmin(t, db, "owner")
	otherID := insertAdmin(t, db, "other")

	first, _ := svc.Start(context.Background(), ownerID)
	if _, err := svc.Callback(context.Background(), mustQuery(t, first).Get("state"), "code-1"); err != nil {
		t.Fatalf("first bind: %v", err)
	}

	// A second administrator authorizes the *same* GitHub account.
	second, _ := svc.Start(context.Background(), otherID)
	_, err := svc.Callback(context.Background(), mustQuery(t, second).Get("state"), "code-2")
	if !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("a second administrator binding the same account must be refused, got %v", err)
	}

	identity, _ := db.FindGitHubIdentityByUserID(context.Background(), 606)
	if identity.AdminID != ownerID {
		t.Fatalf("the binding moved to admin %d", identity.AdminID)
	}
}

func TestCallbackIsIdempotentForTheSameAccount(t *testing.T) {
	m := newMockGitHub(t, 707, "octo")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	first, _ := svc.Start(context.Background(), adminID)
	if _, err := svc.Callback(context.Background(), mustQuery(t, first).Get("state"), "code-1"); err != nil {
		t.Fatalf("first bind: %v", err)
	}

	// The administrator re-authorizes. This is not a second binding: it refreshes
	// the display fields and replaces the credential.
	m.login = "octo-renamed"
	second, _ := svc.Start(context.Background(), adminID)
	result, err := svc.Callback(context.Background(), mustQuery(t, second).Get("state"), "code-2")
	if err != nil {
		t.Fatalf("second bind: %v", err)
	}
	if result.IdentityID == 0 {
		t.Fatal("no identity id in the result")
	}

	identities, err := db.ListGitHubIdentitiesByAdmin(context.Background(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 {
		t.Fatalf("bindings = %d, want 1: a repeat authorization duplicated the binding", len(identities))
	}
	if identities[0].Login != "octo-renamed" {
		t.Errorf("login = %q, want the refreshed one", identities[0].Login)
	}
	// And the immutable id is untouched, which is what the binding is keyed on.
	if identities[0].GitHubUserID != 707 {
		t.Errorf("github user id changed to %d", identities[0].GitHubUserID)
	}
}

func TestCallbackRefusesAFailedExchange(t *testing.T) {
	m := newMockGitHub(t, 808, "octo")
	m.tokenStatus = http.StatusUnauthorized
	m.tokenError = "bad_verification_code"
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, _ := svc.Start(context.Background(), adminID)
	if _, err := svc.Callback(context.Background(), mustQuery(t, authorize).Get("state"), "a-wrong-code"); !errors.Is(err, ErrTokenExchangeFailed) {
		t.Fatalf("a refused exchange must fail the flow, got %v", err)
	}

	identities, _ := db.ListGitHubIdentitiesByAdmin(context.Background(), adminID)
	if len(identities) != 0 {
		t.Fatal("a binding was created despite a failed exchange")
	}
}

func TestCallbackRefusesWhenGitHubWillNotNameTheUser(t *testing.T) {
	m := newMockGitHub(t, 909, "octo")
	m.userStatus = http.StatusForbidden
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, _ := svc.Start(context.Background(), adminID)
	if _, err := svc.Callback(context.Background(), mustQuery(t, authorize).Get("state"), "code"); !errors.Is(err, ErrUserFetchFailed) {
		t.Fatalf("an unidentified user must fail the flow, got %v", err)
	}

	identities, _ := db.ListGitHubIdentitiesByAdmin(context.Background(), adminID)
	if len(identities) != 0 {
		t.Fatal("a binding was created for a user GitHub would not name")
	}
}

func TestCallbackRefusesAMissingCode(t *testing.T) {
	m := newMockGitHub(t, 1010, "octo")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, _ := svc.Start(context.Background(), adminID)
	state := mustQuery(t, authorize).Get("state")
	if _, err := svc.Callback(context.Background(), state, ""); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("a missing code must be refused, got %v", err)
	}
	// The state must not have been consumed by a refused callback: the
	// administrator can retry the same transaction only if GitHub lets them.
	record, err := db.GetOAuthState(context.Background(), state)
	if err != nil {
		t.Fatalf("the state is gone: %v", err)
	}
	if record.ConsumedAt.Valid {
		t.Error("a refused callback consumed the state")
	}
}

func TestUnbindRemovesTheCredentialAndKeepsTheAdministrator(t *testing.T) {
	m := newMockGitHub(t, 1111, "octo")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, _ := svc.Start(context.Background(), adminID)
	result, err := svc.Callback(context.Background(), mustQuery(t, authorize).Get("state"), "code")
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if err := svc.Unbind(context.Background(), result.IdentityID); err != nil {
		t.Fatalf("Unbind: %v", err)
	}

	if _, err := db.GetGitHubIdentity(context.Background(), result.IdentityID); !errors.Is(err, store.ErrNotFound) {
		t.Error("the identity survived an unbind")
	}
	if _, err := db.GetGitHubCredential(context.Background(), result.IdentityID); !errors.Is(err, store.ErrNotFound) {
		t.Error("the credential survived an unbind")
	}
	// The administrator still exists: an unbind must not remove an account.
	var username string
	if err := db.QueryRow(`SELECT username FROM admin_user WHERE id = ?`, adminID).Scan(&username); err != nil {
		t.Errorf("the administrator was removed by an unbind: %v", err)
	} else if username != "owner" {
		t.Errorf("username = %q", username)
	}
}

func TestUnconfiguredServiceRefusesToBind(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "blog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Options{Store: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.Enabled() {
		t.Fatal("an unconfigured service reports itself as enabled")
	}
	if _, err := svc.Start(context.Background(), 1); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Start on an unconfigured service = %v", err)
	}
	if _, err := svc.Callback(context.Background(), "state", "code"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Callback on an unconfigured service = %v", err)
	}
}

// A configured binding with no keyring is a state, not a construction error
// (ID-73): the key file was lost and the rest of the site keeps running. What the
// service refuses is *starting* another binding, and that is asserted in
// TestCallbackAndStartRefuseWithoutAKeyring.
func TestNewAcceptsAConfiguredServiceWithoutAKeyring(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "blog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Options{Store: db, ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatalf("a deployment that lost its key must still construct: %v", err)
	}
	if !svc.Enabled() {
		t.Fatal("the service should still report itself as enabled: the OAuth credentials are there")
	}
	if _, err := svc.Start(context.Background(), 1); !errors.Is(err, ErrCredentialKeyUnavailable) {
		t.Fatalf("Start without a key = %v, want ErrCredentialKeyUnavailable", err)
	}
}

// mustQuery parses a URL and returns its query, failing the test otherwise.
func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

func TestExpiryIsRecorded(t *testing.T) {
	m := newMockGitHub(t, 1212, "octo")
	m.expiresIn = 3600
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	authorize, _ := svc.Start(context.Background(), adminID)
	result, err := svc.Callback(context.Background(), mustQuery(t, authorize).Get("state"), "code")
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if result.ExpiresAt == "" {
		t.Fatal("no expiry recorded although GitHub said the token expires")
	}
	if _, err := time.Parse(time.RFC3339, result.ExpiresAt); err != nil {
		t.Fatalf("expiry %q is not a timestamp: %v", result.ExpiresAt, err)
	}
}

func TestPurgeStatesRemovesDeadTransactions(t *testing.T) {
	m := newMockGitHub(t, 1313, "octo")
	svc, db := testService(t, m)
	adminID := insertAdmin(t, db, "owner")

	if _, err := db.Exec(
		`INSERT INTO oauth_state (state, admin_id, code_verifier, created_at, expires_at, consumed_at)
		 VALUES ('dead', ?, 'v', '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z', NULL)`, adminID); err != nil {
		t.Fatal(err)
	}
	n, err := svc.PurgeStates(context.Background())
	if err != nil {
		t.Fatalf("PurgeStates: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1", n)
	}
}

func TestCallbackRefusesAStateWhoseAdministratorIsGone(t *testing.T) {
	m := newMockGitHub(t, 1414, "octo")
	db, err := store.Open(filepath.Join(t.TempDir(), "blog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	adminID := insertAdmin(t, db, "owner")

	keyring, err := NewKeyring([]byte(testKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		CallbackURL:  testCallbackURL,
		WebBase:      m.server.URL,
		APIBase:      m.server.URL,
		StateTTL:     10 * time.Minute,
		Store:        db,
		Keyring:      keyring,
		// The administrator is gone by the time the callback lands.
		AdminExists: func(context.Context, int64) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	authorize, err := svc.Start(context.Background(), adminID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := svc.Callback(context.Background(), mustQuery(t, authorize).Get("state"), "code"); !errors.Is(err, ErrAdminMissing) {
		t.Fatalf("a state whose administrator is gone must be refused, got %v", err)
	}

	// Nothing was bound, and the code was never exchanged.
	if m.tokenCalls.Load() != 0 {
		t.Error("the code was exchanged for an account that no longer exists")
	}
	identities, err := db.ListGitHubIdentitiesByAdmin(context.Background(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 0 {
		t.Fatalf("bindings = %d, want 0", len(identities))
	}
}
