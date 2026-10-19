package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"time"

	"blogcms/internal/auth"
	"blogcms/internal/github"
	"blogcms/internal/httpx"
	"blogcms/internal/store"
)

const (
	githubSettingClientID     = "github.oauth.clientId"
	githubSettingClientSecret = "github.oauth.clientSecret"
	githubSettingCallbackURL  = "github.oauth.callbackUrl"
	githubSettingWebBase      = "github.oauth.webBase"
	githubSettingAPIBase      = "github.oauth.apiBase"
)

// loadGitHubOAuthConfig reads an explicitly saved OAuth App configuration from the
// setting table. It is a read only; a missing row simply means "use whatever else
// is configured".
func LoadGitHubOAuthConfig(db *store.DB, keyring *github.Keyring) (github.Config, bool, error) {
	if db == nil {
		return github.Config{}, false, nil
	}
	values := map[string]string{}
	rows, err := db.QueryContext(context.Background(), `SELECT key, value FROM setting WHERE key LIKE 'github.oauth.%'`)
	if err != nil {
		return github.Config{}, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return github.Config{}, false, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return github.Config{}, false, err
	}
	clientID := strings.TrimSpace(values[githubSettingClientID])
	callbackURL := strings.TrimSpace(values[githubSettingCallbackURL])
	if clientID == "" || callbackURL == "" || values[githubSettingClientSecret] == "" {
		return github.Config{}, false, nil
	}
	secret := ""
	if keyring != nil {
		opened, err := keyring.Open(values[githubSettingClientSecret])
		if err != nil {
			return github.Config{}, false, err
		}
		secret = opened
	}
	webBase := strings.TrimSpace(values[githubSettingWebBase])
	if webBase == "" {
		webBase = "https://github.com"
	}
	apiBase := strings.TrimSpace(values[githubSettingAPIBase])
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	return github.Config{
		ClientID:     clientID,
		ClientSecret: secret,
		CallbackURL:  callbackURL,
		WebBase:      webBase,
		APIBase:      apiBase,
		StateTTL:     10 * time.Minute,
	}, true, nil
}

func (d Deps) putGitHubConfig(w http.ResponseWriter, r *http.Request) {
	session := SessionFrom(r)
	if session.Role != auth.RoleOwner {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
			"the owner role is required to configure GitHub OAuth"))
		return
	}
	var req struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		CallbackURL  string `json:"callbackUrl"`
		WebBase      string `json:"webBase"`
		APIBase      string `json:"apiBase"`
	}
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	existing := d.GitHub.ReadConfig()
	clientID := strings.TrimSpace(req.ClientID)
	if clientID == "" {
		clientID = existing.ClientID
	}
	callbackURL := strings.TrimSpace(req.CallbackURL)
	if callbackURL == "" {
		callbackURL = existing.CallbackURL
	}
	webBase := strings.TrimSpace(req.WebBase)
	if webBase == "" {
		webBase = existing.WebBase
	}
	if webBase == "" {
		webBase = "https://github.com"
	}
	apiBase := strings.TrimSpace(req.APIBase)
	if apiBase == "" {
		apiBase = existing.APIBase
	}
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	clientSecret := strings.TrimSpace(req.ClientSecret)
	if clientSecret == "" {
		clientSecret = existing.ClientSecret
	}
	if clientID == "" || clientSecret == "" || callbackURL == "" {
		fields := map[string]string{}
		if clientID == "" {
			fields["clientId"] = "client ID is required"
		}
		if clientSecret == "" {
			fields["clientSecret"] = "client secret is required"
		}
		if callbackURL == "" {
			fields["callbackUrl"] = "callback URL is required"
		}
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}
	for name, raw := range map[string]string{"callbackUrl": callbackURL, "webBase": webBase, "apiBase": apiBase} {
		u, err := url.ParseRequestURI(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			httpx.WriteError(w, r, httpx.ValidationError(map[string]string{name: "not a valid URL"}))
			return
		}
	}

	cfg := github.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		CallbackURL:  callbackURL,
		WebBase:      webBase,
		APIBase:      apiBase,
		StateTTL:     existing.StateTTL,
	}
	if err := d.GitHub.Configure(cfg); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := d.saveGitHubOAuthConfig(r.Context(), cfg); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.auditAs(r, "github_config.updated", "github")
	httpx.WriteJSON(w, http.StatusOK, githubConfigResponse{
		Configured:     d.GitHub.Enabled(),
		ClientID:       clientID,
		CallbackURL:    callbackURL,
		WebBase:        webBase,
		APIBase:        apiBase,
		HasClientSecret: clientSecret != "",
	})
}

func (d Deps) saveGitHubOAuthConfig(ctx context.Context, cfg github.Config) error {
	values := map[string]string{
		githubSettingClientID: clientIDValue(cfg.ClientID),
		githubSettingCallbackURL: cfg.CallbackURL,
		githubSettingWebBase:     cfg.WebBase,
		githubSettingAPIBase:      cfg.APIBase,
	}
	sealedSecret, err := d.GitHub.SealSecret(cfg.ClientSecret)
	if err != nil {
		return err
	}
	values[githubSettingClientSecret] = sealedSecret
	return d.DB.WithTx(ctx, func(tx *sql.Tx) error {
		for key, value := range values {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO setting (key, value, updated_at) VALUES (?, ?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				key, value, store.Now()); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d Deps) auditGitHubConfig(w http.ResponseWriter, r *http.Request) {}

func clientIDValue(v string) string { return v }