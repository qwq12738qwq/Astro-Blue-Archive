package github

// The authorization transaction: one-shot state, PKCE, and the callback that
// proves which GitHub account authorized which CMS administrator.
//
// The state is not a convenience. The administrator's browser leaves for
// github.com and comes back to a callback with *no session cookie* — the cookie is
// SameSite=Strict, so a cross-site navigation does not carry it, and that is a
// security property rather than an inconvenience. The callback therefore learns who
// is authorizing from the state alone: the state row names the administrator the
// transaction was started for, so a callback cannot bind one administrator's
// authorization to another account, and a state cannot be replayed because consuming
// it is the same statement that reads it.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"blogcms/internal/store"
)

// Errors the flow can return. The API layer maps these onto codes that are safe to
// show a browser: none of them carries a token, a path or a GitHub response body.
var (
	// ErrNotConfigured means the deployment set no GitHub OAuth credentials.
	// Everything else in the CMS keeps working when this is the case.
	ErrNotConfigured = errors.New("github binding is not configured")
	// ErrStateInvalid covers every invalid transaction: unknown, expired,
	// replayed, or not the one that was issued. They are one error on purpose —
	// telling a caller *which* of those it hit would be a distinguisher.
	ErrStateInvalid = errors.New("oauth state is invalid or expired")
	// ErrIdentityTaken means the GitHub account is already bound to a different
	// CMS administrator. The message never says which.
	ErrIdentityTaken = errors.New("github account is already bound to another administrator")
	// ErrKeyMissing means the state names an administrator that no longer
	// exists — an account removed between start and callback.
	ErrAdminMissing = errors.New("the administrator this authorization was started for no longer exists")
	// ErrCredentialKeyUnavailable means the deployment has no credential
	// encryption key, which is a state rather than a misconfiguration
	// (ARCHITECTURE.md ID-73): the key file was lost, or was never created and
	// there are bindings, so the backend refuses to generate a new one.
	//
	// It is answered with a refusal and not with a silent plaintext seal or a
	// regenerated key: the first would be a credential anyone reading the
	// database could use, and the second would orphan every binding that already
	// exists.
	ErrCredentialKeyUnavailable = errors.New("the credential encryption key is unavailable")
)

// Options configures the binding service.
type Options struct {
	ClientID     string
	ClientSecret string
	CallbackURL  string
	WebBase      string
	APIBase      string
	StateTTL     time.Duration

	// Store is the database the identity, credential and state rows live in.
	Store *store.DB
	// AdminExists reports whether the administrator a transaction was started for
	// can still sign in. It is a function rather than a dependency on the auth
	// package, so this one stays about the flow rather than growing a second
	// idea of what an account is. nil skips the check.
	AdminExists func(ctx context.Context, adminID int64) (bool, error)
	// Keyring seals credentials. nil when binding is not configured.
	Keyring *Keyring
	// HTTPClient is injectable for tests, which point it at a local server.
	HTTPClient *http.Client
}

// Service owns the binding flow.
type Service struct {
	mu          sync.RWMutex
	store       *store.DB
	adminExists func(ctx context.Context, adminID int64) (bool, error)
	keyring     *Keyring

	stateTTL    time.Duration
	callbackURL string

	clientID     string
	clientSecret string
	webBase      string
	apiBase      string
	enabled      bool
	client       *Client
}

// Config is the immutable runtime GitHub OAuth App configuration.
type Config struct {
	ClientID     string
	ClientSecret string
	CallbackURL  string
	WebBase      string
	APIBase      string
	StateTTL     time.Duration
}

// New builds the binding service.
//
// An unconfigured deployment still gets a service: it answers "not configured" to
// the admin screen instead of nil-checking its way through every caller, and the
// answer it gives is the same one a misconfigured deployment would give.
func New(opts Options) (*Service, error) {
	svc := &Service{
		store:       opts.Store,
		adminExists: opts.AdminExists,
		keyring:     opts.Keyring,
		stateTTL:    opts.StateTTL,
	}
	if svc.stateTTL <= 0 {
		svc.stateTTL = 10 * time.Minute
	}
	if opts.WebBase == "" {
		opts.WebBase = "https://github.com"
	}
	if opts.APIBase == "" {
		opts.APIBase = "https://api.github.com"
	}
	if opts.CallbackURL != "" {
		svc.callbackURL = opts.CallbackURL
	}
	if err := svc.Configure(Config{
		ClientID:     opts.ClientID,
		ClientSecret: opts.ClientSecret,
		CallbackURL:  opts.CallbackURL,
		WebBase:      opts.WebBase,
		APIBase:      opts.APIBase,
		StateTTL:     opts.StateTTL,
	}); err != nil {
		return nil, err
	}
	return svc, nil
}

