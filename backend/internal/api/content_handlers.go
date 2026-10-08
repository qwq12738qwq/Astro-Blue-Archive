package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"blogcms/internal/content"
	"blogcms/internal/httpx"
)

// PostRequest is the admin payload for creating or updating a post.
//
// Tags arrive as a comma-separated string because that is what a plain text
// input produces; splitting is done here so the frontend does not need to
// serialise an array.
type PostRequest struct {
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
	Date        string `json:"date"`
	Updated     string `json:"updated,omitempty"`
	Tags        string `json:"tags,omitempty"`
	Cover       string `json:"cover,omitempty"`
	Draft       bool   `json:"draft"`
	Body        string `json:"body"`
}

// PageRequest adds navOrder, which only pages may declare. A separate type keeps
// the strict decoder honest instead of silently dropping the field.
type PageRequest struct {
	PostRequest
	NavOrder *int `json:"navOrder"`
}

func (r PageRequest) toEntry() (*content.Entry, error) {
	entry, err := r.PostRequest.toEntry()
	if err != nil {
		return nil, err
	}
	entry.NavOrder = r.NavOrder
	return entry, nil
}

func (r PostRequest) toEntry() (*content.Entry, error) {
	date, err := parseDate(r.Date)
	if err != nil {
		return nil, &content.ValidationError{Fields: map[string]string{"date": "must be an ISO date (YYYY-MM-DD)"}}
	}

	var updated *time.Time
	if r.Updated != "" {
		u, err := parseDate(r.Updated)
		if err != nil {
			return nil, &content.ValidationError{Fields: map[string]string{"updated": "must be an ISO date (YYYY-MM-DD)"}}
		}
		updated = &u
	}

	return &content.Entry{
		Frontmatter: content.Frontmatter{
			Title:       r.Title,
			Slug:        strings.TrimSpace(r.Slug),
			Description: r.Description,
			Date:        &date,
			Updated:     updated,
			Tags:        splitTags(r.Tags),
			Cover:       r.Cover,
			Draft:       r.Draft,
		},
		Body: r.Body,
	}, nil
}

func registerAdminRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/admin/posts", d.requireSession(d.listPosts))
	mux.HandleFunc("POST /api/v1/admin/posts", d.requireSession(d.createPost))
	mux.HandleFunc("POST /api/v1/admin/posts/validate", d.requireSession(d.validateContent))
	mux.HandleFunc("GET /api/v1/admin/posts/{slug}", d.requireSession(d.getPost))
	mux.HandleFunc("PUT /api/v1/admin/posts/{slug}", d.requireSession(d.updatePost))
	mux.HandleFunc("DELETE /api/v1/admin/posts/{slug}", d.requireSession(d.deletePost))

	mux.HandleFunc("GET /api/v1/admin/pages", d.requireSession(d.listPages))
	mux.HandleFunc("POST /api/v1/admin/pages", d.requireSession(d.createPage))
	mux.HandleFunc("GET /api/v1/admin/pages/{slug}", d.requireSession(d.getPage))
	mux.HandleFunc("PUT /api/v1/admin/pages/{slug}", d.requireSession(d.updatePage))
	mux.HandleFunc("DELETE /api/v1/admin/pages/{slug}", d.requireSession(d.deletePage))
}

type listResponse struct {
	Items    []content.Summary `json:"items"`
	Skipped  []string          `json:"skipped"`
	DraftURL string            `json:"-"`
}

func (d Deps) listPosts(w http.ResponseWriter, r *http.Request) {
	d.listContent(w, r, content.KindPosts)
}

func (d Deps) listPages(w http.ResponseWriter, r *http.Request) {
	d.listContent(w, r, content.KindPages)
}

func (d Deps) listContent(w http.ResponseWriter, r *http.Request, kind content.Kind) {
	items, skipped, err := d.Content.List(kind)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if items == nil {
		items = []content.Summary{}
	}
	if skipped == nil {
		skipped = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, listResponse{Items: items, Skipped: skipped})
}

type entryResponse struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Date        string   `json:"date"`
	Updated     string   `json:"updated,omitempty"`
	Tags        string   `json:"tags,omitempty"`
	Cover       string   `json:"cover,omitempty"`
	Draft       bool     `json:"draft"`
	NavOrder    *int     `json:"navOrder,omitempty"`
	Body        string   `json:"body"`
	Path        string   `json:"path"`
	ModifiedAt  string   `json:"modifiedAt"`
	ReadOnly    []string `json:"readOnly,omitempty"`
}

func newEntryResponse(e *content.Entry) entryResponse {
	out := entryResponse{
		Slug:        e.Slug,
		Title:       e.Title,
		Description: e.Description,
		Tags:        strings.Join(e.Tags, ", "),
		Cover:       e.Cover,
		Draft:       e.Draft,
		NavOrder:    e.NavOrder,
		Body:        e.Body,
		Path:        e.RelPath,
		ModifiedAt:  e.ModTime.UTC().Format(time.RFC3339),
	}
	if e.Date != nil {
		out.Date = e.Date.Format("2006-01-02")
	}
	if e.Updated != nil {
		out.Updated = e.Updated.Format("2006-01-02")
	}
	return out
}

