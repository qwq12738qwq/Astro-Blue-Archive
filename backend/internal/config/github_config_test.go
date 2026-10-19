package config_test

// GitHub account-binding configuration.
//
// The rule this file tests is that a *partially* configured binding is fatal at
// startup rather than half-working later: an ID without a secret, or a secret
// without a callback. A deployment that sets none of them must load unchanged —
// password sign-in, sessions, CSRF, rate limits and the local Git backup do not
// touch GitHub. The credential encryption key is deliberately *not* among the
// variables: the backend owns it (ARCHITECTURE.md ID-73), so there is nothing for
// a deployment to get wrong there.

import (
	"net/url"
	"strings"
	"testing"

	"blogcms/internal/config"
	"blogcms/internal/github"
	"blogcms/internal/testutil"
)

// githubEnv is a fully configured binding, used as the base for the negative cases.
func githubEnv(t *testing.T) map[string]string {
	return map[string]string{
		"GITHUB_CLIENT_ID":     "Iv1.0123456789abcdef",
		"GITHUB_CLIENT_SECRET": "the-client-secret",
		"GITHUB_CALLBACK_URL":  "https://blog.example.com/api/v1/admin/github/callback",
	}
}

func TestGitHubBindingIsOptional(t *testing.T) {
	cfg, err := config.Load(testutil.Env(t, nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GitHubEnabled {
		t.Error("an unset configuration reports itself as enabled")
	}
	if cfg.GitHubClientID != "" || cfg.GitHubCallbackURL != "" {
		t.Errorf("an unset configuration produced values: %+v", cfg)
	}
	// The state TTL still has a sane default, so an enabled configuration that
	// omits it behaves predictably.
	if cfg.GitHubOAuthStateTTL <= 0 {
		t.Error("no default OAuth state lifetime")
	}
}

func TestGitHubBindingLoads(t *testing.T) {
	cfg, err := config.Load(testutil.Env(t, githubEnv(t)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.GitHubEnabled {
		t.Fatal("a fully configured binding is not enabled")
	}
	if cfg.GitHubCallbackURL != "https://blog.example.com/api/v1/admin/github/callback" {
		t.Errorf("callback = %q", cfg.GitHubCallbackURL)
	}
	if cfg.GitHubWebBase != "https://github.com" {
		t.Errorf("web base = %q, want the GitHub default", cfg.GitHubWebBase)
	}
	if cfg.GitHubAPIBase != "https://api.github.com" {
		t.Errorf("api base = %q, want the GitHub default", cfg.GitHubAPIBase)
	}
}

func TestGitHubBindingRejectsHalfAConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(env map[string]string)
		names  string
	}{
		{"a secret without an id", func(e map[string]string) { delete(e, "GITHUB_CLIENT_ID") }, "GITHUB_CLIENT_ID"},
		{"an id without a secret", func(e map[string]string) { delete(e, "GITHUB_CLIENT_SECRET") }, "GITHUB_CLIENT_SECRET"},
		{"no callback", func(e map[string]string) { delete(e, "GITHUB_CALLBACK_URL") }, "GITHUB_CALLBACK_URL"},
		{"a callback with a query", func(e map[string]string) {
			e["GITHUB_CALLBACK_URL"] = "https://blog.example.com/cb?next=/admin"
		}, "GITHUB_CALLBACK_URL"},
		{"a callback that is not a URL", func(e map[string]string) {
			e["GITHUB_CALLBACK_URL"] = "not-a-url"
		}, "GITHUB_CALLBACK_URL"},
		{"a callback with no path", func(e map[string]string) {
			e["GITHUB_CALLBACK_URL"] = "https://blog.example.com"
		}, "GITHUB_CALLBACK_URL"},
		{"a web base with a path", func(e map[string]string) {
			e["GITHUB_ALLOW_ENDPOINT_OVERRIDES"] = "1"
			e["GITHUB_WEB_BASE"] = "https://github.com/some/path"
		}, "GITHUB_WEB_BASE"},
		{"an api base that is not http(s)", func(e map[string]string) {
			e["GITHUB_ALLOW_ENDPOINT_OVERRIDES"] = "1"
			e["GITHUB_API_BASE"] = "ftp://example.test"
		}, "GITHUB_API_BASE"},
		{"a state TTL below the floor", func(e map[string]string) {
			e["GITHUB_OAUTH_STATE_TTL"] = "10s"
		}, "GITHUB_OAUTH_STATE_TTL"},
		{"a state TTL above the ceiling", func(e map[string]string) {
			e["GITHUB_OAUTH_STATE_TTL"] = "24h"
		}, "GITHUB_OAUTH_STATE_TTL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := githubEnv(t)
			tc.mutate(env)
			_, err := config.Load(testutil.Env(t, env))
			if err == nil {
				t.Fatalf("%s must be refused at startup", tc.name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the error should name %s, got %v", tc.names, err)
			}
		})
	}
}

func TestGitHubEndpointOverridesAreAccepted(t *testing.T) {
	env := githubEnv(t)
	env["GITHUB_ALLOW_ENDPOINT_OVERRIDES"] = "true"
	env["GITHUB_WEB_BASE"] = "http://127.0.0.1:9901"
	env["GITHUB_API_BASE"] = "http://127.0.0.1:9902"
	cfg, err := config.Load(testutil.Env(t, env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The overrides exist so the test suites can drive the flow against a local
	// server; they are honored, not silently ignored.
	if cfg.GitHubWebBase != "http://127.0.0.1:9901" || cfg.GitHubAPIBase != "http://127.0.0.1:9902" {
		t.Errorf("overrides = %q / %q", cfg.GitHubWebBase, cfg.GitHubAPIBase)
	}
	if !cfg.GitHubEndpointOverrides {
		t.Error("the deployment opted in, so the config must say so")
	}
}

// The failure this gate exists for: a deployment whose environment still carries
// a test harness's mock GitHub must not send an administrator's browser there.
//
// It happened. `GITHUB_WEB_BASE=http://127.0.0.1:9911` was trusted absolutely,
// nothing logged it, and the bind button left for a local stand-in that answered
// 404 — the flow simply never reached github.com. The rule now is that an
// override without the opt-in is a fatal startup error naming both variables, so
// the deployment cannot boot into a state where the wrong GitHub is authoritative.
func TestGitHubEndpointOverridesRequireTheOptIn(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(env map[string]string)
		names  string
	}{
		{"a mock web base", func(e map[string]string) {
			e["GITHUB_WEB_BASE"] = "http://127.0.0.1:9911"
		}, "GITHUB_WEB_BASE and GITHUB_ALLOW_ENDPOINT_OVERRIDES"},
		{"a mock api base only", func(e map[string]string) {
			e["GITHUB_API_BASE"] = "http://127.0.0.1:9911"
		}, "GITHUB_API_BASE and GITHUB_ALLOW_ENDPOINT_OVERRIDES"},
		{"both mock bases", func(e map[string]string) {
			e["GITHUB_WEB_BASE"] = "http://127.0.0.1:9911"
			e["GITHUB_API_BASE"] = "http://127.0.0.1:9911"
		}, "GITHUB_WEB_BASE and GITHUB_API_BASE"},
		{"an enterprise host without the opt-in", func(e map[string]string) {
			e["GITHUB_WEB_BASE"] = "https://github.example.com"
		}, "GITHUB_ALLOW_ENDPOINT_OVERRIDES"},
		{"an opt-in explicitly false", func(e map[string]string) {
			e["GITHUB_ALLOW_ENDPOINT_OVERRIDES"] = "false"
			e["GITHUB_WEB_BASE"] = "http://127.0.0.1:9911"
		}, "GITHUB_ALLOW_ENDPOINT_OVERRIDES"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := githubEnv(t)
			tc.mutate(env)
			_, err := config.Load(testutil.Env(t, env))
			if err == nil {
				t.Fatalf("%s must be refused at startup", tc.name)
			}
			for _, name := range strings.Split(tc.names, " and ") {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("the error should name %s, got %v", name, err)
				}
			}
		})
	}
}

// The boundary fires whether or not the rest of the binding is configured: a
// stray override in a deployment with no credentials is exactly the residual
// environment variable that becomes the provider the moment someone adds them.
func TestGitHubEndpointOverridesAreGatedEvenWhenBindingIsUnset(t *testing.T) {
	env := map[string]string{"GITHUB_WEB_BASE": "http://127.0.0.1:9911"}
	if _, err := config.Load(testutil.Env(t, env)); err == nil {
		t.Fatal("a residual override with no binding configured must still be refused")
	}
}

// An opt-in with no overrides is harmless: the deployment is on the real GitHub.
func TestGitHubEndpointOverridesOptInAloneChangesNothing(t *testing.T) {
	env := githubEnv(t)
	env["GITHUB_ALLOW_ENDPOINT_OVERRIDES"] = "1"
	cfg, err := config.Load(testutil.Env(t, env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GitHubWebBase != "https://github.com" || cfg.GitHubAPIBase != "https://api.github.com" {
		t.Errorf("bases = %q / %q, want the GitHub defaults", cfg.GitHubWebBase, cfg.GitHubAPIBase)
	}
}

// The whole chain, from environment to the URL the browser is sent to. A unit
// test that concatenated strings would pass even if the deployment's resolved
// configuration pointed somewhere else; this one reads `Config` the same way
// `cmd/server` does and asks the real client for the real authorize URL.
func TestAConfiguredBindingAuthorizesAtGitHub(t *testing.T) {
	cfg, err := config.Load(testutil.Env(t, githubEnv(t)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	client := github.NewClient(github.ClientOptions{
		WebBase:      cfg.GitHubWebBase,
		APIBase:      cfg.GitHubAPIBase,
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		CallbackURL:  cfg.GitHubCallbackURL,
	})
	authorize := client.AuthorizeURL("the-one-shot-state", "the-pkce-challenge")

	const want = "https://github.com/login/oauth/authorize?"
	if !strings.HasPrefix(authorize, want) {
		t.Fatalf("authorize URL = %q, want it to start with %q", authorize, want)
	}
	if strings.Contains(authorize, "127.0.0.1") {
		t.Errorf("authorize URL leaves for a loopback stand-in: %q", authorize)
	}
	// The redirect_uri is the configured callback, byte for byte: GitHub compares
	// it with what the App was registered with, and a mismatch is the one failure
	// an administrator sees as "GitHub says the redirect URI is wrong".
	parsed, err := url.Parse(authorize)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := parsed.Query().Get("redirect_uri"); got != cfg.GitHubCallbackURL {
		t.Errorf("redirect_uri = %q, want %q", got, cfg.GitHubCallbackURL)
	}
	if got := parsed.Query().Get("state"); got != "the-one-shot-state" {
		t.Errorf("state = %q", got)
	}
	if got := parsed.Query().Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
}

// The same chain, with the opt-in set: the stand-in is used, and it is used
// because the deployment said so rather than because the environment said so.
func TestAnOptedInStandInAuthorizesThere(t *testing.T) {
	env := githubEnv(t)
	env["GITHUB_ALLOW_ENDPOINT_OVERRIDES"] = "1"
	env["GITHUB_WEB_BASE"] = "http://127.0.0.1:9911"
	env["GITHUB_API_BASE"] = "http://127.0.0.1:9911"
	cfg, err := config.Load(testutil.Env(t, env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	client := github.NewClient(github.ClientOptions{
		WebBase:      cfg.GitHubWebBase,
		APIBase:      cfg.GitHubAPIBase,
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		CallbackURL:  cfg.GitHubCallbackURL,
	})
	authorize := client.AuthorizeURL("state", "challenge")
	if !strings.HasPrefix(authorize, "http://127.0.0.1:9911/login/oauth/authorize?") {
		t.Errorf("authorize URL = %q, want it on the opted-in stand-in", authorize)
	}
}

func TestGitHubCallbackURLIsNotWidenedByASlash(t *testing.T) {
	env := githubEnv(t)
	env["GITHUB_CALLBACK_URL"] = "https://blog.example.com/cb/"
	cfg, err := config.Load(testutil.Env(t, env))
	if err != nil {
		t.Skipf("a trailing slash is tolerated: %v", err)
	}
	if strings.HasSuffix(cfg.GitHubCallbackURL, "//") {
		t.Errorf("callback = %q", cfg.GitHubCallbackURL)
	}
}

func TestDeploymentMode(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", config.DeploymentProduction},
		{"production", config.DeploymentProduction},
		{"prod", config.DeploymentProduction},
		{"development", config.DeploymentDevelopment},
		{"dev", config.DeploymentDevelopment},
		{"DEV", config.DeploymentDevelopment},
		{" dev ", config.DeploymentDevelopment},
		// An unrecognised word is production, not a startup failure: a deployment
		// that misspells "development" gets the stricter flow.
		{"staging", config.DeploymentProduction},
		{"devellopment", config.DeploymentProduction},
	}
	for _, tc := range cases {
		env := map[string]string{}
		if tc.raw != "" {
			env["APP_ENV"] = tc.raw
		}
		cfg, err := config.Load(testutil.Env(t, env))
		if err != nil {
			t.Fatalf("APP_ENV=%q: %v", tc.raw, err)
		}
		if cfg.Deployment != tc.want {
			t.Errorf("APP_ENV=%q: deployment = %q, want %q", tc.raw, cfg.Deployment, tc.want)
		}
	}
}