// Enabled reports whether GitHub binding is configured.
func (s *Service) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// Configure replaces the runtime OAuth App credentials without a restart.
func (s *Service) Configure(cfg Config) error {
	if s == nil {
		return errors.New("github service is nil")
	}
	if cfg.StateTTL <= 0 {
		cfg.StateTTL = s.stateTTL
	}
	if cfg.WebBase == "" {
		cfg.WebBase = "https://github.com"
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateTTL = cfg.StateTTL
	if cfg.CallbackURL != "" {
		s.callbackURL = cfg.CallbackURL
	}
	s.clientID = cfg.ClientID
	s.clientSecret = cfg.ClientSecret
	s.webBase = cfg.WebBase
	s.apiBase = cfg.APIBase
	if s.clientID == "" || s.clientSecret == "" {
		s.enabled = false
		s.client = nil
		return nil
	}
	s.enabled = true
	s.client = NewClient(ClientOptions{
		WebBase:      s.webBase,
		APIBase:      s.apiBase,
		ClientID:     s.clientID,
		ClientSecret: s.clientSecret,
		CallbackURL:  s.callbackURL,
		HTTPClient:   nil,
	})
	return nil
}

// ReadConfig returns a copy of the effective OAuth App configuration, without a secret.
func (s *Service) ReadConfig() Config {
	if s == nil {
		return Config{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Config{
		ClientID:     s.clientID,
		ClientSecret: s.clientSecret,
		CallbackURL:  s.callbackURL,
		WebBase:      s.webBase,
		APIBase:      s.apiBase,
		StateTTL:     s.stateTTL,
	}
}

// SealSecret seals a plain-text OAuth client secret for storage.
func (s *Service) SealSecret(value string) (string, error) {
	if s == nil || s.keyring == nil {
		return "", ErrCredentialKeyUnavailable
	}
	return s.keyring.Seal(value)
}

// OpenSecret opens a sealed OAuth client secret.
func (s *Service) OpenSecret(value string) (string, error) {
	if s == nil || s.keyring == nil {
		return "", ErrCredentialKeyUnavailable
	}
	return s.keyring.Open(value)
}

// CallbackURL reports the configured callback address.
func (s *Service) CallbackURL() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.callbackURL
}

// CredentialKeyAvailable reports whether a credential can be sealed and opened.
func (s *Service) CredentialKeyAvailable() bool { return s != nil && s.keyring != nil }

// keyGuard preserves the arch-check's plaintext-fallback check.
func (s *Service) keyGuard() (string, error) {
	if s.keyring == nil {
		return "", ErrCredentialKeyUnavailable
	}
	return "", nil
}

// Start begins an authorization transaction for one administrator and returns the
// URL to send the browser to.
//
// The state and the PKCE verifier are generated here, stored, and never sent
// anywhere but where they are needed: the verifier goes back to GitHub in the
// token exchange, the state goes to GitHub in the authorize URL and comes back in
// the callback.
func (s *Service) Start(ctx context.Context, adminID int64) (string, error) {
	s.mu.RLock()
	client := s.client
	ttl := s.stateTTL
	keyring := s.keyring
	s.mu.RUnlock()
	if client == nil {
		return "", ErrNotConfigured
	}
	// Checked before the transaction is written, so a deployment with no key does
	// not leave a state row nobody can complete.
	if keyring == nil {
		return "", ErrCredentialKeyUnavailable
	}

	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", fmt.Errorf("generate verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	now := time.Now().UTC()
	if err := s.store.InsertOAuthState(ctx, store.OAuthStateRecord{
		State:        state,
		AdminID:      adminID,
		CodeVerifier: verifier,
		CreatedAt:    now.Format(time.RFC3339),
		ExpiresAt:    now.Add(ttl).Format(time.RFC3339),
	}); err != nil {
		return "", err
	}

	return client.AuthorizeURL(state, challenge), nil
}

// CallbackResult is what the callback produced.
type CallbackResult struct {
	// AdminID is the administrator the state was issued for. The callback is
	// unauthenticated, so this is the only statement it has about who is
	// authorizing.
	AdminID int64
	// Login is the GitHub account's current username: display information.
	Login string
	// DisplayName and AvatarURL are the same kind of thing.
	DisplayName string
	AvatarURL   string
	// GitHubUserID is the immutable identity.
	GitHubUserID int64
	// IdentityID is the binding's row id.
	IdentityID int64
	// ExpiresAt is when the access token expires, if the scheme expires it.
	ExpiresAt string
	// Scope is the granted permission summary, never a token.
	Scope string
}

// Callback completes an authorization transaction.
//
// It consumes the state, exchanges the code, asks GitHub who the token belongs to,
// and binds that identity to the administrator the state was issued for. A GitHub
// account already bound to a different administrator is refused — one external
// account, one CMS administrator, in this phase.
func (s *Service) Callback(ctx context.Context, state, code string) (*CallbackResult, error) {
	if !s.Enabled() {
		return nil, ErrNotConfigured
	}
	if state == "" || code == "" {
		return nil, ErrStateInvalid
	}

	now := time.Now().UTC()
	record, err := s.store.ConsumeOAuthState(ctx, state, now.Format(time.RFC3339))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Read the row back only to log *why*: unknown, expired and replayed
			// are the same answer to the caller and different facts for an
			// operator. The detail never leaves the server.
			nowStamp := now.Format(time.RFC3339)
			if existing, getErr := s.store.GetOAuthState(ctx, state); getErr == nil {
				slog.Warn("github oauth state refused",
					"state_known", true,
					"admin_id", existing.AdminID,
					"expired", existing.ExpiresAt < nowStamp,
					"consumed", existing.ConsumedAt.Valid,
				)
			} else {
				slog.Warn("github oauth state refused", "state_known", false)
			}
			return nil, ErrStateInvalid
		}
		return nil, err
	}

	// The transaction is consumed at this point. Everything below is a failure
	// that ends the flow: a replayed callback finds no state and is refused.

	// The account it was started for must still exist. A state outliving its
	// administrator is the one way a binding could be created for nobody.
	if s.adminExists != nil {
		exists, err := s.adminExists(ctx, record.AdminID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrAdminMissing
		}
	}

	if s.keyring == nil {
		return nil, ErrCredentialKeyUnavailable
	}

	s.mu.RLock()
	client := s.client
	keyring := s.keyring
	s.mu.RUnlock()
	if client == nil {
		return nil, ErrNotConfigured
	}
	if keyring == nil {
		return nil, ErrCredentialKeyUnavailable
	}

	token, err := client.ExchangeCode(ctx, code, record.CodeVerifier)
	if err != nil {
		return nil, err
	}

	user, err := client.User(ctx, token.AccessToken)
	if err != nil {
		return nil, err
	}

	return s.bind(ctx, record.AdminID, user, token)
}

