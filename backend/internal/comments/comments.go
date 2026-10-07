// Package comments implements anonymous commenting.
//
// ARCHITECTURE.md D4 / §14: comment bodies are PLAIN TEXT. There is no Markdown
// subset, no HTML and no rendering here. Astro escapes the text at display time,
// so an anonymous user can never reach set:html, innerHTML or the Markdown
// processor.
//
// ARCHITECTURE.md §8 #13: every submission is length-checked, rate limited,
// honeypot screened and defaulted to `pending`.
package comments

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Field limits (ARCHITECTURE.md §14).
const (
	NicknameMin = 1
	NicknameMax = 60
	BodyMin     = 2
	BodyMax     = 4000

	// MaxLinks bounds link spam without parsing URLs into anything actionable.
	MaxLinks = 3
)

// Statuses.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusSpam     = "spam"
	StatusDeleted  = "deleted"
)

// ValidStatuses is the closed set the moderation API accepts.
var ValidStatuses = map[string]bool{
	StatusPending:  true,
	StatusApproved: true,
	StatusSpam:     true,
	StatusDeleted:  true,
}

// Errors.
var (
	ErrNotFound = errors.New("comment not found")
	ErrNoChange = errors.New("nothing to update")
)

// ValidationError carries per-field messages.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	return "validation failed: " + strings.Join(keys, ", ")
}

// Fields exposes the per-field messages.
func (e *ValidationError) FieldErrors() map[string]string { return e.Fields }

// Comment is one row. `content` is plain text; it is never interpreted.
type Comment struct {
	ID          string
	PostSlug    string
	Nickname    string
	Content     string
	Status      string
	CreatedAt   time.Time
	ModeratedAt *time.Time

	// IPHashLabel and UserAgentSummary are moderation *indicators*, populated only by
	// the admin query. They are derived, never stored: the raw IP is never kept and
	// the raw user agent is not carried across this boundary.
	IPHashLabel      string
	UserAgentSummary string
}

// New is a validated submission.
type New struct {
	PostSlug  string
	Nickname  string
	Content   string
	IPHash    []byte
	UserAgent string
}

