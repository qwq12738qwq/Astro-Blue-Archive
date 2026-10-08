package config_test

import (
	"net"
	"strings"
	"testing"

	"blogcms/internal/config"
	"blogcms/internal/testutil"
)

// ARCHITECTURE.md §8 and ID-11: Astro is the only process that may be reachable from
// outside, and the Go backend is loopback-only.
//
// The address used to be built as ":"+PORT, which reaches every interface. Nothing
// caught it — a test that passed `PORT` and asserted a string could not tell the
// difference, and the consequence (the admin JSON API on the network, with no CSP and
// no Origin check, because both live in Astro) is invisible until someone runs this
// on a host with a public address.
func TestBackendListensOnLoopbackByDefault(t *testing.T) {
	cfg, err := config.Load(testutil.Env(t, map[string]string{"PORT": "9901"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9901" {
		t.Errorf("Addr = %q, want loopback only; binding every interface would expose the admin API directly", cfg.Addr)
	}
	if host, _, err := net.SplitHostPort(cfg.Addr); err != nil || host != "127.0.0.1" {
		t.Errorf("listen host = %q (%v), want 127.0.0.1", host, err)
	}
}

// The one escape hatch, deliberately spelled out rather than implied by leaving BIND
// unset: a deployment that genuinely needs the backend on another interface has to
// name it, so the exposure cannot happen by forgetting a variable.
func TestBindOverridesLoopbackOnlyWhenAsked(t *testing.T) {
	cfg, err := config.Load(testutil.Env(t, map[string]string{"PORT": "9901", "BIND": "0.0.0.0"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Addr != "0.0.0.0:9901" {
		t.Errorf("Addr = %q, want the explicitly requested bind", cfg.Addr)
	}
}

// A BIND that is a bare IPv6 literal is the case that makes `":"+PORT` and
// JoinHostPort disagree, so the two must not be assumed interchangeable.
func TestBindAcceptsAnIPv6Literal(t *testing.T) {
	cfg, err := config.Load(testutil.Env(t, map[string]string{"PORT": "9901", "BIND": "::1"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if host, _, err := net.SplitHostPort(cfg.Addr); err != nil || host == "" {
		t.Fatalf("Addrs %q is not a listen address: %v", cfg.Addr, err)
	}
}

// A malformed value must be reported against the variable that is wrong. Without this
// the only failure is a net.Listen error quoting an address string the operator never
// typed.
func TestBadBindIsRejectedAtStartup(t *testing.T) {
	_, err := config.Load(testutil.Env(t, map[string]string{"PORT": "9901", "BIND": "not:an:address"}))
	if err == nil {
		t.Fatal("a malformed BIND must be refused rather than reaching net.Listen")
	}
	if !strings.Contains(err.Error(), "BIND") {
		t.Errorf("the error should name BIND, got %v", err)
	}
}
