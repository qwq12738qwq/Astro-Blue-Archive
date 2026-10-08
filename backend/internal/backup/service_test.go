package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// newTestService builds a service over fresh temporary
// content, media and repository roots, with a small
// per-file ceiling so the size tests can use real
// file sizes instead of gigabytes.
func newTestService(t *testing.T) (*Service, string, string) {
	t.Helper()
	contentRoot := t.TempDir()
	mediaRoot := t.TempDir()
	repoRoot := t.TempDir()
	svc := New(Options{
		ContentRoot:   contentRoot,
		MediaRoot:     mediaRoot,
		RepoRoot:      repoRoot,
		MaxFileBytes:  1 << 20, // 1 MiB
		MaxTotalBytes: 8 << 20,
		Timeout:       30 * time.Second,
	})
	return svc, contentRoot, mediaRoot
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeCreatesRepository(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	writeFile(t, filepath.Join(contentRoot, "posts", "hello.md"), "hello")

	res, err := svc.Initialize(t.Context())
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.Commit.Message != "Initial content backup" {
		t.Fatalf("initial message = %q", res.Commit.Message)
	}
	if res.Commit.Hash == "" {
		t.Fatal("initial commit has no hash")
	}
	if _, err := os.Stat(filepath.Join(svc.repoPath, ".git")); err != nil {
		t.Fatalf("repository missing: %v", err)
	}

	// The initial backup records the current content.
	commits, err := svc.History(t.Context(), 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("history = %d commits, want 1", len(commits))
	}
	if commits[0].Hash != res.Commit.Hash {
		t.Fatal("history does not contain the initial commit")
	}
}

func TestInitializeTwiceRefused(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := svc.Initialize(t.Context()); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second initialize = %v, want ErrAlreadyInitialized", err)
	}
}

func TestCommitRefusesEmptyCommit(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	writeFile(t, filepath.Join(contentRoot, "posts", "hello.md"), "hello")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// Nothing changed since the initial backup.
	if _, err := svc.Commit(t.Context()); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("commit with no changes = %v, want ErrNothingToCommit", err)
	}
	commits, err := svc.History(t.Context(), 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("an empty backup created a commit: %d commits", len(commits))
	}
}

func TestCommitRecordsContentChange(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	post := filepath.Join(contentRoot, "posts", "hello.md")
	writeFile(t, post, "# Hello")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	writeFile(t, post, "# Hello, world")
	res, err := svc.Commit(t.Context())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.ChangedFiles != 1 {
		t.Fatalf("changed files = %d, want 1", res.ChangedFiles)
	}
	if !strings.HasPrefix(res.Commit.Message, "Backup content: ") {
		t.Fatalf("message = %q", res.Commit.Message)
	}

	commits, err := svc.History(t.Context(), 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("history = %d commits, want 2", len(commits))
	}
	if commits[0].Hash != res.Commit.Hash {
		t.Fatal("newest commit is not the one just created")
	}
}

func TestCommitRecordsCSSAndJSChanges(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	css := filepath.Join(contentRoot, "system", "markdown", "010-code.css")
	js := filepath.Join(contentRoot, "system", "js", "001-base.js")
	writeFile(t, css, "pre {}")
	writeFile(t, js, "console.log(1)")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	writeFile(t, css, "pre { color: red; }")
	writeFile(t, js, "console.log(2)")
	res, err := svc.Commit(t.Context())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.ChangedFiles != 2 {
		t.Fatalf("changed files = %d, want 2", res.ChangedFiles)
	}
}