func (d Deps) getPost(w http.ResponseWriter, r *http.Request) {
	d.getContent(w, r, content.KindPosts)
}

func (d Deps) getPage(w http.ResponseWriter, r *http.Request) {
	d.getContent(w, r, content.KindPages)
}

func (d Deps) getContent(w http.ResponseWriter, r *http.Request, kind content.Kind) {
	slug := r.PathValue("slug")
	entry, err := d.Content.Get(kind, slug)
	if err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newEntryResponse(entry))
}

func (d Deps) createPost(w http.ResponseWriter, r *http.Request) {
	d.createContent(w, r, content.KindPosts)
}

func (d Deps) createPage(w http.ResponseWriter, r *http.Request) {
	var req PageRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	entry, err := req.toEntry()
	if err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	if err := d.Content.Create(content.KindPages, entry); err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	d.AuditContent(r.Context(), content.KindPages, "created", entry.Slug)
	httpx.WriteJSON(w, http.StatusCreated, newEntryResponse(entry))
}

func (d Deps) createContent(w http.ResponseWriter, r *http.Request, kind content.Kind) {
	var req PostRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	entry, err := req.toEntry()
	if err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	// A page may declare navOrder; posts may not.
	entry.NavOrder = nil

	if err := d.Content.Create(kind, entry); err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}

	d.AuditContent(r.Context(), kind, "created", entry.Slug)
	httpx.WriteJSON(w, http.StatusCreated, newEntryResponse(entry))
}

func (d Deps) updatePost(w http.ResponseWriter, r *http.Request) {
	d.updateContent(w, r, content.KindPosts)
}

func (d Deps) updatePage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var req PageRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	entry, err := req.toEntry()
	if err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	entry.Normalize()
	if fields := entry.Validate(); len(fields) > 0 {
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}
	if entry.Slug == "" {
		entry.Slug = slug
	}
	if entry.Slug != slug {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
			"the slug cannot be changed; create a new page and delete this one"))
		return
	}
	if err := d.Content.Update(content.KindPages, slug, entry); err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	d.AuditContent(r.Context(), content.KindPages, "updated", slug)
	httpx.WriteJSON(w, http.StatusOK, newEntryResponse(entry))
}

func (d Deps) updateContent(w http.ResponseWriter, r *http.Request, kind content.Kind) {
	slug := r.PathValue("slug")

	var req PostRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	entry, err := req.toEntry()
	if err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}

	// ARCHITECTURE.md §9: the slug is the filename. Validate it before anything
	// else so an *invalid* slug is reported as invalid (422) rather than being
	// mistaken for an attempt to rename the post (409).
	entry.Normalize()
	if fields := entry.Validate(); len(fields) > 0 {
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}

	if entry.Slug == "" {
		entry.Slug = slug
	}
	if entry.Slug != slug {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
			"the slug cannot be changed; create a new post and delete this one"))
		return
	}

	if err := d.Content.Update(kind, slug, entry); err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}

	d.AuditContent(r.Context(), kind, "updated", slug)
	httpx.WriteJSON(w, http.StatusOK, newEntryResponse(entry))
}

func (d Deps) deletePost(w http.ResponseWriter, r *http.Request) {
	d.deleteContent(w, r, content.KindPosts)
}

func (d Deps) deletePage(w http.ResponseWriter, r *http.Request) {
	d.deleteContent(w, r, content.KindPages)
}

func (d Deps) deleteContent(w http.ResponseWriter, r *http.Request, kind content.Kind) {
	slug := r.PathValue("slug")
	if err := d.Content.Delete(kind, slug); err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	d.AuditContent(r.Context(), kind, "deleted", slug)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": slug})
}

// validateContent is a dry run used by the editor to show errors before saving.
func (d Deps) validateContent(w http.ResponseWriter, r *http.Request) {
	var req PostRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	entry, err := req.toEntry()
	if err != nil {
		var ve *content.ValidationError
		if errors.As(err, &ve) {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"valid": false, "fields": ve.Fields})
			return
		}
		httpx.WriteError(w, r, err)
		return
	}

	entry.Normalize()
	fields := entry.Validate()
	if len(entry.Body) > content.BodyMaxBytes {
		fields["body"] = "body is too large"
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"valid":  len(fields) == 0,
		"fields": fields,
		"slug":   entry.Slug,
	})
}

// contentError maps content-package errors onto the API error envelope.
func contentError(err error) error {
	var ve *content.ValidationError
	if errors.As(err, &ve) {
		return httpx.ValidationError(ve.Fields)
	}
	switch {
	case errors.Is(err, content.ErrNotFound):
		return httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound, "no such content")
	case errors.Is(err, content.ErrConflict):
		return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "%s", err.Error())
	case errors.Is(err, content.ErrSlugMismatch):
		return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "%s", err.Error())
	default:
		return err
	}
}

func splitTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty date")
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("unparseable date")
}