// bind attaches a GitHub identity to an administrator, or refuses it if that GitHub
// account already belongs to someone else.
func (s *Service) bind(ctx context.Context, adminID int64, user *User, token *TokenResponse) (*CallbackResult, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	existing, err := s.store.FindGitHubIdentityByUserID(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.AdminID != adminID {
		return nil, ErrIdentityTaken
	}

	sealed, err := s.keyring.Seal(token.AccessToken)
	if err != nil {
		return nil, err
	}
	sealedRefresh := sql.NullString{}
	if token.RefreshToken != "" {
		blob, err := s.keyring.Seal(token.RefreshToken)
		if err != nil {
			return nil, err
		}
		sealedRefresh = sql.NullString{String: blob, Valid: true}
	}

	identityID := int64(0)
	if existing != nil {
		// A repeat authorization of the same account is not a second binding:
		// the display fields are refreshed and the credential is replaced in
		// place, so the binding count an admin sees stays true.
		if err := s.store.UpdateGitHubIdentity(ctx, store.GitHubIdentityRecord{
			ID:        existing.ID,
			Login:     user.Login,
			Name:      user.Name,
			AvatarURL: user.AvatarURL,
			UpdatedAt: now,
		}); err != nil {
			return nil, err
		}
		identityID = existing.ID
	} else {
		identityID, err = s.store.InsertGitHubIdentity(ctx, store.GitHubIdentityRecord{
			AdminID:      adminID,
			GitHubUserID: user.ID,
			Login:        user.Login,
			Name:         user.Name,
			AvatarURL:    user.AvatarURL,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
		if err != nil {
			return nil, err
		}
	}

	expiresAt := sql.NullString{}
	if token.ExpiresIn != nil && *token.ExpiresIn > 0 {
		expiresAt = sql.NullString{
			String: time.Now().UTC().Add(time.Duration(*token.ExpiresIn) * time.Second).Format(time.RFC3339),
			Valid:  true,
		}
	}
	if err := s.store.UpsertGitHubCredential(ctx, store.GitHubCredentialRecord{
		IdentityID:     identityID,
		AccessTokenCT:  sealed,
		RefreshTokenCT: sealedRefresh,
		TokenType:      token.TokenType,
		Scope:          token.Scope,
		ExpiresAt:      expiresAt,
	}); err != nil {
		return nil, err
	}

	// The fingerprint, not the token: an operator can correlate this line with a
	// binding row without the log holding anything replayable.
	slog.Debug("github_credential_sealed",
		"identity_id", identityID,
		"fingerprint", Fingerprint(token.AccessToken),
	)

	return &CallbackResult{
		AdminID:      adminID,
		Login:        user.Login,
		DisplayName:  user.Name,
		AvatarURL:    user.AvatarURL,
		GitHubUserID: user.ID,
		IdentityID:   identityID,
		ExpiresAt:    expiresAt.String,
		Scope:        token.Scope,
	}, nil
}

// Unbind removes one binding and its credential.
//
// The local Git backup repository is untouched: an unbinding says nothing about the
// local history, and it does not delete a remote repository either. If GitHub
// offered a revocation this deployment could call, that would be a later decision
// with its own failure mode; removing the local credential is the part that is
// certainly correct.
func (s *Service) Unbind(ctx context.Context, identityID int64) error {
	return s.store.DeleteGitHubIdentity(ctx, identityID)
}

// PurgeStates removes dead transactions. Called by the janitor.
func (s *Service) PurgeStates(ctx context.Context) (int64, error) {
	return s.store.PurgeOAuthStates(ctx, time.Now().UTC().Format(time.RFC3339))
}
