// The Git repository manager. It is the only component that
// touches the backup repository, and it is the boundary the HTTP
// layer talks to: handlers never invoke Git themselves.
//
// The repository is a local, single-branch repository whose work
// tree holds a snapshot of the site's source content (content/
// and media/). One backup is one commit on that branch, so the
// commit timeline is the site's version history. Remote push,
// restore and multi-account operation are deliberately absent —
// see PROJECT_STATUS.md for the phase plan.
package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Commit identity. A backup is made by the CMS, not by an admin
// account: the admin's real email never becomes part of the
// repository, so nothing personal leaks into a future remote.
const (
	commitName  = "CMS Backup"
	commitEmail = "backup@local.invalid"
)

var (
	// ErrNotInitialized is returned when the repository has not
	// been created yet.
	ErrNotInitialized = errors.New("backup repository is not initialized")
	// ErrAlreadyInitialized is returned when initializing a
	// repository that already exists.
	ErrAlreadyInitialized = errors.New("backup repository is already initialized")
	// ErrNothingToCommit is returned when the source content has
	// not changed since the last backup. An empty commit would
	// fake a backup, so it is refused.
	ErrNothingToCommit = errors.New("no content changes to back up")
)

// Options configures the service. RepoRoot must be an absolute,
// validated path outside the content and media roots.
type Options struct {
	ContentRoot   string
	MediaRoot     string
	RepoRoot      string
	DefaultBranch string
	MaxFileBytes  int64
	MaxTotalBytes int64
	// Timeout bounds one backup operation. The Git library
	// cannot interrupt an in-flight operation, so the deadline
	// is enforced between phases (sync, stage, commit) and by
	// the caller that owns the request.
	Timeout time.Duration
}

// Service owns the backup repository. Its mutex is the
// repository-level lock: status, stage and commit never run
// concurrently, which is what keeps git's index.lock-free
// operation safe when an admin edits content while another
// admin backs up.
type Service struct {
	repoPath string
	branch   string
	policy   *Policy
	timeout  time.Duration
	mu       sync.Mutex
}

// New builds the backup service. It does not touch the
// filesystem: an uninitialized repository is a normal state,
// not an error.
func New(opts Options) *Service {
	return &Service{
		repoPath: opts.RepoRoot,
		branch:   opts.DefaultBranch,
		policy:   NewPolicy(opts.ContentRoot, opts.MediaRoot, opts.MaxFileBytes, opts.MaxTotalBytes),
		timeout:  opts.Timeout,
	}
}

// Status is the state the admin screen reports.
type Status struct {
	Initialized   bool
	Branch        string
	Clean         bool
	ChangedFiles  int
	LastCommit    *CommitInfo
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// CommitInfo is one entry of the repository's own history.
// Git is the history authority; the CMS never stores a copy
// of this in its database.
type CommitInfo struct {
	Hash    string
	Author  string
	Date    time.Time
	Message string
}

// Change is one pending difference between the source content
// and the last backup.
type Change struct {
	Path   string
	Status string
}

// CommitResult reports what a backup did. Commit is nil
// when an initialization found no content at all: an
// empty site gets an initialized, empty repository,
// never a fabricated empty commit. Branch is the
// repository's current branch, read from its state.
type CommitResult struct {
	Commit       *CommitInfo
	Branch       string
	ChangedFiles int
}

// deadline returns the context deadline for one operation.
func (s *Service) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.timeout > 0 {
		return context.WithTimeout(ctx, s.timeout)
	}
	return context.WithCancel(ctx)
}