// Validate checks and normalises a submission.
//
// Normalisation is deliberately conservative: control characters are removed and
// whitespace runs collapsed, while line breaks are kept so multi-line comments
// survive. Nothing is HTML-escaped here, because the value is never placed into
// markup — Astro escapes it when rendering.
func (n *New) Validate() error {
	fields := map[string]string{}

	n.PostSlug = strings.TrimSpace(n.PostSlug)
	if n.PostSlug == "" {
		fields["postSlug"] = "required"
	} else if len(n.PostSlug) > 80 {
		fields["postSlug"] = "too long"
	}

	// Length is checked against the ORIGINAL input. Truncating first would let an
	// oversized value silently become an acceptable one.
	rawNickname := n.Nickname
	rawContent := n.Content

	n.Nickname = clean(rawNickname, NicknameMax*4)
	n.Content = clean(rawContent, BodyMax*4)

	switch {
	case utf8.RuneCountInString(rawNickname) == 0:
		fields["nickname"] = "required"
	case utf8.RuneCountInString(rawNickname) > NicknameMax:
		fields["nickname"] = fmt.Sprintf("must be at most %d characters", NicknameMax)
	}

	switch {
	case utf8.RuneCountInString(rawContent) < BodyMin:
		fields["content"] = fmt.Sprintf("must be at least %d characters", BodyMin)
	case utf8.RuneCountInString(rawContent) > BodyMax:
		fields["content"] = fmt.Sprintf("must be at most %d characters", BodyMax)
	}

	if n.Content != "" && countLinks(n.Content) > MaxLinks {
		fields["content"] = fmt.Sprintf("may contain at most %d links", MaxLinks)
	}

	if n.UserAgent == "" {
		n.UserAgent = "-"
	}
	if len(n.UserAgent) > 255 {
		n.UserAgent = n.UserAgent[:255]
	}

	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// clean removes control characters and collapses whitespace runs, keeping line
// breaks. hardCap bounds the work done on pathological input; it sits far above
// the real limits, so anything it truncates is rejected by Validate anyway.
func clean(s string, hardCap int) string {
	var b strings.Builder
	b.Grow(len(s))

	pendingSpace := false
	newlines := 0
	written := 0

	for _, r := range s {
		if written >= hardCap {
			break
		}
		switch {
		case r == '\n':
			if pendingSpace {
				b.WriteByte(' ')
				written++
				pendingSpace = false
			}
			// Keep at most one blank line, so a comment stays readable without
			// carrying whatever spacing the submitter happened to type.
			if newlines < 2 {
				b.WriteByte('\n')
				written++
				newlines++
			}
		case unicode.IsSpace(r):
			// Collapse any run of spaces and tabs into one pending space.
			pendingSpace = true
			newlines = 0
		case unicode.IsControl(r):
			// Dropped entirely: these carry no meaning in a comment and can be
			// used to confuse logs or terminals.
		default:
			if pendingSpace {
				b.WriteByte(' ')
				written++
				pendingSpace = false
			}
			b.WriteRune(r)
			written++
			newlines = 0
		}
	}
	if pendingSpace {
		b.WriteByte(' ')
	}

	return strings.TrimSpace(b.String())
}

// countLinks counts occurrences that look like links, without parsing them.
func countLinks(s string) int {
	lower := strings.ToLower(s)
	count := 0
	for _, needle := range []string{"http://", "https://", "www.", "[url", "[/url", "mailto:"} {
		count += strings.Count(lower, needle)
	}
	return count
}

// Store persists comments.
type Store struct {
	db dbLike
}

type dbLike interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// NewStore builds a Store.
func NewStore(db dbLike) *Store { return &Store{db: db} }

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Create inserts a comment with the given status.
func (s *Store) Create(ctx context.Context, n New, status string) (*Comment, error) {
	if !ValidStatuses[status] {
		return nil, fmt.Errorf("comments: invalid status %q", status)
	}
	if err := n.Validate(); err != nil {
		return nil, err
	}

	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}

	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO comment (id, post_slug, nickname, content, status, created_at, ip_hash, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, n.PostSlug, n.Nickname, n.Content, status,
		now.Format(time.RFC3339), n.IPHash, n.UserAgent,
	); err != nil {
		return nil, err
	}

	return &Comment{
		ID:        id,
		PostSlug:  n.PostSlug,
		Nickname:  n.Nickname,
		Content:   n.Content,
		Status:    status,
		CreatedAt: now,
	}, nil
}

