package api

// GitHub account binding.
//
// The one endpoint that is *not* behind requireSession is the OAuth callback, and
// that is a property of the protocol rather than a hole in it: the browser is sent
// here by GitHub, so it arrives with no session cookie (the cookie is SameSite=Strict,
// which is why a cross-site navigation does not carry it) and with a `state` that
// names the transaction. Everything the callback accepts is bound to that state row,
// which was created by an authenticated administrator and is consumed exactly once.
// Skipping the state check because "this is not a form post" is precisely the
// mistake this design does not make.
//
// Nothing here echoes a token, a client secret or a GitHub response body back to a
// browser. The most an API response says about a credential is that one exists, what
// it may do, and when it expires.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"blogcms/internal/auth"
	"blogcms/internal/github"
	"blogcms/internal/httpx"
	"blogcms/internal/store"
)

// githubConfigResponse tells the admin screen whether binding can be attempted at
// all. `Configured` is false when the deployment set no credentials, and every
// other CMS feature keeps working in that state.
type githubConfigResponse struct {
	Configured       bool   `json:"configured"`
	ClientID         string `json:"clientId,omitempty"`
	CallbackURL      string `json:"callbackUrl,omitempty"`
	WebBase          string `json:"webBase,omitempty"`
	APIBase          string `json:"apiBase,omitempty"`
	HasClientSecret  bool   `json:"hasClientSecret"`
}

