package backup

import (
	"fmt"
	"time"
)

// FileTooLargeError blocks a backup because one source
// file exceeds the per-file ceiling. The backup is
// refused rather than the file silently skipped: the
// admin learns which file is too large and why.
type FileTooLargeError struct {
	Path  string
	Size  int64
	Limit int64
}

func (e *FileTooLargeError) Error() string {
	return fmt.Sprintf("%s is %s, over the %s per-file backup limit",
		e.Path, formatBytes(e.Size), formatBytes(e.Limit))
}

// TotalTooLargeError blocks a backup whose source content
// would exceed the overall ceiling.
type TotalTooLargeError struct {
	Path  string
	Total int64
	Limit int64
}

func (e *TotalTooLargeError) Error() string {
	return fmt.Sprintf("backup would exceed the %s total limit at %s (while reading %s)",
		formatBytes(e.Limit), formatBytes(e.Total), e.Path)
}

// InvalidPathError reports a repository-relative path
// that is not a valid backup path.
type InvalidPathError struct {
	Path string
}

func (e *InvalidPathError) Error() string {
	return fmt.Sprintf("invalid backup path %q", e.Path)
}

// formatBytes renders a limit the way the admin reads it.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// backupMessage builds the commit message. The format
// lives here and nowhere else, so the history reads
// consistently and the wording cannot drift between
// call sites.
func backupMessage(initial bool) string {
	if initial {
		return "Initial content backup"
	}
	return "Backup content: " + time.Now().UTC().Format("2006-01-02 15:04")
}
