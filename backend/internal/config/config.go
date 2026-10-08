// Package config loads and validates all runtime configuration.
//
// Configuration errors are always fatal. There is deliberately no "empty blog"
// fallback: a misconfigured deployment must fail loudly at startup rather than
// silently serve an empty site (ARCHITECTURE.md §6).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// hostnameRe accepts what a bind address can legitimately be: an IP literal (checked
// separately, by net.ParseIP) or a resolvable name.
var hostnameRe = regexp.MustCompile("^[A-Za-z0-9._-]+$")

// Backup-side defaults. The per-file ceiling stops one huge
// upload from turning a backup into a denial of service, and
// the total ceiling stops the whole source tree from doing the
// same. Both bound what one backup may read.
const (
	defaultBackupMaxFileBytes  int64 = 128 << 20 // 128 MiB
	defaultBackupMaxTotalBytes int64 = 2 << 30   // 2 GiB
	defaultBackupTimeout             = 60 * time.Second
)

// backupBranchGrammar is the subset of git's reference
// rules the CMS accepts for a configured initial branch.
var backupBranchGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,119}$`)

// Config is the fully validated runtime configuration.
type Config struct {
	Addr string

	ContentRoot string
	MediaRoot   string
	DataRoot    string
	DBPath      string

	// MediaCacheRoot holds DERIVED WebP representations. It is deliberately a
	// different directory from MediaRoot: MEDIA_ROOT is the source of truth and
	// must only ever contain uploaded originals, so a representation cache written
	// beside them would be indistinguishable from a source asset — and a restore
	// from backup would quietly promote a cache entry into the truth.
	MediaCacheRoot string

	// PublicOrigin is the raw PUBLIC_ORIGIN value, kept for logging.
	PublicOrigin string
	// PublicOrigins holds *extra* allowed origins, beyond same-origin.
	//
	// ARCHITECTURE.md ID-24: the allowlist is not a closed set of addresses. Same-origin
	// mutations are accepted without any configuration (see auth.VerifyOrigin), and
	// this adds only what that comparison cannot cover — a hostname, or an origin
	// behind TLS termination. Membership is exact and constant-time: no prefix, no
	// suffix, no wildcard.
	PublicOrigins []string
	SecureCookie  bool

	SessionSecretFile string
	SessionTTL        time.Duration
	SessionIdleTTL    time.Duration

	MaxJSONBody    int64
	MaxUploadBytes int64

	// ImageMaxPixels bounds the DECODED pixel count of an upload and of any
	// conversion.
	//
	// ARCHITECTURE.md §26: a 5 MB JPEG can decode to hundreds of megapixels, and the
	// encoder allocates several bytes per pixel while it works. Capping the byte
	// count alone does not bound memory, so the pixel count is checked separately,
	// from the header, before anything is decoded.
	//
	// 20 megapixels is roughly a 6000×3400 frame: larger than any phone camera, and
	// a ceiling that stops a 100 KB crafted file from asking for a gigabyte. RGBA
	// alone is 80 MB at that size, before the encoder's own working set.
	ImageMaxPixels int64

	// ImageMemoryCacheMB is the default in-memory ceiling for converted
	// representations. The admin can change it at runtime (settings), so this is
	// only the value used before any setting has been read.
	ImageMemoryCacheMB int

	LoginPerMinute int
	LoginPerHour   int

	CommentPerHour int

	// GitBackupRoot is the directory the local backup
	// repository lives in. It is deliberately a separate
	// root: the repository is a versioned copy of the
	// content, not content itself, and keeping it outside
	// the content and media roots is what lets a backup
	// snapshot them without recording its own metadata
	// (see backupRoot).
	GitBackupRoot string
	// GitDefaultBranch names the branch a newly
	// initialized repository starts on. Empty means the
	// operator's global Git configuration (init.defaultBranch)
	// decides, and the Git library's own default after
	// that. The branch of an existing repository is always
	// read from the repository, never from this field.
	GitDefaultBranch string
	// GitBackupMaxFileBytes and GitBackupMaxTotalBytes
	// bound what one backup may read: one huge upload must
	// not turn a backup into a denial of service. Both are
	// ceilings on reads, not on uploads.
	GitBackupMaxFileBytes  int64
	GitBackupMaxTotalBytes int64
	// GitBackupTimeout bounds one backup operation.
	GitBackupTimeout time.Duration
}

// Load reads configuration from the environment and validates it.
//
// It returns an error rather than exiting so that tests can exercise the
// validation rules without spawning a process.
func Load(getenv func(string) string) (*Config, error) {
	g := getenv
	if g == nil {
		g = os.Getenv
	}

	cfg := &Config{
		// ARCHITECTURE.md §8 / ID-11: Astro is the only process that may be reachable
		// from outside, and the Go backend is loopback-only. That is not a default to
		// try and forget — binding `:PORT` reaches every interface, which would put the
		// admin JSON API on the network with no CSP and no Origin check in front of it,
		// since both of those live in Astro.
		//
		// Loopback is therefore the default rather than the exception. `BIND` exists for
		// the deployment that genuinely needs the backend on another interface, and it
		// must be asked for by name: someone has to type it, so the exposure cannot
		// happen by forgetting a variable.
		Addr:              net.JoinHostPort(envOr(g, "BIND", "127.0.0.1"), envOr(g, "PORT", "8080")),
		SessionSecretFile: g("SESSION_SECRET_FILE"),
	}

	var err error
	if cfg.ContentRoot, err = requiredRoot(g, "CONTENT_ROOT"); err != nil {
		return nil, err
	}
	if cfg.MediaRoot, err = requiredRoot(g, "MEDIA_ROOT"); err != nil {
		return nil, err
	}
	if cfg.DataRoot, err = requiredRoot(g, "DATA_ROOT"); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataRoot, 0o750); err != nil {
		return nil, fmt.Errorf("DATA_ROOT is not writable: %w", err)
	}

	cfg.DBPath = filepath.Join(cfg.DataRoot, "blog.db")

	// The representation cache defaults inside DATA_ROOT and is derived data, so
	// it may be wiped at any time without losing anything. MEDIA_CACHE_ROOT is
	// overridable for a deployment that wants it on faster storage.
	cfg.MediaCacheRoot = filepath.Join(cfg.DataRoot, "media-cache")
	if raw := strings.TrimSpace(g("MEDIA_CACHE_ROOT")); raw != "" {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return nil, fmt.Errorf("MEDIA_CACHE_ROOT: cannot resolve %q: %w", raw, err)
		}
		cfg.MediaCacheRoot = filepath.Clean(abs)
		// A configured cache root that does not exist is created, unlike the three
		// content roots: it holds nothing anyone authored.
		if err := os.MkdirAll(cfg.MediaCacheRoot, 0o750); err != nil {
			return nil, fmt.Errorf("MEDIA_CACHE_ROOT is not writable: %w", err)
		}
	}

	// ARCHITECTURE.md ID-24: PUBLIC_ORIGIN lists *extra* origins, and is optional.
	//
	// The Origin check compares the browser's Origin against the address the request
	// arrived on, which needs no list. This list exists only for what that cannot see:
	// a hostname behind TLS termination, or a front-end proxy that rewrites the
	// authority. It is additive, never a closed set — a closed set of addresses is a
	// list the server cannot keep correct (behind NAT, a port-forward or a public IP,
	// the address the browser used is not an address this machine has), and a list
	// that cannot be kept correct is what locked the owner out of their own admin.
	if raw := strings.TrimSpace(g("PUBLIC_ORIGIN")); raw != "" {
		cfg.PublicOrigin = raw
		cfg.PublicOrigins, err = ParseOrigins(raw)
		if err != nil {
			return nil, err
		}
		slog.Info("public_origin_extra", "origins", strings.Join(cfg.PublicOrigins, ","))
	}

	// Whether the session cookie carries `Secure` is the deployer's
	// decision, not the application's: a TLS deployment sets
	// SECURE_COOKIES, a plain-HTTP deployment leaves it off. The
	// default is false so an install reached over HTTP can still
	// reach its own admin — a Secure cookie on an HTTP origin is
	// a lockout, not a hardening. The cookie stays HttpOnly and
	// SameSite=Strict either way, and no other behaviour differs.
	cfg.SecureCookie = boolEnv(g, "SECURE_COOKIES", false)

	// The listen address is assembled above, so a bad BIND or PORT reaches the point
	// where the only symptom would be a net.Listen error naming `cfg.Addr` — a string
	// the operator did not type. Checking it here turns that into a message about the
	// variable that is actually wrong.
	//
	// SplitHostPort is not enough on its own: net.JoinHostPort will happily wrap
	// `not:an:address` into `[not:an:address]:9901`, which splits cleanly and fails
	// much later with a DNS error. So the host is checked to be an IP or a bare
	// hostname, which is what either of them actually is.
	if host, port, err := net.SplitHostPort(cfg.Addr); err != nil {
		return nil, fmt.Errorf("BIND and PORT do not form a listen address (%q): %w", cfg.Addr, err)
	} else if net.ParseIP(host) == nil && !hostnameRe.MatchString(host) {
		return nil, fmt.Errorf("BIND %q is not an IP address or a hostname", host)
	} else if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("PORT %q is not a number", port)
	}

	if cfg.SessionTTL, err = durationEnv(g, "SESSION_TTL", 12*time.Hour); err != nil {
		return nil, err
	}
	if cfg.SessionIdleTTL, err = durationEnv(g, "SESSION_IDLE_TTL", 2*time.Hour); err != nil {
		return nil, err
	}
	if cfg.MaxJSONBody, err = int64Env(g, "MAX_JSON_BODY", 1<<20); err != nil {
		return nil, err
	}
	if cfg.MaxUploadBytes, err = int64Env(g, "MAX_UPLOAD_BYTES", 5<<20); err != nil {
		return nil, err
	}
	if cfg.ImageMaxPixels, err = int64Env(g, "IMAGE_MAX_PIXELS", 20_000_000); err != nil {
		return nil, err
	}
	if cfg.ImageMemoryCacheMB, err = intEnv(g, "IMAGE_MEMORY_CACHE_MB", 64); err != nil {
		return nil, err
	}
	if cfg.LoginPerMinute, err = intEnv(g, "LOGIN_PER_MINUTE", 5); err != nil {
		return nil, err
	}
	if cfg.LoginPerHour, err = intEnv(g, "LOGIN_PER_HOUR", 20); err != nil {
		return nil, err
	}
	if cfg.CommentPerHour, err = intEnv(g, "COMMENT_PER_HOUR", 5); err != nil {
		return nil, err
	}

	// Git Backup Phase 1 (PROJECT_STATUS.md): a local repository
	// that versions the file-based content. The location and the
	// ceilings are configuration, not code, so a deployment can
	// place the repository where it wants and bound what a backup
	// may read.
	if cfg.GitBackupRoot, err = backupRoot(g, "GIT_BACKUP_ROOT", cfg.DataRoot, cfg.ContentRoot, cfg.MediaRoot); err != nil {
		return nil, err
	}
	if raw := strings.TrimSpace(g("GIT_DEFAULT_BRANCH")); raw != "" {
		if !backupBranchGrammar.MatchString(raw) || strings.Contains(raw, "..") ||
			strings.HasSuffix(raw, ".lock") || strings.HasSuffix(raw, "/") {
			return nil, fmt.Errorf("GIT_DEFAULT_BRANCH %q is not a valid branch name", raw)
		}
		cfg.GitDefaultBranch = raw
	}
	if cfg.GitBackupMaxFileBytes, err = int64Env(g, "GIT_BACKUP_MAX_FILE_BYTES", defaultBackupMaxFileBytes); err != nil {
		return nil, err
	}
	if cfg.GitBackupMaxTotalBytes, err = int64Env(g, "GIT_BACKUP_MAX_TOTAL_BYTES", defaultBackupMaxTotalBytes); err != nil {
		return nil, err
	}
	if cfg.GitBackupTimeout, err = durationEnv(g, "GIT_BACKUP_TIMEOUT", defaultBackupTimeout); err != nil {
		return nil, err
	}
	if cfg.GitBackupMaxFileBytes < 1 {
		return nil, fmt.Errorf("GIT_BACKUP_MAX_FILE_BYTES must be >= 1")
	}
	if cfg.GitBackupMaxTotalBytes < 1 {
		return nil, fmt.Errorf("GIT_BACKUP_MAX_TOTAL_BYTES must be >= 1")
	}

	// Rate limiting is never fully disabled (ARCHITECTURE.md §8, §27): values
	// below 1 would remove abuse protection, so they are rejected outright
	// rather than silently honoured.
	for name, v := range map[string]int{
		"LOGIN_PER_MINUTE": cfg.LoginPerMinute,
		"LOGIN_PER_HOUR":   cfg.LoginPerHour,
		"COMMENT_PER_HOUR": cfg.CommentPerHour,
		// The image cache is not a protection, so a small value is merely useless
		// rather than unsafe. Zero would be a real footgun though: every request
		// would convert again, and the setting would look like it had been applied.
		"IMAGE_MEMORY_CACHE_MB": cfg.ImageMemoryCacheMB,
	} {
		if v < 1 {
			return nil, fmt.Errorf("%s must be >= 1", name)
		}
	}
	if cfg.ImageMemoryCacheMB > 1024 {
		return nil, fmt.Errorf("IMAGE_MEMORY_CACHE_MB must be <= 1024, got %d", cfg.ImageMemoryCacheMB)
	}
	if cfg.ImageMaxPixels < 1 {
		return nil, fmt.Errorf("IMAGE_MAX_PIXELS must be >= 1, got %d", cfg.ImageMaxPixels)
	}

	if cfg.SessionSecretFile != "" {
		if err := validateFileTarget(cfg.SessionSecretFile); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

func requiredEnv(g func(string) string, key string) (string, error) {
	v := strings.TrimSpace(g(key))
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

// requiredRoot resolves an environment variable to an existing directory.
//
// Per ARCHITECTURE.md §6 the variable must exist, must point at an existing
// path, and that path must be a directory. Anything else is fatal.
func requiredRoot(g func(string) string, key string) (string, error) {
	v, err := requiredEnv(g, key)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve %q: %w", key, v, err)
	}
	abs = filepath.Clean(abs)

	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: path %q does not exist (mount the volume before starting)", key, abs)
		}
		return "", fmt.Errorf("%s: cannot stat %q: %w", key, abs, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s: %q is not a directory", key, abs)
	}
	return abs, nil
}

// ParseOrigins splits PUBLIC_ORIGIN into the set of origins a mutation may come
// from.
//
// The value may hold several, comma-separated, because a single-origin assumption
// does not survive contact with a laptop, a phone on the same network and a tunnel
// (ID-22). Every entry is still validated as an absolute http(s) origin with no
// path, and duplicates are collapsed, so "one value" and "one value in a list"
// behave identically — which is why every existing test and deployment keeps
// working unchanged.
func ParseOrigins(raw string) ([]string, error) {
	seen := make(map[string]bool)
	out := make([]string, 0, 1)
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimRight(strings.TrimSpace(part), "/")
		if origin == "" {
			// A trailing comma is a typo, not an empty allowance. Silently skipping
			// it would make "a,,b" harder to read and would hide a missing value.
			return nil, fmt.Errorf("PUBLIC_ORIGIN contains an empty origin: %q", raw)
		}
		if err := validateOrigin(origin); err != nil {
			return nil, err
		}
		if seen[origin] {
			continue
		}
		seen[origin] = true
		out = append(out, origin)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("PUBLIC_ORIGIN lists no origin: %q", raw)
	}
	return out, nil
}

// validateOrigin rejects anything that is not an absolute http(s) origin, so
// that Origin/Referer comparisons for CSRF cannot be trivially satisfied.
func validateOrigin(origin string) error {
	switch {
	case strings.HasPrefix(origin, "https://"):
	case strings.HasPrefix(origin, "http://"):
	default:
		return fmt.Errorf("PUBLIC_ORIGIN must start with http:// or https://, got %q", origin)
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	if rest == "" || strings.ContainsAny(rest, "/?# ") {
		return fmt.Errorf("PUBLIC_ORIGIN must be scheme://host[:port] with no path, got %q", origin)
	}
	return nil
}

// validateFileTarget refuses a session secret path that is not a plain file
// inside a directory we control.
func validateFileTarget(p string) error {
	if p == "" {
		return nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("SESSION_SECRET_FILE: %w", err)
	}
	st, err := os.Stat(abs)
	if err == nil && st.IsDir() {
		return fmt.Errorf("SESSION_SECRET_FILE %q is a directory", abs)
	}
	return nil
}

func envOr(g func(string) string, key, def string) string {
	if v := strings.TrimSpace(g(key)); v != "" {
		return v
	}
	return def
}

func boolEnv(g func(string) string, key string, def bool) bool {
	v := strings.TrimSpace(g(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func intEnv(g func(string) string, key string, def int) (int, error) {
	v := strings.TrimSpace(g(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func int64Env(g func(string) string, key string, def int64) (int64, error) {
	v := strings.TrimSpace(g(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func durationEnv(g func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(g(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return d, nil
}

// backupRoot resolves GIT_BACKUP_ROOT (default: DATA_ROOT/git-backup),
// creates it if missing, and enforces the containment rules that keep the
// repository a copy of the content rather than part of it.
//
// The repository must live outside the content and media roots — a
// repository inside what it backs up would record its own metadata as
// content — and it must not contain the data root, whose runtime database
// and derived cache must never sit inside a repository work tree.
func backupRoot(g func(string) string, key, dataRoot, contentRoot, mediaRoot string) (string, error) {
	raw := strings.TrimSpace(g(key))
	if raw == "" {
		raw = filepath.Join(dataRoot, "git-backup")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve %q: %w", key, raw, err)
	}
	abs = filepath.Clean(abs)
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return "", fmt.Errorf("%s: cannot create %q: %w", key, abs, err)
	}
	// Resolve symlinks on both sides before comparing, so a symlinked
	// mount cannot place the repository inside a root it must not
	// touch — or the other way around.
	repo, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve %q: %w", key, abs, err)
	}
	for _, root := range []string{contentRoot, mediaRoot} {
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", fmt.Errorf("%s: cannot resolve %q: %w", key, root, err)
		}
		if within(repo, canonical) {
			return "", fmt.Errorf("%s: %q is inside %q; the backup repository must live outside the content it versions", key, repo, canonical)
		}
	}
	canonicalData, err := filepath.EvalSymlinks(dataRoot)
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve %q: %w", key, dataRoot, err)
	}
	if within(canonicalData, repo) {
		return "", fmt.Errorf("%s: %q contains the data root %q; the runtime database must never sit inside the repository work tree", key, repo, canonicalData)
	}
	return repo, nil
}

// within reports whether path is inside dir. Both must be absolute,
// cleaned and symlink-resolved.
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(os.PathSeparator))
}