// ListForPost returns the approved comments on a post, oldest first.
//
// ARCHITECTURE.md §15: the public listing is hard-wired to `approved` in the
// query, so no request parameter can widen it.
func (s *Store) ListForPost(ctx context.Context, slug string, limit int) ([]Comment, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, post_slug, nickname, content, status, created_at, moderated_at
		   FROM comment
		  WHERE post_slug = ? AND status = 'approved'
		  ORDER BY created_at ASC, id ASC
		  LIMIT ?`, slug, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanAll(rows)
}

// ListForAdmin returns comments matching the filters, newest first.
//
// An empty status means "all" and an empty postSlug means "any post". `search`
// matches a substring of the nickname or the body; the caller has already escaped
// the LIKE wildcards in it (see likeParam in the api package), because a search for
// `100%` must find a literal `100%` and not everything.
func (s *Store) ListForAdmin(ctx context.Context, status, postSlug, search string, limit, offset int) ([]Comment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	q := `SELECT id, post_slug, nickname, content, status, created_at, moderated_at, ip_hash, user_agent
	        FROM comment WHERE 1 = 1`
	var args []any
	if ValidStatuses[status] {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if postSlug != "" {
		q += ` AND post_slug = ?`
		args = append(args, postSlug)
	}
	if search != "" {
		q += ` AND (nickname LIKE ? ESCAPE '\' OR content LIKE ? ESCAPE '\')`
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanAdminAll(rows)
}

// CountByStatus returns how many comments sit in each status.
func (s *Store) CountByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM comment GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// CountPending returns the moderation backlog size.
func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM comment WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// Get returns one comment regardless of status (admin use).
func (s *Store) Get(ctx context.Context, id string) (*Comment, error) {
	var c Comment
	var created, moderated sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, post_slug, nickname, content, status, created_at, moderated_at
		   FROM comment WHERE id = ?`, id).
		Scan(&c.ID, &c.PostSlug, &c.Nickname, &c.Content, &c.Status, &created, &moderated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, created.String); err == nil {
		c.CreatedAt = t
	}
	if moderated.Valid {
		if t, err := time.Parse(time.RFC3339, moderated.String); err == nil {
			c.ModeratedAt = &t
		}
	}
	return &c, nil
}

// SetStatus moves a comment between moderation states.
func (s *Store) SetStatus(ctx context.Context, id, status string) error {
	if !ValidStatuses[status] {
		return fmt.Errorf("comments: invalid status %q", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE comment SET status = ?, moderated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a comment outright.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM comment WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecentIPHashes returns the IP hashes that commented on a post within the
// window, used for rate limiting and duplicate detection.
func (s *Store) RecentIPHashes(ctx context.Context, postSlug string, since time.Time) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ip_hash FROM comment
		  WHERE post_slug = ? AND created_at >= ? AND ip_hash IS NOT NULL`,
		postSlug, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanAll(rows *sql.Rows) ([]Comment, error) {
	out := []Comment{}
	for rows.Next() {
		var c Comment
		var created, moderated sql.NullString
		if err := rows.Scan(&c.ID, &c.PostSlug, &c.Nickname, &c.Content, &c.Status,
			&created, &moderated); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, created.String); err == nil {
			c.CreatedAt = t
		}
		if moderated.Valid {
			if t, err := time.Parse(time.RFC3339, moderated.String); err == nil {
				c.ModeratedAt = &t
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanAdminAll is scanAll plus the two moderation signals.
//
// ARCHITECTURE.md §43: the moderation queue needs to be able to tell "the same
// address keeps posting" and "this arrived from a browser". It never needs the raw
// address, so only the *indicator* crosses this boundary — see IPHashLabel.
func scanAdminAll(rows *sql.Rows) ([]Comment, error) {
	out := []Comment{}
	for rows.Next() {
		var c Comment
		var created, moderated sql.NullString
		var ipHash []byte
		var userAgent string
		if err := rows.Scan(&c.ID, &c.PostSlug, &c.Nickname, &c.Content, &c.Status,
			&created, &moderated, &ipHash, &userAgent); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, created.String); err == nil {
			c.CreatedAt = t
		}
		if moderated.Valid {
			if t, err := time.Parse(time.RFC3339, moderated.String); err == nil {
				c.ModeratedAt = &t
			}
		}
		c.IPHashLabel = IPHashLabel(ipHash)
		c.UserAgentSummary = SummariseUserAgent(userAgent)
		out = append(out, c)
	}
	return out, rows.Err()
}

// IPHashLabel renders a short, non-reversible label for a stored IP hash.
//
// ARCHITECTURE.md §43: the moderation screen shows whether a comment shares an
// address with others, and must not show the address. Four hex characters of a
// keyed hash are enough to compare within this site and useless for recovering
// anything. An absent hash renders as "unknown" rather than an empty cell, because
// a blank reads as "not collected" and the truth is "not available".
func IPHashLabel(hash []byte) string {
	if len(hash) == 0 {
		return "unknown"
	}
	const hexdigits = "0123456789abcdef"
	n := len(hash)
	if n > 4 {
		n = 4
	}
	out := make([]byte, 0, n*2)
	for _, b := range hash[:n] {
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return "#" + string(out)
}

// SummariseUserAgent reduces a user agent string to a short, safe label.
//
// The point is to distinguish "a phone", "a desktop browser" and "a bot" at a
// glance. The full string is not rendered: it is unbounded attacker-controlled text
// in a table cell, and everything a moderator needs from it is its first token plus
// its platform hint.
func SummariseUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" || ua == "-" {
		return "unknown"
	}
	label := ua
	if idx := strings.IndexAny(label, " ("); idx > 0 {
		label = label[:idx]
	}
	if len(label) > 48 {
		label = label[:48]
	}
	return label
}
