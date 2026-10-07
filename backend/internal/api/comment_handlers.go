package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"blogcms/internal/comments"
	"blogcms/internal/content"
	"blogcms/internal/httpx"
)

func registerCommentsRoutes(mux *http.ServeMux, d Deps) {
	// Public
	mux.HandleFunc("GET /api/v1/comments", d.listPublicComments)
	mux.HandleFunc("POST /api/v1/comments", d.createComment)

	// Admin
	mux.HandleFunc("GET /api/v1/admin/comments", d.requireSession(d.listAdminComments))
	mux.HandleFunc("PATCH /api/v1/admin/comments/{id}", d.requireSession(d.moderateComment))
	mux.HandleFunc("DELETE /api/v1/admin/comments/{id}", d.requireSession(d.deleteComment))
}

// createCommentRequest is the anonymous submission payload.
//
// honeypot and renderedAt are anti-spam signals, not content. A real browser
// fills the honeypot (which is hidden from humans) and takes more than a moment
// to render and type.
type createCommentRequest struct {
	PostSlug string `json:"postSlug"`
	Nickname string `json:"nickname"`
	Content  string `json:"content"`
	Honeypot string `json:"honeypot"`
	Started  int64  `json:"startedAt"`
}

type commentResponse struct {
	ID        string `json:"id"`
	PostSlug  string `json:"postSlug"`
	Nickname  string `json:"nickname"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

func newCommentResponse(c comments.Comment) commentResponse {
	return commentResponse{
		ID:        c.ID,
		PostSlug:  c.PostSlug,
		Nickname:  c.Nickname,
		Content:   c.Content,
		Status:    c.Status,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// createComment accepts an anonymous comment.
func (d Deps) createComment(w http.ResponseWriter, r *http.Request) {
	// §13: a cookie-bearing cross-site POST must not be possible.
	if !d.Auth.VerifyOrigin(r) {
		d.refuseOrigin(w, r)
		return
	}

	var req createCommentRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	ipHash := d.Auth.HashIP(httpx.ClientIP(r))

	// §8 #13: per-IP submission limit. The honeypot and timing checks happen
	// before the database write so a bot costs nothing.
	if !d.CommentsEnabled(r.Context()) {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
			"comments are closed"))
		return
	}

	decision, err := d.CommentLimiter.Allow(r.Context(), hexKey(ipHash))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !decision.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(decision.RetryIn.Seconds())+1))
		httpx.WriteError(w, r, httpx.Errorf(http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many comments from this address; try again later"))
		return
	}

	if req.Honeypot != "" {
		// Report success so a bot learns nothing, but store nothing.
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{
			"id":     "queued",
			"status": comments.StatusPending,
		})
		return
	}

	// A form completed faster than a human could plausibly type is a bot.
	if req.Started > 0 && time.Since(time.UnixMilli(req.Started)) < minCommentFillTime {
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{
			"id":     "queued",
			"status": comments.StatusPending,
		})
		return
	}

	// The post must actually exist. Go reads the filesystem, so the slug is
	// validated against the real file rather than a database row.
	if err := content.ValidateSlug(req.PostSlug); err != nil {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{"postSlug": err.Error()}))
		return
	}
	if _, err := d.Content.Get(content.KindPosts, req.PostSlug); err != nil {
		if errors.Is(err, content.ErrNotFound) {
			httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
				"postSlug": "no such post",
			}))
			return
		}
		httpx.WriteError(w, r, err)
		return
	}

	newComment := comments.New{
		PostSlug:  req.PostSlug,
		Nickname:  req.Nickname,
		Content:   req.Content,
		IPHash:    ipHash,
		UserAgent: r.UserAgent(),
	}

	status := comments.StatusPending
	if !d.AutoModerate(r.Context()) {
		status = comments.StatusApproved
	}

	created, err := d.Comments.Create(r.Context(), newComment, status)
	if err != nil {
		var ve *comments.ValidationError
		if errors.As(err, &ve) {
			httpx.WriteError(w, r, httpx.ValidationError(ve.FieldErrors()))
			return
		}
		httpx.WriteError(w, r, err)
		return
	}

	d.Audit(r.Context(), "comment.created", created.ID)

	// The response echoes only what a public client may see: no IP hash, no
	// user agent, and the plain-text body unchanged.
	httpx.WriteJSON(w, http.StatusCreated, newCommentResponse(*created))
}

// minCommentFillTime is the shortest plausible gap between opening the form and
// submitting it.
const minCommentFillTime = 2 * time.Second

// listPublicComments returns approved comments for one post.
//
// ARCHITECTURE.md §15: the status filter is hard-wired to `approved` inside the
// store query, so no request parameter can widen it.
func (d Deps) listPublicComments(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("post"))
	if slug == "" {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"a 'post' query parameter is required"))
		return
	}
	if err := content.ValidateSlug(slug); err != nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"invalid post slug"))
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	items, err := d.Comments.ListForPost(r.Context(), slug, limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	out := make([]commentResponse, 0, len(items))
	for _, c := range items {
		out = append(out, newCommentResponse(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// adminCommentResponse is the moderation view of one comment.
//
// ARCHITECTURE.md §43: it adds the two indicators a moderator needs and nothing
// else. The raw IP is never stored and never returned; the user agent crosses the
// boundary only as a short summary. A password, a session token or a CSRF secret has
// no representation here at all.
type adminCommentResponse struct {
	commentResponse
	Excerpt          string `json:"excerpt"`
	IPHashIndicator  string `json:"ipHashIndicator"`
	UserAgentSummary string `json:"userAgentSummary"`
	ModeratedAt      string `json:"moderatedAt,omitempty"`
	PostHref         string `json:"postHref"`
}

// commentExcerptLength bounds the preview in the list view.
//
// The full body is already in the response for the detail view; this is the
// collapsed cell. It is cut on a rune boundary and a marker is appended so a
// truncated cell cannot be mistaken for the whole comment.
const commentExcerptLength = 160

func excerpt(s string) string {
	runes := []rune(s)
	if len(runes) <= commentExcerptLength {
		return s
	}
	cut := commentExcerptLength
	for cut > 0 && !unicode.IsSpace(rune(runes[cut-1])) {
		cut--
	}
	if cut < 40 {
		cut = commentExcerptLength
	}
	return strings.TrimSpace(string(runes[:cut])) + "…"
}

func (d Deps) listAdminComments(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	post := strings.TrimSpace(r.URL.Query().Get("post"))
	search := strings.TrimSpace(r.URL.Query().Get("q"))

	if status != "" && !comments.ValidStatuses[status] {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"unknown status filter"))
		return
	}
	if n := len([]rune(search)); n > 100 {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"q must be at most 100 characters"))
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	items, err := d.Comments.ListForAdmin(r.Context(), status, post, likeEscape(search), limit, offset)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	counts, err := d.Comments.CountByStatus(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	out := make([]adminCommentResponse, 0, len(items))
	for _, c := range items {
		item := adminCommentResponse{
			commentResponse:  newCommentResponse(c),
			Excerpt:          excerpt(c.Content),
			IPHashIndicator:  c.IPHashLabel,
			UserAgentSummary: c.UserAgentSummary,
			PostHref:         "/admin/posts/" + c.PostSlug,
		}
		if c.ModeratedAt != nil {
			item.ModeratedAt = c.ModeratedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":   out,
		"counts":  counts,
		"pending": counts[comments.StatusPending],
	})
}

func (d Deps) moderateComment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !comments.ValidStatuses[body.Status] {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"status must be one of pending, approved, spam, deleted"))
		return
	}

	id := r.PathValue("id")
	if err := d.Comments.SetStatus(r.Context(), id, body.Status); err != nil {
		if errors.Is(err, comments.ErrNotFound) {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
				"no such comment"))
			return
		}
		httpx.WriteError(w, r, err)
		return
	}

	d.Audit(r.Context(), "comment."+body.Status, id)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "status": body.Status})
}

func (d Deps) deleteComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := d.Comments.Delete(r.Context(), id); err != nil {
		if errors.Is(err, comments.ErrNotFound) {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
				"no such comment"))
			return
		}
		httpx.WriteError(w, r, err)
		return
	}
	d.Audit(r.Context(), "comment.deleted", id)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// hexKey renders an IP hash as a stable rate-limit bucket key.
func hexKey(h []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(h)*2)
	for _, b := range h {
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out)
}