// githubBindingItem is one bound account, reduced to what a screen may show.
//
// There is no token here and no way to derive one: no `accessToken`, no
// `refreshToken`, no ciphertext. A browser that inspected the whole response would
// learn the account's identity and its expiry, and nothing else.
type githubBindingItem struct {
	ID           int64  `json:"id"`
	Login        string `json:"login"`
	DisplayName  string `json:"displayName,omitempty"`
	AvatarURL    string `json:"avatarUrl,omitempty"`
	GitHubUserID int64  `json:"githubUserId"`
	AdminID      int64  `json:"adminId"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	// HasCredential is the only claim made about the credential.
	HasCredential bool `json:"hasCredential"`
	// CredentialUnavailable is true when the deployment has no credential
	// encryption key (ID-73). The row is there and the account is bound, but its
	// credential can never be opened — which is the state that has to be visible,
	// because every other part of the binding looks perfectly healthy.
	CredentialUnavailable bool   `json:"credentialUnavailable"`
	ExpiresAt             string `json:"expiresAt,omitempty"`
	// Scope is the granted permission summary, never a token.
	Scope string `json:"scope,omitempty"`
	// Expired is derived from ExpiresAt and the current time, so a screen can
	// show "re-authorize" without computing anything itself.
	Expired bool `json:"expired"`
}

type githubBindingsResponse struct {
	Bindings []githubBindingItem `json:"bindings"`
}

type githubBindStartRequest struct {
	// AdminID lets an owner start a binding on another administrator's behalf. It
	// is validated, not trusted: an ordinary admin naming someone else's id gets a
	// refusal rather than a different account.
	AdminID int64 `json:"adminId"`
}

type githubBindStartResponse struct {
	AuthorizeURL string `json:"authorizeUrl"`
}

type githubUnbindResponse struct {
	OK bool `json:"ok"`
}

// githubResultBase is where the callback lands the browser: the detail page of the
// administrator the transaction was started for, plus `/` and that administrator's
// integer id. It is an Astro page, because Go must not produce HTML (ARCHITECTURE.md
// §1). Binding is not a screen of its own — a binding belongs to an administrator,
// and this is that administrator's page.
const githubResultBase = "/admin/admins"

func registerGitHubRoutes(mux *http.ServeMux, d Deps) {
	// Every authenticated route. The session, the Origin check and the CSRF
	// double-submit header all apply.
	mux.HandleFunc("GET /api/v1/admin/github/config", d.requireSession(d.getGitHubConfig))
	mux.HandleFunc("PUT /api/v1/admin/github/config", d.requireSession(d.putGitHubConfig))
	mux.HandleFunc("GET /api/v1/admin/github/identities", d.requireSession(d.listGitHubBindings))
	mux.HandleFunc("POST /api/v1/admin/github/bind/start", d.requireSession(d.startGitHubBind))
	mux.HandleFunc("POST /api/v1/admin/github/identities/{id}/unbind", d.requireSession(d.unbindGitHub))
	// The callback is the one exception, and it is validated by its transaction
	// rather than by a session: see the file comment.
	mux.HandleFunc("GET /api/v1/admin/github/callback", d.githubCallback)
}

func (d Deps) getGitHubConfig(w http.ResponseWriter, r *http.Request) {
	resp := githubConfigResponse{Configured: d.GitHub.Enabled()}
	cfg := d.GitHub.ReadConfig()
	resp.ClientID = cfg.ClientID
	resp.CallbackURL = cfg.CallbackURL
	resp.WebBase = cfg.WebBase
	resp.APIBase = cfg.APIBase
	resp.HasClientSecret = cfg.ClientSecret != ""
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// bindingTarget resolves which administrator a request is about.
//
// `self or owner` is the whole rule: an administrator always reaches its own
// bindings, and an owner reaches anyone's. It is resolved here so every binding
// endpoint answers the same question the same way.
func (d Deps) bindingTarget(w http.ResponseWriter, r *http.Request, requested int64) (int64, bool) {
	session := SessionFrom(r)
	if requested == 0 || requested == session.AdminID {
		return session.AdminID, true
	}
	if session.Role != auth.RoleOwner {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
			"the owner role is required to manage another administrator's GitHub bindings"))
		return 0, false
	}
	return requested, true
}

func (d Deps) listGitHubBindings(w http.ResponseWriter, r *http.Request) {
	adminID, ok := d.bindingTarget(w, r, queryID(r, "adminId"))
	if !ok {
		return
	}
	identities, err := d.DB.ListGitHubIdentitiesByAdmin(r.Context(), adminID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	out := make([]githubBindingItem, 0, len(identities))
	for _, identity := range identities {
		item, err := d.toBindingItem(r.Context(), identity)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		out = append(out, item)
	}
	httpx.WriteJSON(w, http.StatusOK, githubBindingsResponse{Bindings: out})
}

// toBindingItem reduces one binding plus its credential summary to the public
// shape.
//
// The credential's *existence* is read here and the credential itself is not: the
// sealed blob never leaves this function, and what crosses the boundary is a
// boolean, a scope string and an expiry.
func (d Deps) toBindingItem(ctx context.Context, identity store.GitHubIdentityRecord) (githubBindingItem, error) {
	item := githubBindingItem{
		ID:           identity.ID,
		Login:        identity.Login,
		DisplayName:  identity.Name,
		AvatarURL:    identity.AvatarURL,
		GitHubUserID: identity.GitHubUserID,
		AdminID:      identity.AdminID,
		CreatedAt:    identity.CreatedAt,
		UpdatedAt:    identity.UpdatedAt,
	}
	credential, err := d.DB.GetGitHubCredential(ctx, identity.ID)
	if errors.Is(err, store.ErrNotFound) {
		// A binding with no credential is a legitimate state: it says "this GitHub
		// account is recognised" and "this CMS may not act with it yet".
		return item, nil
	}
	if err != nil {
		return item, err
	}
	item.HasCredential = credential.AccessTokenCT != ""
	// Only meaningful next to a credential: "the key is missing" is a reason a
	// credential cannot be used, not a fact about a binding that never had one.
	item.CredentialUnavailable = item.HasCredential && !d.GitHub.CredentialKeyAvailable()
	item.Scope = credential.Scope
	if credential.ExpiresAt.Valid {
		item.ExpiresAt = credential.ExpiresAt.String
		if expiresAt, parseErr := time.Parse(time.RFC3339, item.ExpiresAt); parseErr == nil {
			item.Expired = time.Now().UTC().After(expiresAt)
		}
	}
	return item, nil
}

func (d Deps) startGitHubBind(w http.ResponseWriter, r *http.Request) {
	if !d.GitHub.Enabled() {
		githubError(w, r, github.ErrNotConfigured)
		return
	}

	var req githubBindStartRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	adminID, ok := d.bindingTarget(w, r, req.AdminID)
	if !ok {
		return
	}

	// The target must exist and be able to sign in. Binding a credential to a
	// disabled account would produce a row nobody can use, and whose owner cannot
	// be asked about it.
	target, err := d.Admins.Get(r.Context(), adminID)
	if err != nil {
		adminError(w, r, err)
		return
	}
	if target.Status != auth.StatusActive {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "admin_disabled",
			"enable the account before binding a GitHub account to it"))
		return
	}

	authorizeURL, err := d.GitHub.Start(r.Context(), adminID)
	if err != nil {
		githubError(w, r, err)
		return
	}
	d.auditAs(r, "github_binding.started", target.Username)
	httpx.WriteJSON(w, http.StatusOK, githubBindStartResponse{AuthorizeURL: authorizeURL})
}

// githubCallback completes an authorization transaction.
//
// The result is a redirect to an Astro page rather than a body: Go returns JSON and
// never HTML (ARCHITECTURE.md §1), and a redirect is not a body. The landing page
// then reads the bindings through the API like every other screen. The redirect
// target is assembled from the configured callback origin and the transaction's own
// administrator — both server-owned — so it can never be steered by a query
// parameter.
func (d Deps) githubCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")

	// Read the transaction before it is consumed: the state row is the only
	// server-owned statement of which administrator this callback is about, and
	// after a cancellation or a failure the row may be gone. An id of 0 means the
	// state named no transaction we issued, and the browser lands on the
	// administrators screen instead.
	adminID := int64(0)
	if record, recErr := d.DB.GetOAuthState(r.Context(), state); recErr == nil {
		adminID = record.AdminID
	}

	// A cancelled or denied authorization is a normal outcome, not an error, and
	// it still consumes the transaction so it cannot be replayed.
	if q.Get("error") != "" {
		_ = d.consumeCancelledState(r.Context(), state)
		d.redirectToResult(w, r, adminID, "cancelled", "")
		return
	}

	result, err := d.GitHub.Callback(r.Context(), state, code)
	if err != nil {
		// The audit names the administrator the state was for, read above, rather
		// than anything taken from the request.
		d.auditBindFailure(r, err, state)
		d.redirectToResult(w, r, adminID, "error", "github_error")
		return
	}

	// Name the actor the state was bound to, not the caller: the callback has no
	// session, so the transaction is the only statement about who authorized.
	if target, getErr := d.Admins.Get(r.Context(), result.AdminID); getErr == nil {
		d.auditFor(r.Context(), "github_binding.created", target.Username, target.Username)
	} else {
		d.auditFor(r.Context(), "github_binding.created", strconv.FormatInt(result.AdminID, 10), result.Login)
	}
	d.redirectToResult(w, r, result.AdminID, "ok", "")
}

// unbindGitHub removes one binding and its credential.
func (d Deps) unbindGitHub(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"id must be a positive integer"))
		return
	}

	identity, err := d.DB.GetGitHubIdentity(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
				"no such GitHub binding"))
			return
		}
		httpx.WriteError(w, r, err)
		return
	}
	if _, ok := d.bindingTarget(w, r, identity.AdminID); !ok {
		return
	}

	if err := d.GitHub.Unbind(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
				"no such GitHub binding"))
			return
		}
		githubError(w, r, err)
		return
	}

	// The audit reference is the GitHub account's *id*, not its username: a
	// username can change on GitHub's side, and the immutable id is the fact that
	// was unbound.
	ref := strconv.FormatInt(identity.GitHubUserID, 10)
	actor := "admin"
	if target, getErr := d.Admins.Get(r.Context(), identity.AdminID); getErr == nil {
		actor = target.Username
	}
	d.auditFor(r.Context(), "github_binding.deleted", ref, actor)
	httpx.WriteJSON(w, http.StatusOK, githubUnbindResponse{OK: true})
}

// consumeCancelledState marks a cancelled transaction used, so the state cannot be
// reused after the administrator pressed "cancel" at GitHub.
func (d Deps) consumeCancelledState(ctx context.Context, state string) error {
	if state == "" {
		return nil
	}
	_, err := d.DB.ConsumeOAuthState(ctx, state, time.Now().UTC().Format(time.RFC3339))
	return err
}

// auditBindFailure records a failed authorization without recording anything
// about it that is not a decision.
//
// Two rules keep this honest. It never writes a token: the error string can
// carry a GitHub response body, so only the *classification* is stored. And it
// only fires for a transaction that actually existed — a callback presenting a
// state nobody issued is noise an unauthenticated endpoint should not be able
// to grow a table with.
func (d Deps) auditBindFailure(r *http.Request, err error, state string) {
	reason := "github_error"
	switch {
	case errors.Is(err, github.ErrStateInvalid):
		reason = "github_state_invalid"
	case errors.Is(err, github.ErrIdentityTaken):
		reason = "github_identity_taken"
	}
	record, getErr := d.DB.GetOAuthState(r.Context(), state)
	if getErr != nil {
		// Unknown state: no transaction to attribute the failure to, and no
		// row worth writing.
		return
	}
	// The actor is the administrator the transaction was for — an account
	// name, never the state, which is a secret.
	actor := "github-callback"
	if admin, adminErr := d.Admins.Get(r.Context(), record.AdminID); adminErr == nil {
		actor = admin.Username
	}
	d.auditFor(r.Context(), "github_binding.failed", reason, actor)
}

// redirectToResult sends the browser to the Astro result page: the detail screen of
// the administrator the transaction was for.
//
// `adminID` is the transaction's own administrator and never a request parameter;
// 0 means the callback named a transaction we did not issue, and the browser goes to
// the administrators screen. `result` and `reason` are machine codes the page maps
// onto words; neither ever carries a token, a path or a GitHub response body.
func (d Deps) redirectToResult(w http.ResponseWriter, r *http.Request, adminID int64, result, reason string) {
	origin, err := callbackOrigin(d.Cfg.GitHubCallbackURL)
	if err != nil {
		// Without a configured callback origin there is nowhere to land the
		// browser that this deployment did not declare. JSON is the only other
		// shape this process may produce.
		httpx.WriteError(w, r, httpx.Errorf(http.StatusInternalServerError, httpx.CodeInternal,
			"the authorization finished but the callback address is not configured"))
		return
	}
	target := origin + githubResultBase
	if adminID > 0 {
		target += "/" + strconv.FormatInt(adminID, 10)
	}
	target += "?result=" + url.QueryEscape(result)
	if reason != "" {
		target += "&reason=" + url.QueryEscape(reason)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// callbackOrigin reduces the configured callback URL to scheme://host:port. A path,
// query or fragment would be a second, unconfigured place to redirect to.
func callbackOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("callback url is not an absolute origin")
	}
	return u.Scheme + "://" + u.Host, nil
}

// githubError maps binding failures onto the uniform envelope.
//
// The messages are deliberately generic about GitHub: an exchange failure says what
// step failed and nothing about what GitHub answered, because that answer can echo
// the client id, the redirect URI or a token.
func githubError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, github.ErrNotConfigured):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusServiceUnavailable, "github_not_configured",
			"GitHub account binding is not configured on this server"))
	case errors.Is(err, github.ErrStateInvalid):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, "github_state_invalid",
			"this authorization is no longer valid; start it again from the admin"))
	case errors.Is(err, github.ErrCredentialKeyUnavailable):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusServiceUnavailable, "github_key_unavailable",
			"this deployment has no credential encryption key, so a GitHub account cannot be bound"))
	case errors.Is(err, github.ErrAdminMissing):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "github_admin_missing",
			"the administrator this authorization was started for is no longer active"))
	case errors.Is(err, github.ErrIdentityTaken):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "github_identity_taken",
			"this GitHub account is already bound to another administrator"))
	case errors.Is(err, github.ErrTokenExchangeFailed):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadGateway, "github_token_exchange_failed",
			"GitHub did not return a token for this authorization"))
	case errors.Is(err, github.ErrUserFetchFailed):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadGateway, "github_identity_failed",
			"GitHub did not confirm which account authorized this"))
	default:
		httpx.WriteError(w, r, err)
	}
}

// queryID reads a numeric query parameter, returning 0 when it is absent or
// unparseable. An unparseable value is treated as absent rather than as an error so
// that a screen defaulting to "my own bindings" cannot be broken by a stray
// parameter.
func queryID(r *http.Request, name string) int64 {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}
