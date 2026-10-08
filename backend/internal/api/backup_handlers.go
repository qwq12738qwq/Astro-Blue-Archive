package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"blogcms/internal/backup"
	"blogcms/internal/httpx"
)

// The backup API exposes the local Git repository: its
// state, its history and the pending differences between
// the source content and the last backup. Every endpoint
// goes through requireSession, which enforces the session,
// the Origin check and the CSRF double-submit header.
//
// "Backup" here means a local commit. Push to a remote is
// a separate, later phase and is deliberately not offered.

// backupCommit is one repository commit, as the admin
// screen reads it.
type backupCommit struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

// backupStatus is the repository state.
type backupStatus struct {
	Initialized   bool          `json:"initialized"`
	Branch        string        `json:"branch"`
	Clean         bool          `json:"clean"`
	ChangedFiles  int           `json:"changedFiles"`
	LastCommit    *backupCommit `json:"lastCommit"`
	MaxFileBytes  int64         `json:"maxFileBytes"`
	MaxTotalBytes int64         `json:"maxTotalBytes"`
}

// backupHistory is a bounded slice of the repository's
// own history. Git is the history authority; the CMS
// stores no copy of it.
type backupHistory struct {
	Commits []*backupCommit `json:"commits"`
}

// backupChange is one pending difference.
type backupChange struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// backupChanges lists pending differences.
type backupChanges struct {
	Changes []backupChange `json:"changes"`
}

// backupDiff is one file's pending change and its unified
// diff. The patch is plain text and is escaped by the
// client like any other untrusted value: it is computed
// from admin-authored Markdown, CSS and JavaScript.
type backupDiff struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Patch  string `json:"patch"`
}

// backupCommitResult reports what a backup did.
type backupCommitResult struct {
	Branch       string        `json:"branch"`
	Commit       *backupCommit `json:"commit"`
	ChangedFiles int           `json:"changedFiles"`
}

func registerBackupRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/admin/backup", d.requireSession(d.getBackupStatus))
	mux.HandleFunc("POST /api/v1/admin/backup/initialize", d.requireSession(d.initializeBackup))
	mux.HandleFunc("POST /api/v1/admin/backup/commit", d.requireSession(d.commitBackup))
	mux.HandleFunc("GET /api/v1/admin/backup/history", d.requireSession(d.getBackupHistory))
	mux.HandleFunc("GET /api/v1/admin/backup/changes", d.requireSession(d.getBackupChanges))
	mux.HandleFunc("GET /api/v1/admin/backup/diff", d.requireSession(d.getBackupDiff))
}

func (d Deps) getBackupStatus(w http.ResponseWriter, r *http.Request) {
	st, err := d.Backup.Status(r.Context())
	if err != nil {
		backupError(w, r, err)
		return
	}
	var lastCommit *backupCommit
	if st.LastCommit != nil {
		lastCommit = toBackupCommit(st.LastCommit)
	}
	httpx.WriteJSON(w, http.StatusOK, backupStatus{
		Initialized:   st.Initialized,
		Branch:        st.Branch,
		Clean:         st.Clean,
		ChangedFiles:  st.ChangedFiles,
		LastCommit:    lastCommit,
		MaxFileBytes:  st.MaxFileBytes,
		MaxTotalBytes: st.MaxTotalBytes,
	})
}

func (d Deps) initializeBackup(w http.ResponseWriter, r *http.Request) {
	res, err := d.Backup.Initialize(r.Context())
	if err != nil {
		backupError(w, r, err)
		return
	}
	// The audit reference is the commit when one was
	// created, and the branch when an empty site was
	// initialized without one.
	ref := res.Branch
	if res.Commit != nil {
		ref = res.Commit.Hash
	}
	d.Audit(r.Context(), "git_backup.initialize", ref)
	httpx.WriteJSON(w, http.StatusOK, backupCommitResult{
		Branch: res.Branch,
		Commit: toBackupCommit(res.Commit),
	})
}

func (d Deps) commitBackup(w http.ResponseWriter, r *http.Request) {
	res, err := d.Backup.Commit(r.Context())
	if err != nil {
		backupError(w, r, err)
		return
	}
	d.Audit(r.Context(), "git_backup.commit", res.Commit.Hash)
	httpx.WriteJSON(w, http.StatusOK, backupCommitResult{
		Branch:       res.Branch,
		Commit:       toBackupCommit(res.Commit),
		ChangedFiles: res.ChangedFiles,
	})
}

