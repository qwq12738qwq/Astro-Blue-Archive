package github

// The GitHub side of the binding: the HTTP client that talks to GitHub's
// authorize, token and user endpoints.
//
// It is an interface on purpose. Every property this package claims about the flow
// — a missing state, a replayed state, a failed code exchange, a user id that is
// already bound — has to be provable without a real GitHub account, a real client
// secret or the public internet. Tests substitute a local server that answers with
// whatever GitHub would, or with whatever GitHub would never do.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Errors the GitHub client can return. They are mapped onto safe, generic codes at
// the API boundary: nothing here reaches the browser, because every one of them can
// carry a body that came from GitHub.
var (
	ErrTokenExchangeFailed = errors.New("github token exchange failed")
	ErrUserFetchFailed     = errors.New("github user lookup failed")
)

// Client talks to GitHub.
type Client struct {
	webBase string
	apiBase string

	clientID     string
	clientSecret string
	callbackURL  string

	http *http.Client
}

// ClientOptions configures the GitHub client. WebBase and APIBase are absolute
// origins with no path.
type ClientOptions struct {
	WebBase      string
	APIBase      string
	ClientID     string
	ClientSecret string
	CallbackURL  string
	// HTTPClient is injectable so tests can point it at a local server with a
	// transport of their choosing.
	HTTPClient *http.Client
}

// NewClient builds a GitHub client.
func NewClient(opts ClientOptions) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		// A timeout, because a GitHub that stops answering must not hang an
		// administrator's browser tab forever.
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		webBase:      strings.TrimRight(opts.WebBase, "/"),
		apiBase:      strings.TrimRight(opts.APIBase, "/"),
		clientID:     opts.ClientID,
		clientSecret: opts.ClientSecret,
		callbackURL:  opts.CallbackURL,
		http:         hc,
	}
}

// AuthorizeURL builds the URL the administrator's browser is sent to.
//
// The PKCE code challenge travels here, so the verifier never leaves the server:
// only its SHA-256 is on the wire, and GitHub holds the browser to what it was
// given. `state` is the caller's one-shot transaction id.
//
// There is deliberately no `scope` parameter. The binding stores the public
// profile `/user` returns — the immutable id, the login, the name and the avatar
// URL — and a token with no scopes can read it. The only scope this screen could
// ever justify is `user:email`, and the binding stores no email, so it is not
// asked for "just in case": a scope is added when a phase needs what it unlocks
// (ARCHITECTURE.md ID-64, ID-76).
func (c *Client) AuthorizeURL(state, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", c.callbackURL)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return c.webBase + "/login/oauth/authorize?" + q.Encode()
}

// TokenResponse is the token endpoint's answer.
//
// `ExpiresIn` is absent for a scheme without expiring tokens (an OAuth App), which
// is why it is a pointer: "does not expire" and "expires in zero seconds" are
// different facts and must not collapse into one.
type TokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"`
	ExpiresIn             *int   `json:"expires_in"`
	RefreshTokenExpiresIn *int   `json:"refresh_token_expires_in"`
}

// TokenError is GitHub's error body for a failed exchange.
type TokenError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// ExchangeCode trades an authorization code for a user access token.
//
// The code is single-use by GitHub's own rule, and the exchange is a server-to-server
// POST that carries the client secret: the browser never sees this request, and the
// secret is never in a URL, a header a browser sends, or a log line.
func (c *Client) ExchangeCode(ctx context.Context, code, codeVerifier string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", c.callbackURL)
	form.Set("grant_type", "authorization_code")
	form.Set("code_verifier", codeVerifier)

	res, err := c.postForm(ctx, c.webBase+"/login/oauth/access_token", form)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrTokenExchangeFailed, err)
	}
	if res.StatusCode != http.StatusOK {
		var tokenErr TokenError
		_ = json.Unmarshal(body, &tokenErr)
		return nil, fmt.Errorf("%w: status %d code %q", ErrTokenExchangeFailed, res.StatusCode, tokenErr.Error)
	}

	var out TokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrTokenExchangeFailed, err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("%w: no access token in the response", ErrTokenExchangeFailed)
	}
	return &out, nil
}

// User is the GitHub account the token belongs to.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// User fetches the authenticated user's identity.
//
// This call is what makes the flow trustworthy. A callback that accepted the
// username or avatar from the URL would let anyone bind an account they do not
// control; asking GitHub *who this token belongs to* cannot be spoofed by the
// caller, and the numeric id it returns is the immutable key the binding is stored
// under.
func (c *Client) User(ctx context.Context, accessToken string) (*User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/user", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUserFetchFailed, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUserFetchFailed, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrUserFetchFailed, err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrUserFetchFailed, res.StatusCode)
	}

	var out User
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUserFetchFailed, err)
	}
	if out.ID == 0 {
		return nil, fmt.Errorf("%w: no user id in the response", ErrUserFetchFailed)
	}
	return &out, nil
}

// postForm posts a form and asks for JSON back. GitHub answers with a form body
// unless Accept says otherwise, and a form-encoded error is not something this
// code should try to parse.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}
