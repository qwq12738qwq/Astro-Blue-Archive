// Command blogcms-backup creates a consistent backup of everything that matters.
//
// ARCHITECTURE.md §28: a personal blog's real data is content + media +
// database. Backing up only one of them is not a backup.
//
// The database is copied with SQLite's online backup API rather than `cp`,
// because the database runs in WAL mode: a plain file copy of blog.db can miss
// committed transactions that still live in the -wal sidecar.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	var (
		dataRoot   = flag.String("data", "", "DATA_ROOT containing blog.db (required)")
		contentDir = flag.String("content", "", "CONTENT_ROOT (required)")
		mediaDir   = flag.String("media", "", "MEDIA_ROOT (required)")
		outDir     = flag.String("out", "", "directory to write the archive into (required)")
		keep       = flag.Int("keep", 7, "how many archives to retain")
		now        = flag.String("now", "", "override the timestamp (RFC3339), for tests")
	)
	flag.Parse()

	if err := run(*dataRoot, *contentDir, *mediaDir, *outDir, *keep, *now); err != nil {
		fmt.Fprintf(os.Stderr, "backup failed: %v\n", err)
		os.Exit(1)
	}
}

func run(dataRoot, contentDir, mediaDir, outDir string, keep int, nowOverride string) error {
	for name, v := range map[string]string{
		"data": dataRoot, "content": contentDir, "media": mediaDir, "out": outDir,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}

	stamp := time.Now().UTC()
	if nowOverride != "" {
		parsed, err := time.Parse(time.RFC3339, nowOverride)
		if err != nil {
			return fmt.Errorf("-now: %w", err)
		}
		stamp = parsed
	}

	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// A staging directory keeps the snapshot self-consistent: content and media
	// are copied first, then the database, so the archive contains one moment
	// in time rather than three unrelated ones.
	stage, err := os.MkdirTemp(outDir, ".backup-stage-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := copyTree(contentDir, filepath.Join(stage, "content")); err != nil {
		return fmt.Errorf("copy content: %w", err)
	}
	if err := copyTree(mediaDir, filepath.Join(stage, "media")); err != nil {
		return fmt.Errorf("copy media: %w", err)
	}
	if err := backupDatabase(filepath.Join(dataRoot, "blog.db"), filepath.Join(stage, "data", "blog.db")); err != nil {
		return fmt.Errorf("back up database: %w", err)
	}

	name := fmt.Sprintf("backup-%s.tar.gz", stamp.Format("20060102T150405Z"))
	target := filepath.Join(outDir, name)

	if err := writeArchive(stage, target); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}

	if err := prune(outDir, keep); err != nil {
		return fmt.Errorf("prune old backups: %w", err)
	}

	fmt.Printf("%s\n", target)
	return nil
}

// backupDatabase uses SQLite's online backup API, which is safe against a
// running WAL database. It writes a single self-contained file.
func backupDatabase(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("database %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}

	// Opened read-write because the fallback below needs to checkpoint the WAL.
	// It is only ever read from, never modified by this tool.
	in, err := sql.Open("sqlite", "file:"+filepath.ToSlash(src))
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// VACUUM INTO is SQLite's online-backup primitive: it writes a consistent,
	// self-contained snapshot of a WAL database while other connections may be
	// active. This is the only path that is correct regardless of what the WAL
	// currently holds.
	//
	// The destination is interpolated as a quoted SQL literal because VACUUM
	// INTO does not accept a bound parameter. It is a path this tool chose, and
	// single quotes are escaped.
	quoted := "'" + strings.ReplaceAll(filepath.ToSlash(dst), "'", "''") + "'"
	if _, err := in.ExecContext(ctx, "VACUUM INTO "+quoted); err == nil {
		return os.Chmod(dst, 0o600)
	} else {
		fmt.Fprintf(os.Stderr, "warning: VACUUM INTO failed (%v); trying a checkpointed copy\n", err)
	}

	// Fallback: fold the WAL into the main file, then copy. A checkpoint can fail
	// while another connection holds a read lock, so a failure here is reported
	// rather than silently producing a backup that is missing committed rows.
	if _, err := in.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("VACUUM INTO unavailable (%v) and the WAL could not be checkpointed: %w",
			"see the warning above", err)
	}

	if err := copyFileWithMode(src, dst, 0o600); err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	return os.Chmod(dst, 0o600)
}

// copyTree copies a directory tree, skipping editor cruft.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skipName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFileWithMode(path, target, 0o640)
	})
}

func skipName(name string) bool {
	if strings.HasPrefix(name, ".DS_Store") || strings.HasPrefix(name, "._") {
		return true
	}
	switch name {
	case ".git", "node_modules":
		return true
	}
	return false
}

func copyFileWithMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeArchive(stage, target string) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}

	// The writers are closed explicitly and in order rather than
	// with defers: a deferred Close runs after the return value is
	// decided, so a failure to flush the tar or gzip stream would be
	// silent — and a truncated backup is exactly what this tool
	// exists to prevent.
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := filepath.WalkDir(stage, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(filepath.Join("blogcms-backup", rel))
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		header.ModTime = info.ModTime().UTC().Truncate(time.Second)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		_, err = io.Copy(tw, in)
		return err
	}); err != nil {
		_ = f.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("close gzip writer: %w", err)
	}
	return f.Close()
}

// prune keeps the newest `keep` archives.
func prune(dir string, keep int) error {
	if keep < 1 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var archives []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "backup-") && strings.HasSuffix(e.Name(), ".tar.gz") {
			archives = append(archives, e.Name())
		}
	}
	if len(archives) <= keep {
		return nil
	}

	// Names embed a sortable UTC timestamp, so lexical order is chronological.
	sort.Strings(archives)
	for _, name := range archives[:len(archives)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