func (d Deps) getBackupHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
				"limit must be between 1 and 100"))
			return
		}
		limit = n
	}
	commits, err := d.Backup.History(r.Context(), limit)
	if err != nil {
		backupError(w, r, err)
		return
	}
	out := make([]*backupCommit, 0, len(commits))
	for i := range commits {
		out = append(out, toBackupCommit(&commits[i]))
	}
	httpx.WriteJSON(w, http.StatusOK, backupHistory{Commits: out})
}

func (d Deps) getBackupChanges(w http.ResponseWriter, r *http.Request) {
	changes, err := d.Backup.Changes(r.Context())
	if err != nil {
		backupError(w, r, err)
		return
	}
	out := make([]backupChange, 0, len(changes))
	for _, change := range changes {
		out = append(out, backupChange{Path: change.Path, Status: change.Status})
	}
	httpx.WriteJSON(w, http.StatusOK, backupChanges{Changes: out})
}

func (d Deps) getBackupDiff(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"path is required"))
		return
	}
	change, patch, err := d.Backup.Diff(r.Context(), path)
	if err != nil {
		backupError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, backupDiff{
		Path:   change.Path,
		Status: change.Status,
		Patch:  patch,
	})
}

// toBackupCommit converts a commit into the API shape.
// A nil commit — an initialized but empty repository —
// converts to a nil commit.
func toBackupCommit(in *backup.CommitInfo) *backupCommit {
	if in == nil {
		return nil
	}
	return &backupCommit{
		Hash:    in.Hash,
		Author:  in.Author,
		Date:    in.Date.UTC().Format(time.RFC3339),
		Message: in.Message,
	}
}

// backupError maps backup failures onto the uniform error
// envelope. The Git library's own messages never reach the
// client: only a safe code and a generic message do, and the
// real error is logged server-side where an operator can read
// it (ARCHITECTURE.md §40).
func backupError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		fileTooLarge  *backup.FileTooLargeError
		totalTooLarge *backup.TotalTooLargeError
		invalidPath   *backup.InvalidPathError
	)
	switch {
	case errors.Is(err, backup.ErrNotInitialized):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "backup_not_initialized",
			"Initialize the backup repository first."))
	case errors.Is(err, backup.ErrAlreadyInitialized):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "backup_already_initialized",
			"The backup repository is already initialized."))
	case errors.Is(err, backup.ErrNothingToCommit):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "backup_nothing_to_commit",
			"No content changes to back up."))
	case errors.As(err, &fileTooLarge):
		httpx.WriteError(w, r, &httpx.APIError{
			Status:  http.StatusUnprocessableEntity,
			Code:    "backup_file_too_large",
			Message: "A content file exceeds the per-file backup limit.",
			Fields: map[string]string{
				"file":  fileTooLarge.Path,
				"size":  strconv.FormatInt(fileTooLarge.Size, 10),
				"limit": strconv.FormatInt(fileTooLarge.Limit, 10),
			},
		})
	case errors.As(err, &totalTooLarge):
		httpx.WriteError(w, r, &httpx.APIError{
			Status:  http.StatusUnprocessableEntity,
			Code:    "backup_too_large",
			Message: "The content tree exceeds the total backup limit.",
			Fields: map[string]string{
				"total": strconv.FormatInt(totalTooLarge.Total, 10),
				"limit": strconv.FormatInt(totalTooLarge.Limit, 10),
			},
		})
	case errors.As(err, &invalidPath):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, "backup_invalid_path",
			"Not a backup path."))
	case errors.Is(err, context.DeadlineExceeded):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusGatewayTimeout, "backup_timeout",
			"The backup operation timed out."))
	default:
		slog.Error("backup operation failed",
			"request_id", httpx.RequestIDFrom(r.Context()),
			"method", r.Method, "path", r.URL.Path, "error", err)
		httpx.WriteError(w, r, httpx.Errorf(http.StatusInternalServerError, "backup_failed",
			"Backup operation failed."))
	}
}