func TestCommitRecordsMediaAndDeletion(t *testing.T) {
	svc, contentRoot, mediaRoot := newTestService(t)
	post := filepath.Join(contentRoot, "posts", "with-image.md")
	writeFile(t, post, "![img](/media/2026/10/photo.png)")
	photo := filepath.Join(mediaRoot, "2026", "10", "photo.png")
	writeFile(t, photo, "PNGDATA")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// A new upload enters the same commit timeline as the
	// post that references it.
	writeFile(t, filepath.Join(mediaRoot, "2026", "10", "other.png"), "PNG2")
	if _, err := svc.Commit(t.Context()); err != nil {
		t.Fatalf("commit media: %v", err)
	}

	// Deleting the post records the deletion, and the
	// snapshot no longer holds it.
	if err := os.Remove(post); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Commit(t.Context())
	if err != nil {
		t.Fatalf("commit deletion: %v", err)
	}
	if res.ChangedFiles != 1 {
		t.Fatalf("changed files = %d, want 1", res.ChangedFiles)
	}
	if _, err := os.Stat(filepath.Join(svc.repoPath, "content", "posts", "with-image.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted post still in snapshot: %v", err)
	}
}

// A realistic deployment layout, and the promise that decides what a backup
// holds: only the two source roots are ever read. The database, the session
// secret, the derived WebP cache and the rate-limit rows are not *filtered*
// out — the backup never looks at the data root at all — so there is no filter
// to get wrong.
//
// A draft is an ordinary file with `draft: true` in its frontmatter. It enters
// the backup like everything else: the backup records the site's state, not its
// publication schedule, and a draft is not a branch (§52).
func TestRuntimeStateNeverEntersTheBackup(t *testing.T) {
	base := t.TempDir()
	contentRoot := filepath.Join(base, "content")
	mediaRoot := filepath.Join(base, "media")
	dataRoot := filepath.Join(base, "data")
	repoRoot := filepath.Join(base, "git-backup")
	for _, dir := range []string{
		contentRoot, mediaRoot, dataRoot, repoRoot,
		filepath.Join(dataRoot, "media-cache", "ab"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(Options{
		ContentRoot: contentRoot, MediaRoot: mediaRoot, RepoRoot: repoRoot,
		MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20, Timeout: 30 * time.Second,
	})

	// Source content, in the shapes the loader and the asset managers write.
	included := []string{
		"posts/published.md",
		"posts/a-draft.md",
		"pages/about.md",
		"system/custom.css",
		"system/custom.js",
		"system/css/001-base.css",
		"system/js/001-base.js",
		"system/markdown/020-code.css",
		"system/parked/markdown/020-code.css",
	}
	for _, rel := range included {
		writeFile(t, filepath.Join(contentRoot, filepath.FromSlash(rel)), "x")
	}
	writeFile(t, filepath.Join(mediaRoot, "2026", "10", "photo.jpg"), "JPEG")

	// Everything the architecture calls runtime state.
	for _, rel := range []string{
		"blog.db", "blog.db-wal", "blog.db-shm", "session_secret",
		"media-cache/ab/abcd.webp",
	} {
		writeFile(t, filepath.Join(dataRoot, filepath.FromSlash(rel)), "runtime")
	}

	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	tracked := trackedPaths(t, repoRoot)
	want := make(map[string]bool, len(included)+1)
	for _, rel := range included {
		want["content/"+rel] = true
	}
	want["media/2026/10/photo.jpg"] = true

	for path := range want {
		if !tracked[path] {
			t.Errorf("%s is source content and must be in the backup", path)
		}
	}
	for path := range tracked {
		if !want[path] {
			t.Errorf("%s is in the backup but the policy does not include it", path)
		}
	}

	// The exclusions stated as themselves, so a failure names the rule that
	// broke rather than only the size of the difference.
	for _, absent := range []string{
		"content/blog.db", "content/blog.db-wal", "content/session_secret",
		"content/media-cache/ab/abcd.webp", "content/rate_limit",
		"media/blog.db", "media/media-cache", "media/session_secret",
	} {
		if tracked[absent] {
			t.Errorf("runtime state %s entered the backup", absent)
		}
	}
	// A draft is not a branch and not an omission.
	if !tracked["content/posts/a-draft.md"] {
		t.Error("a draft is an ordinary content file and must be backed up")
	}
}

// A backup reads the source content and writes Git metadata. It does not
// reformat a file, rename one, add frontmatter, or delete anything: the
// bytes on disk after a backup are the bytes that were there before it.
//
// This is asserted against the filesystem rather than by reading the code,
// because the failure it exists to catch is a *write* the code performs —
// `os.WriteFile` inside a sync loop would look correct in every other test.
// Content whose bytes a backup changed would also be content whose history
// lies about what the author wrote.
func TestBackupNeverModifiesSourceContent(t *testing.T) {
	svc, contentRoot, mediaRoot := newTestService(t)
	// Deliberately awkward input: no trailing newline, inconsistent
	// indentation, CRLF, trailing whitespace and a BOM. A backup that
	// "tidied" any of it would change these bytes.
	post := filepath.Join(contentRoot, "posts", "messy.md")
	writeFile(t, post, "\ufeff---\ntitle: Messy\n  slug:  messy\n---\n\n\r\n#  Messy   \n\n\tindented\n")
	writeFile(t, filepath.Join(contentRoot, "pages", "about.md"), "about   \n")
	writeFile(t, filepath.Join(contentRoot, "system", "custom.css"), "body{color:red}   \n")
	writeFile(t, filepath.Join(mediaRoot, "2026", "10", "photo.jpg"), "JPEGBYTES")

	// Each operation is bracketed individually, so the test's own edits are
	// never mistaken for the backup's: only what happened *inside* the call
	// can be attributed to the CMS.
	step := func(label string, fn func() error) {
		t.Helper()
		before := readSourceState(t, contentRoot, mediaRoot)
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		readSourceState(t, contentRoot, mediaRoot).compare(t, "source content", before)
	}

	step("initialize", func() error {
		_, err := svc.Initialize(t.Context())
		return err
	})
	// The read paths sync the snapshot too, so an ordinary screen view
	// counts: the repository is never the only operation checked.
	step("status", func() error {
		_, err := svc.Status(t.Context())
		return err
	})
	step("changes", func() error {
		_, err := svc.Changes(t.Context())
		return err
	})
	step("diff", func() error {
		_, _, err := svc.Diff(t.Context(), "content/posts/messy.md")
		return err
	})
	step("history", func() error {
		_, err := svc.History(t.Context(), 10)
		return err
	})
	step("a backup with nothing to change", func() error {
		if _, err := svc.Commit(t.Context()); !errors.Is(err, ErrNothingToCommit) {
			return fmt.Errorf("commit = %v, want ErrNothingToCommit", err)
		}
		return nil
	})

	// A backup over edited content, and over a deletion: the operations that
	// write the most, so they are the ones worth bracketing.
	writeFile(t, post, "---\ntitle: Messy\nslug: messy\n---\n\n# Messy\n")
	step("commit an edit", func() error {
		_, err := svc.Commit(t.Context())
		return err
	})
	if err := os.Remove(filepath.Join(contentRoot, "pages", "about.md")); err != nil {
		t.Fatal(err)
	}
	step("commit a deletion", func() error {
		_, err := svc.Commit(t.Context())
		return err
	})
}

// compare reports every way got differs from want: a file added, removed,
// rewritten, or had its mode changed.
func (got sourceState) compare(t *testing.T, label string, want sourceState) {
	t.Helper()
	for root, wantTree := range want {
		gotTree, ok := got[root]
		if !ok {
			t.Errorf("%s: %s disappeared", label, root)
			continue
		}
		wantTree.compare(t, root, gotTree)
	}
	for root := range got {
		if _, ok := want[root]; !ok {
			t.Errorf("%s: %s appeared", label, root)
		}
	}
}

// sourceTree is one source root's contents: every file's path, mode and
// bytes. A backup that touched any of them shows up here.
type sourceTree struct {
	files map[string]sourceFile
}

type sourceFile struct {
	mode os.FileMode
	sum  string
}

// compare reports every way got differs from want: a file added, removed,
// rewritten, or had its mode changed.
func (s sourceTree) compare(t *testing.T, root string, got sourceTree) {
	t.Helper()
	for name, want := range s.files {
		have, ok := got.files[name]
		if !ok {
			t.Errorf("a backup removed %s/%s", root, name)
			continue
		}
		if have.sum != want.sum {
			t.Errorf("a backup rewrote %s/%s (%s → %s)", root, name, want.sum, have.sum)
		}
		if have.mode != want.mode {
			t.Errorf("a backup changed the mode of %s/%s (%v → %v)", root, name, want.mode, have.mode)
		}
	}
	for name := range got.files {
		if _, ok := s.files[name]; !ok {
			t.Errorf("a backup created %s/%s", root, name)
		}
	}
}

// sourceState is both source roots read as path → mode and a digest of the
// bytes. A backup that wrote into either root shows up here.
type sourceState map[string]sourceTree

func readSourceState(t *testing.T, roots ...string) sourceState {
	t.Helper()
	out := sourceState{}
	for _, root := range roots {
		tree := sourceTree{files: map[string]sourceFile{}}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			tree.files[filepath.ToSlash(rel)] = sourceFile{
				mode: info.Mode().Perm(),
				sum:  hex.EncodeToString(sum[:8]),
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read source root %s: %v", root, err)
		}
		out[root] = tree
	}
	return out
}

// trackedPaths lists the repository work tree, relative and
// slash-separated, with the repository metadata directory removed.
func trackedPaths(t *testing.T, repoRoot string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == git.GitDirName || strings.HasPrefix(rel, git.GitDirName+"/") {
			return nil
		}
		out[rel] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	return out
}

func TestPolicyExclusions(t *testing.T) {
	svc, contentRoot, mediaRoot := newTestService(t)
	// Included.
	writeFile(t, filepath.Join(contentRoot, "posts", "a.md"), "a")
	writeFile(t, filepath.Join(contentRoot, "pages", "about.md"), "about")
	writeFile(t, filepath.Join(contentRoot, "system", "custom.css"), "body{}")
	writeFile(t, filepath.Join(contentRoot, "system", "custom.js"), "// x")
	writeFile(t, filepath.Join(mediaRoot, "img.png"), "png")
	// Excluded: dotfiles, editor leftovers, unknown content-root
	// directories, and anything that is not a regular file.
	writeFile(t, filepath.Join(contentRoot, "posts", ".DS_Store"), "junk")
	writeFile(t, filepath.Join(contentRoot, "posts", "draft.md.tmp"), "junk")
	writeFile(t, filepath.Join(contentRoot, "posts", "notes.txt~"), "junk")
	writeFile(t, filepath.Join(contentRoot, "notes", "skip.md"), "junk")
	writeFile(t, filepath.Join(mediaRoot, ".hidden"), "junk")
	if err := os.Symlink("/etc", filepath.Join(contentRoot, "posts", "evil")); err != nil {
		t.Skip("symlinks unavailable")
	}

	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	tracked, err := svc.Changes(t.Context())
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(tracked) != 0 {
		t.Fatalf("working tree not clean after backup: %v", tracked)
	}
	for _, path := range []string{
		"content/posts/a.md",
		"content/pages/about.md",
		"content/system/custom.css",
		"content/system/custom.js",
		"media/img.png",
	} {
		if _, err := os.Stat(filepath.Join(svc.repoPath, filepath.FromSlash(path))); err != nil {
			t.Errorf("%s not in snapshot: %v", path, err)
		}
	}
	for _, path := range []string{
		"content/posts/.DS_Store",
		"content/posts/draft.md.tmp",
		"content/posts/notes.txt~",
		"content/notes/skip.md",
		"media/.hidden",
		"content/posts/evil",
	} {
		if _, err := os.Stat(filepath.Join(svc.repoPath, filepath.FromSlash(path))); err == nil {
			t.Errorf("%s leaked into the snapshot", path)
		}
	}
}

func TestFileTooLargeBlocksBackup(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	writeFile(t, filepath.Join(contentRoot, "posts", "big.md"), strings.Repeat("x", 2<<20))
	_, err := svc.Initialize(t.Context())
	var tooLarge *FileTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("initialize = %v, want FileTooLargeError", err)
	}
	if tooLarge.Path != "posts/big.md" {
		t.Fatalf("path = %q", tooLarge.Path)
	}
	// The repository was not initialized by the failed
	// backup: the snapshot sync runs before the first
	// commit.
	if _, err := os.Stat(filepath.Join(svc.repoPath, ".git")); err == nil {
		t.Fatal("a blocked backup initialized the repository")
	}
}

func TestDiffShowsPatchAndEscapesNothing(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	post := filepath.Join(contentRoot, "posts", "a.md")
	writeFile(t, post, "line one\nline two\n")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	writeFile(t, post, "line one\nline TWO\n<script>alert(1)</script>\n")
	change, patch, err := svc.Diff(t.Context(), "content/posts/a.md")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if change.Status != "modified" {
		t.Fatalf("status = %q", change.Status)
	}
	if !strings.Contains(patch, "-line two") || !strings.Contains(patch, "+line TWO") {
		t.Fatalf("patch does not show the change:\n%s", patch)
	}
	// The diff is computed text, never markup: a script in
	// the content must appear verbatim in the patch text
	// (the client escapes it when rendering).
	if !strings.Contains(patch, "<script>alert(1)</script>") {
		t.Fatalf("patch does not contain the new line verbatim:\n%s", patch)
	}
}

func TestDiffNewAndDeletedFiles(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	writeFile(t, filepath.Join(contentRoot, "posts", "keep.md"), "keep")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	writeFile(t, filepath.Join(contentRoot, "posts", "new.md"), "new file\n")
	change, patch, err := svc.Diff(t.Context(), "content/posts/new.md")
	if err != nil {
		t.Fatalf("diff new: %v", err)
	}
	if change.Status != "untracked" {
		t.Fatalf("new file status = %q", change.Status)
	}
	if !strings.Contains(patch, "+new file") {
		t.Fatalf("new file patch:\n%s", patch)
	}

	if err := os.Remove(filepath.Join(contentRoot, "posts", "keep.md")); err != nil {
		t.Fatal(err)
	}
	change, patch, err = svc.Diff(t.Context(), "content/posts/keep.md")
	if err != nil {
		t.Fatalf("diff deleted: %v", err)
	}
	if change.Status != "deleted" {
		t.Fatalf("deleted file status = %q", change.Status)
	}
	if !strings.Contains(patch, "-keep") {
		t.Fatalf("deleted file patch:\n%s", patch)
	}
}

func TestDiffRejectsTraversal(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	for _, path := range []string{
		"../etc/passwd",
		"../../../../etc/passwd",
		"/etc/passwd",
		"content/../../etc/passwd",
		"posts/a.md",       // not under a snapshot directory
		"",                 // empty
		"content",          // a directory, not a file path
		"content/../media", // normalized traversal
	} {
		if _, _, err := svc.Diff(t.Context(), path); err == nil {
			t.Errorf("diff(%q) was accepted", path)
		}
	}
}

func TestStatusReportsState(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)

	// Uninitialized is a normal state, not an error.
	st, err := svc.Status(t.Context())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Initialized {
		t.Fatal("status reports initialized before initialization")
	}

	writeFile(t, filepath.Join(contentRoot, "posts", "a.md"), "a")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	st, err = svc.Status(t.Context())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Initialized || st.Branch == "" || !st.Clean || st.LastCommit == nil {
		t.Fatalf("status = %+v", st)
	}

	writeFile(t, filepath.Join(contentRoot, "posts", "b.md"), "b")
	st, err = svc.Status(t.Context())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Clean || st.ChangedFiles != 1 {
		t.Fatalf("status = clean:%v changed:%d, want dirty with 1 file", st.Clean, st.ChangedFiles)
	}
}

