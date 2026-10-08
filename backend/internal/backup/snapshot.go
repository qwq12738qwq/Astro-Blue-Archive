package backup

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// tmpSuffix marks the scratch name a file is placed under before
// its atomic rename into the snapshot. A crashed run may leave
// one behind; the prune step removes stragglers.
const tmpSuffix = ".backup-tmp"

// syncSnapshot materializes the current state of every source root
// into the repository work tree, then removes snapshot files whose
// source is gone. It is the only writer of the work tree, and it
// never writes into a source root: a backup reads content and
// writes Git metadata, nothing else (ARCHITECTURE.md §63).
//
// Each file is placed atomically (write or link to a scratch
// name, then rename over the target), so a concurrent editor
// never observes a half-written snapshot. Hardlinks are used
// when the source and the repository share a filesystem: the
// content writer replaces files with os.Rename, so a hardlink
// holds the inode of the version that existed at sync time and
// costs no extra storage.
func (s *Service) syncSnapshot(ctx context.Context) error {
	seen := make(map[string]bool)
	var total int64
	for _, src := range s.policy.sources() {
		if err := s.syncRoot(ctx, src, seen, &total); err != nil {
			return err
		}
	}
	for _, src := range s.policy.sources() {
		if err := s.prune(filepath.Join(s.repoPath, src.snapshot), src.snapshot, seen); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) syncRoot(ctx context.Context, src source, seen map[string]bool, total *int64) error {
	return filepath.WalkDir(src.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			// Directories are walked through; symlinks, devices
			// and other special files are never backed up.
			return nil
		}
		rel, err := filepath.Rel(src.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !s.policy.included(src.root, rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > s.policy.maxFile {
			return &FileTooLargeError{Path: rel, Size: info.Size(), Limit: s.policy.maxFile}
		}
		*total += info.Size()
		if *total > s.policy.maxTotal {
			return &TotalTooLargeError{Path: rel, Total: *total, Limit: s.policy.maxTotal}
		}
		if err := placeFile(p, filepath.Join(s.repoPath, src.snapshot, filepath.FromSlash(rel))); err != nil {
			return err
		}
		seen[src.snapshot+"/"+rel] = true
		// Check between files, not per byte: the walk is the
		// natural cancellation point of a backup.
		return ctx.Err()
	})
}

// placeFile copies or hardlinks src to dst atomically.
func placeFile(src, dst string) error {
	// The snapshot directory tree is created on demand:
	// the repository starts empty, so a post's parent
	// directory usually does not exist yet.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + tmpSuffix
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Link(src, tmp); err != nil {
		// Cross-device, or a filesystem without hardlinks:
		// fall back to a real copy, which is always correct.
		if err := copyFile(src, tmp); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		// The copy error is the one worth reporting; the close error on a
		// partially written scratch file is not. placeFile removes the
		// scratch name before every attempt, and prune sweeps any stragglers,
		// so the partial file never becomes a snapshot.
		_ = out.Close()
		return err
	}
	return out.Close()
}

// prune removes snapshot files whose source no longer exists, so
// the next commit records the deletion. It also drops scratch
// files a crashed run left behind. Empty directories are left
// alone: Git does not track them, and removing them here would
// be cosmetic work with no effect on history.
func (s *Service) prune(snapshotDir, prefix string, seen map[string]bool) error {
	return filepath.WalkDir(snapshotDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(snapshotDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if seen[prefix+"/"+rel] {
			return nil
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}
