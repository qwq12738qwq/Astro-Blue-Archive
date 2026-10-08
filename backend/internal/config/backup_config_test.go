package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blogcms/internal/config"
)

// backupEnv builds a getenv over separate content,
// media and data roots, because the backup-root
// containment rules are only meaningful when the
// three roots are distinct directories. The roots
// are returned so a test can name paths inside
// the same tree the loader will see.
func backupEnv(t *testing.T, over map[string]string) (func(string) string, map[string]string) {
	t.Helper()
	base := t.TempDir()
	content := filepath.Join(base, "content")
	media := filepath.Join(base, "media")
	data := filepath.Join(base, "data")
	for _, dir := range []string{content, media, data} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"CONTENT_ROOT":  content,
		"MEDIA_ROOT":    media,
		"DATA_ROOT":     data,
		"PUBLIC_ORIGIN": "http://127.0.0.1:9900",
	}
	for k, v := range over {
		env[k] = v
	}
	return func(k string) string { return env[k] }, env
}

// The repository lives in its own directory by
// default: DATA_ROOT/git-backup, not the content
// root it versions.
func TestBackupRootDefaultsInsideDataRoot(t *testing.T) {
	g, env := backupEnv(t, nil)
	cfg, err := config.Load(g)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := filepath.Join(env["DATA_ROOT"], "git-backup")
	if cfg.GitBackupRoot != want {
		t.Errorf("GitBackupRoot = %q, want %q", cfg.GitBackupRoot, want)
	}
	if st, err := os.Stat(cfg.GitBackupRoot); err != nil || !st.IsDir() {
		t.Errorf("default backup root was not created: %v", err)
	}
}

// A separate, explicit root is accepted.
func TestBackupRootAcceptsSeparateRoot(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "backup-repository")
	g, _ := backupEnv(t, map[string]string{"GIT_BACKUP_ROOT": repo})
	cfg, err := config.Load(g)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GitBackupRoot != repo {
		t.Errorf("GitBackupRoot = %q, want %q", cfg.GitBackupRoot, repo)
	}
}

// The repository must not live inside the content
// root: a repository inside what it backs up would
// record its own metadata as content.
func TestBackupRootRejectsContentRoot(t *testing.T) {
	g, env := backupEnv(t, nil)
	// The override is written into the same env the
	// closure reads, so the loader evaluates the
	// path against the roots it came from.
	env["GIT_BACKUP_ROOT"] = filepath.Join(env["CONTENT_ROOT"], "git-backup")
	_, err := config.Load(g)
	if err == nil {
		t.Fatal("a backup root inside CONTENT_ROOT must be refused")
	}
	if !strings.Contains(err.Error(), "GIT_BACKUP_ROOT") {
		t.Errorf("the error should name GIT_BACKUP_ROOT, got %v", err)
	}
}

func TestBackupRootRejectsMediaRoot(t *testing.T) {
	g, env := backupEnv(t, nil)
	env["GIT_BACKUP_ROOT"] = filepath.Join(env["MEDIA_ROOT"], "repo")
	_, err := config.Load(g)
	if err == nil {
		t.Fatal("a backup root inside MEDIA_ROOT must be refused")
	}
}

// The repository must not contain the data root:
// the runtime database would end up inside the
// repository work tree, and a backup would version
// runtime state.
func TestBackupRootRejectsDataRootAncestor(t *testing.T) {
	g, env := backupEnv(t, nil)
	env["GIT_BACKUP_ROOT"] = filepath.Dir(env["DATA_ROOT"])
	_, err := config.Load(g)
	if err == nil {
		t.Fatal("a backup root containing DATA_ROOT must be refused")
	}
	env["GIT_BACKUP_ROOT"] = env["DATA_ROOT"]
	_, err = config.Load(g)
	if err == nil {
		t.Fatal("DATA_ROOT itself must be refused as a backup root")
	}
}

// A configured initial branch is validated at
// startup: it names a Git reference, so path
// separators, ".." and ".lock" endings are refused
// rather than reaching the repository.
func TestBackupBranchIsValidated(t *testing.T) {
	for _, branch := range []string{"main", "site-content", "release/2026"} {
		g, _ := backupEnv(t, map[string]string{"GIT_DEFAULT_BRANCH": branch})
		if _, err := config.Load(g); err != nil {
			t.Errorf("branch %q rejected: %v", branch, err)
		}
	}
	for _, branch := range []string{"../escape", "a..b", "evil.lock", "-leading", "has space"} {
		g, _ := backupEnv(t, map[string]string{"GIT_DEFAULT_BRANCH": branch})
		if _, err := config.Load(g); err == nil {
			t.Errorf("branch %q accepted", branch)
		}
	}
}

// The size ceilings and the timeout are configuration
// with sane defaults, and a zero value is never
// honoured as "unlimited".
func TestBackupLimitsAreValidated(t *testing.T) {
	g, _ := backupEnv(t, map[string]string{"GIT_BACKUP_MAX_FILE_BYTES": "0"})
	if _, err := config.Load(g); err == nil {
		t.Fatal("a zero per-file ceiling must be refused")
	}
	g, _ = backupEnv(t, map[string]string{"GIT_BACKUP_MAX_TOTAL_BYTES": "-1"})
	if _, err := config.Load(g); err == nil {
		t.Fatal("a negative total ceiling must be refused")
	}
	g, _ = backupEnv(t, map[string]string{
		"GIT_BACKUP_MAX_FILE_BYTES":  "1048576",
		"GIT_BACKUP_MAX_TOTAL_BYTES": "2097152",
		"GIT_BACKUP_TIMEOUT":         "30s",
	})
	cfg, err := config.Load(g)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GitBackupMaxFileBytes != 1<<20 || cfg.GitBackupMaxTotalBytes != 2<<20 {
		t.Errorf("limits = %d/%d", cfg.GitBackupMaxFileBytes, cfg.GitBackupMaxTotalBytes)
	}
	if cfg.GitBackupTimeout.String() != "30s" {
		t.Errorf("timeout = %v", cfg.GitBackupTimeout)
	}
}