func TestBranchComesFromRepositoryState(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	st, err := svc.Status(t.Context())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// The service was built with no configured branch, so
	// the name came from the repository state — which is
	// the library default here — and never from a constant
	// in this code.
	if st.Branch == "" {
		t.Fatal("branch name is empty")
	}
}

func TestConfiguredBranchIsUsed(t *testing.T) {
	contentRoot := t.TempDir()
	mediaRoot := t.TempDir()
	repoRoot := t.TempDir()
	svc := New(Options{
		ContentRoot:   contentRoot,
		MediaRoot:     mediaRoot,
		RepoRoot:      repoRoot,
		DefaultBranch: "site-content",
		MaxFileBytes:  1 << 20,
		MaxTotalBytes: 8 << 20,
		Timeout:       30 * time.Second,
	})
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	st, err := svc.Status(t.Context())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Branch != "site-content" {
		t.Fatalf("branch = %q, want site-content", st.Branch)
	}
}

func TestHistoryLimit(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(contentRoot, "posts", "p.md"), "rev "+string(rune('a'+i)))
		if _, err := svc.Commit(t.Context()); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	commits, err := svc.History(t.Context(), 3)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(commits) != 3 {
		t.Fatalf("history = %d commits, want 3", len(commits))
	}
}

func TestConcurrentBackupsDoNotCorrupt(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// Phase one: every worker writes its own file,
	// so each change is independently visible.
	const workers = 8
	var write sync.WaitGroup
	for i := 0; i < workers; i++ {
		write.Add(1)
		go func(i int) {
			defer write.Done()
			writeFile(t, filepath.Join(contentRoot, "posts", "c"+string(rune('a'+i))+".md"),
				"rev "+string(rune('a'+i)))
		}(i)
	}
	write.Wait()

	// Phase two: every worker backs up at once. The
	// repository lock serializes them; whichever
	// worker's sync runs first may sweep up files
	// another worker wrote, which is correct — one
	// commit is one consistent snapshot of the whole
	// tree — so a worker can legitimately find
	// nothing left to commit.
	var commit sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		commit.Add(1)
		go func(i int) {
			defer commit.Done()
			_, errs[i] = svc.Commit(t.Context())
		}(i)
	}
	commit.Wait()

	committed := 0
	for _, err := range errs {
		if err == nil {
			committed++
		} else if !errors.Is(err, ErrNothingToCommit) {
			t.Fatalf("concurrent commit: %v", err)
		}
	}
	if committed == 0 {
		t.Fatal("no concurrent commit succeeded")
	}

	// Whatever the interleaving, a further backup now
	// finds nothing to commit: the last sync committed
	// every file that exists.
	if _, err := svc.Commit(t.Context()); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("final commit = %v, want ErrNothingToCommit", err)
	}

	// The repository is still valid, its history is a
	// straight line, and every worker's file is in it.
	commits, err := svc.History(t.Context(), 100)
	if err != nil {
		t.Fatalf("history after concurrency: %v", err)
	}
	if len(commits) < 1 {
		t.Fatal("no commit recorded the concurrent changes")
	}
	// The parent chain must be unbroken: a corrupted
	// repository would fail to walk it.
	repo, err := git.PlainOpen(svc.repoPath)
	if err != nil {
		t.Fatalf("reopen repository: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	walked := 0
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	err = iter.ForEach(func(c *object.Commit) error {
		walked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk history: %v", err)
	}
	if walked != len(commits) {
		t.Fatalf("walked %d commits, history reported %d", walked, len(commits))
	}
	if _, err := svc.Status(t.Context()); err != nil {
		t.Fatalf("status after concurrency: %v", err)
	}
	for i := 0; i < workers; i++ {
		if _, err := os.Stat(filepath.Join(svc.repoPath, "content", "posts", "c"+string(rune('a'+i))+".md")); err != nil {
			t.Errorf("worker file missing from snapshot: %v", err)
		}
	}
}

func TestCommitIdentity(t *testing.T) {
	svc, contentRoot, _ := newTestService(t)
	writeFile(t, filepath.Join(contentRoot, "posts", "a.md"), "a")
	if _, err := svc.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	commits, err := svc.History(t.Context(), 1)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	want := commitName + " <" + commitEmail + ">"
	if commits[0].Author != want {
		t.Fatalf("author = %q, want %q", commits[0].Author, want)
	}
}