// withLock runs fn holding the repository lock, bounded by the
// operation timeout.
func (s *Service) withLock(ctx context.Context, fn func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := s.deadline(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

// openRepo opens an initialized repository.
func (s *Service) openRepo() (*git.Repository, error) {
	repo, err := git.PlainOpen(s.repoPath)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}
	return repo, nil
}

// currentBranch reads the branch name from the repository
// state — HEAD when it is born, the symbolic HEAD otherwise —
// so the name is never assumed by this code.
func currentBranch(repo *git.Repository) (string, error) {
	head, err := repo.Head()
	if err == nil {
		return head.Name().Short(), nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	ref, err := repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return "", fmt.Errorf("read symbolic HEAD: %w", err)
	}
	return ref.Target().Short(), nil
}

// commitInfo converts a commit object into the API shape.
func commitInfo(c *object.Commit) CommitInfo {
	return CommitInfo{
		Hash:    c.Hash.String(),
		Author:  c.Author.Name + " <" + c.Author.Email + ">",
		Date:    c.Author.When,
		Message: c.Message,
	}
}

// Status reports the repository state and the pending
// differences between the source content and the last backup.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	var st *Status
	err := s.withLock(ctx, func(ctx context.Context) error {
		repo, err := s.openRepo()
		if err != nil {
			if errors.Is(err, ErrNotInitialized) {
				st = &Status{
					Initialized:   false,
					MaxFileBytes:  s.policy.MaxFileBytes(),
					MaxTotalBytes: s.policy.MaxTotalBytes(),
				}
				return nil
			}
			return err
		}
		// The snapshot is synchronized before it is read, so
		// the reported changes are exactly what a backup would
		// commit. The snapshot is derived state — syncing it
		// reads the source content and never writes it.
		if err := s.syncSnapshot(ctx); err != nil {
			return err
		}
		return s.refreshStatus(ctx, repo, &st)
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// refreshStatus fills a Status from an open repository. The
// caller holds the repository lock.
func (s *Service) refreshStatus(ctx context.Context, repo *git.Repository, out **Status) error {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("open worktree: %w", err)
	}
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	branch, err := currentBranch(repo)
	if err != nil {
		return err
	}
	st := &Status{
		Initialized:   true,
		Branch:        branch,
		Clean:         status.IsClean(),
		ChangedFiles:  len(status),
		MaxFileBytes:  s.policy.MaxFileBytes(),
		MaxTotalBytes: s.policy.MaxTotalBytes(),
	}
	head, err := repo.Head()
	if err == nil {
		if commit, err := repo.CommitObject(head.Hash()); err == nil {
			info := commitInfo(commit)
			st.LastCommit = &info
		} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return fmt.Errorf("read HEAD commit: %w", err)
		}
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("read HEAD: %w", err)
	}
	*out = st
	return nil
}

// Initialize creates the repository and records the
// current source content as its first commit. The
// initial branch is chosen in this order: the CMS
// configuration, the operator's global Git
// configuration (init.defaultBranch), then the Git
// library's own default.
//
// The snapshot is synchronized before the repository
// is created, so a backup blocked by a size limit
// leaves no half-initialized repository behind. A
// site with no content at all is initialized with an
// empty repository and no commit: an empty commit
// would fake a backup that never happened.
func (s *Service) Initialize(ctx context.Context) (*CommitResult, error) {
	var result *CommitResult
	err := s.withLock(ctx, func(ctx context.Context) error {
		if _, err := os.Stat(filepath.Join(s.repoPath, git.GitDirName)); err == nil {
			return ErrAlreadyInitialized
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := s.syncSnapshot(ctx); err != nil {
			return err
		}

		branch := plumbing.ReferenceName("")
		if s.branch != "" {
			branch = plumbing.NewBranchReferenceName(s.branch)
		} else if cfg, err := config.LoadConfig(config.GlobalScope); err == nil && cfg.Init.DefaultBranch != "" {
			branch = plumbing.NewBranchReferenceName(cfg.Init.DefaultBranch)
		}
		repo, err := git.PlainInitWithOptions(s.repoPath, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: branch},
		})
		if err != nil {
			return fmt.Errorf("initialize repository: %w", err)
		}

		info, _, err := s.commitChanges(ctx, repo, backupMessage(true))
		branchName, berr := currentBranch(repo)
		if berr != nil {
			return berr
		}
		if errors.Is(err, ErrNothingToCommit) {
			result = &CommitResult{Branch: branchName}
			return nil
		}
		if err != nil {
			return err
		}
		result = &CommitResult{Commit: info, Branch: branchName}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Commit synchronizes the snapshot with the source content and
// creates one commit. It refuses to create an empty commit when
// nothing changed.
func (s *Service) Commit(ctx context.Context) (*CommitResult, error) {
	var result *CommitResult
	err := s.withLock(ctx, func(ctx context.Context) error {
		repo, err := s.openRepo()
		if err != nil {
			return err
		}
		if err := s.syncSnapshot(ctx); err != nil {
			return err
		}
		info, changed, err := s.commitChanges(ctx, repo, backupMessage(false))
		if err != nil {
			return err
		}
		branch, err := currentBranch(repo)
		if err != nil {
			return err
		}
		result = &CommitResult{Commit: info, Branch: branch, ChangedFiles: changed}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// commitChanges stages every pending change and commits
// them, reporting how many files the commit covers.
func (s *Service) commitChanges(ctx context.Context, repo *git.Repository, message string) (*CommitInfo, int, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, 0, fmt.Errorf("open worktree: %w", err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return nil, 0, fmt.Errorf("stage changes: %w", err)
	}
	status, err := wt.Status()
	if err != nil {
		return nil, 0, fmt.Errorf("read status: %w", err)
	}
	if status.IsClean() {
		return nil, 0, ErrNothingToCommit
	}
	when := time.Now()
	hash, err := wt.Commit(message, &git.CommitOptions{
		Author:    s.identity(when),
		Committer: s.identity(when),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("create commit: %w", err)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("read new commit: %w", err)
	}
	info := commitInfo(commit)
	return &info, len(status), nil
}

// identity is the fixed commit signature (see commitName).
func (s *Service) identity(when time.Time) *object.Signature {
	return &object.Signature{Name: commitName, Email: commitEmail, When: when}
}

// History returns the repository's own commits, newest first,
// bounded by limit.
func (s *Service) History(ctx context.Context, limit int) ([]CommitInfo, error) {
	var commits []CommitInfo
	err := s.withLock(ctx, func(ctx context.Context) error {
		repo, err := s.openRepo()
		if err != nil {
			return err
		}
		head, err := repo.Head()
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			commits = []CommitInfo{}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read HEAD: %w", err)
		}
		iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
		if err != nil {
			return fmt.Errorf("read history: %w", err)
		}
		defer iter.Close()
		commits = []CommitInfo{}
		return iter.ForEach(func(c *object.Commit) error {
			if len(commits) >= limit {
				// ErrStop ends the iteration
				// without surfacing as an error.
				return storer.ErrStop
			}
			commits = append(commits, commitInfo(c))
			return ctx.Err()
		})
	})
	if err != nil {
		return nil, err
	}
	return commits, nil
}

// Changes lists the pending differences between the source
// content and the last backup. Like Status, it synchronizes
// the snapshot first, so the list is what a backup would
// commit.
func (s *Service) Changes(ctx context.Context) ([]Change, error) {
	var changes []Change
	err := s.withLock(ctx, func(ctx context.Context) error {
		repo, err := s.openRepo()
		if err != nil {
			return err
		}
		if err := s.syncSnapshot(ctx); err != nil {
			return err
		}
		wt, err := repo.Worktree()
		if err != nil {
			return fmt.Errorf("open worktree: %w", err)
		}
		status, err := wt.Status()
		if err != nil {
			return fmt.Errorf("read status: %w", err)
		}
		changes = []Change{}
		for path, file := range status {
			changes = append(changes, Change{Path: path, Status: statusCode(file)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// Diff returns the pending change state of one file and its
// unified diff against the last backup. path is a
// repository-relative path such as "content/posts/a.md".
// The snapshot is synchronized first, so the diff is what a
// backup would commit.
func (s *Service) Diff(ctx context.Context, path string) (*Change, string, error) {
	var change *Change
	var patch string
	err := s.withLock(ctx, func(ctx context.Context) error {
		if !validRepoPath(path) {
			return &InvalidPathError{Path: path}
		}
		repo, err := s.openRepo()
		if err != nil {
			return err
		}
		if err := s.syncSnapshot(ctx); err != nil {
			return err
		}
		wt, err := repo.Worktree()
		if err != nil {
			return fmt.Errorf("open worktree: %w", err)
		}
		status, err := wt.Status()
		if err != nil {
			return fmt.Errorf("read status: %w", err)
		}
		file, ok := status[path]
		if !ok || (file.Staging == unmodified && file.Worktree == unmodified) {
			change = &Change{Path: path, Status: "unmodified"}
			patch = ""
			return nil
		}
		change = &Change{Path: path, Status: statusCode(file)}

		head, err := repo.Head()
		if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("read HEAD: %w", err)
		}
		var oldContent *string
		if err == nil {
			commit, err := repo.CommitObject(head.Hash())
			if err != nil {
				return fmt.Errorf("read HEAD commit: %w", err)
			}
			if f, err := commit.File(path); err == nil {
				contents, err := f.Contents()
				if err != nil {
					return fmt.Errorf("read committed file: %w", err)
				}
				oldContent = &contents
			} else if !errors.Is(err, object.ErrFileNotFound) &&
				!errors.Is(err, object.ErrEntryNotFound) {
				return fmt.Errorf("read committed file: %w", err)
			}
		}
		newContent, err := os.ReadFile(filepath.Join(s.repoPath, filepath.FromSlash(path)))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read worktree file: %w", err)
		}
		patch = buildPatch(path, oldContent, newContent)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return change, patch, nil
}

// validRepoPath confines a diff request to the two snapshot
// directories: the cleaned path must be a file inside one of
// them, so no parent segment can escape the repository work
// tree.
func validRepoPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return strings.HasPrefix(cleaned, "content/") || strings.HasPrefix(cleaned, "media/")
}

// unmodified is the go-git status code for a file with no
// pending change, named here so the comparisons read.
const unmodified = ' '

// statusCode names a pending change the way the admin
// reads it. The worktree state is the interesting one;
// the staging state only matters for something staged
// outside this service, which cannot happen.
func statusCode(file *git.FileStatus) string {
	code := file.Worktree
	if code == unmodified {
		code = file.Staging
	}
	switch code {
	case git.Added:
		return "added"
	case git.Modified:
		return "modified"
	case git.Deleted:
		return "deleted"
	case git.Renamed:
		return "renamed"
	case git.Untracked:
		return "untracked"
	default:
		return "modified"
	}
}
