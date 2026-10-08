package api

// Markdown style template management.
//
// ARCHITECTURE.md §34: `content/system/markdown/NNN-name.css`
// files are the presentation layer for rendered Markdown content.
// The division of labour is the custom-asset manager's, with the
// type dimension removed because every template is CSS:
//
//   - Go writes, validates, orders, audits and holds the metadata.
//   - The database stores metadata only. There is no body column,
//     and losing the table costs one rescan of one directory.
//   - Astro aggregates the directory at request time to answer
//     /markdown.css, so nothing here has to be "published" and no
//     build is involved.
//
// Every route goes through `requireSession`, which is also where
// the Origin check and the CSRF header live: writing CSS is a
// write, and a 512 KB stylesheet is still a write.
import (
	"context"
	"errors"
	"net/http"

	"blogcms/internal/content"
	"blogcms/internal/httpx"
	"blogcms/internal/store"
)

// markdownTemplateBodyLimit is the JSON body ceiling for these
// routes. See customAssetBodyLimit: the 512 KiB template ceiling
// and the general 1 MiB JSON limit only fit together because JSON
// escaping expands, so these routes ask for room for the largest
// legal body plus its worst case.
func markdownTemplateBodyLimit(cfgMax int64) int64 {
	worst := int64(content.BodyMaxBytes) * 6 // JSON escapes a byte as six
	if cfgMax >= worst {
		return cfgMax
	}
	return worst
}

// MarkdownTemplateView is the API shape of one template.
//
// There is no path field, absolute or relative: ID-42 — a public or
// admin response must not tell a caller where CONTENT_ROOT is or
// how it is laid out. `id` is the filename, which is the identity
// the filesystem already uses.
type MarkdownTemplateView struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	Enabled   bool   `json:"enabled"`
	Order     int    `json:"order"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	// Status is `ok`, `missing` or `invalid`. Anything other
	// than `ok` is not served, and says why in Problem.
	Status  string `json:"status"`
	Problem string `json:"problem,omitempty"`
}

// markdownTemplateListResponse carries the list plus the limits an
// editor needs to validate before the server does.
type markdownTemplateListResponse struct {
	Templates []MarkdownTemplateView `json:"templates"`
	MaxBytes  int                    `json:"maxBytes"`
}

// markdownTemplateContentResponse is one template plus its body.
type markdownTemplateContentResponse struct {
	MarkdownTemplateView
	Content string `json:"content"`
}

// markdownTemplateRequest creates or updates one template.
//
// Every field is a pointer. §27's rule: an omitted field means
// "leave it alone", so saving a renamed file must not blank its
// body, and toggling `enabled` must not require resending 512 KB
// of text the server already has. An explicit `false` is a value
// and is applied.
type markdownTemplateRequest struct {
	Filename *string `json:"filename"`
	Content  *string `json:"content"`
	Enabled  *bool   `json:"enabled"`
}

func registerMarkdownTemplateRoutes(mux *http.ServeMux, d Deps) {
	base := "/api/v1/admin/markdown"
	mux.HandleFunc("GET "+base, d.requireSession(d.listMarkdownTemplates))
	mux.HandleFunc("POST "+base, d.requireSession(d.createMarkdownTemplate))
	mux.HandleFunc("GET "+base+"/{id}", d.requireSession(d.getMarkdownTemplate))
	mux.HandleFunc("PUT "+base+"/{id}", d.requireSession(d.updateMarkdownTemplate))
	mux.HandleFunc("DELETE "+base+"/{id}", d.requireSession(d.deleteMarkdownTemplate))
}

// markdownTemplateError maps content-package errors onto the API
// error envelope.
func markdownTemplateError(err error) error {
	var ve *content.ValidationError
	if errors.As(err, &ve) {
		return httpx.ValidationError(ve.Fields)
	}
	switch {
	case errors.Is(err, content.ErrInvalidMarkdownTemplateName):
		return httpx.Errorf(http.StatusUnprocessableEntity, httpx.CodeValidation, "%s", err.Error())
	case errors.Is(err, content.ErrMarkdownTemplateNotFound):
		return httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound, "no such markdown template")
	case errors.Is(err, content.ErrConflict):
		return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "%s", err.Error())
	default:
		return err
	}
}

// markdownTemplateViews merges the filesystem and the metadata
// index into the list the admin sees, reconciling the index on the
// way through.
//
// The filesystem is read first and is authoritative; the index only
// adds the one thing the tree cannot say, which is when an admin
// first created the file. A row with no file is reported as
// `missing` and a file with no grammar as `invalid`, and neither is
// deleted or repaired here — ID-45: startup and reads record an
// anomaly, they do not quietly destroy it.
//
// Doing this on read rather than at startup is deliberate. It is two
// directory listings and a handful of upserts on a screen only one
// signed-in admin ever loads, and it means the index can never be
// the thing that is stale.
func (d Deps) markdownTemplateViews(ctx context.Context) ([]MarkdownTemplateView, error) {
	files, err := d.Content.ListMarkdownTemplateFiles()
	if err != nil {
		return nil, err
	}
	records, err := d.DB.ListMarkdownTemplates(ctx)
	if err != nil {
		return nil, err
	}

	rows := make(map[string]store.MarkdownTemplateRecord, len(records))
	for _, record := range records {
		rows[record.Filename] = record
	}

	views := make([]MarkdownTemplateView, 0, len(files))
	seen := map[string]bool{}
	now := store.Now()

	for _, file := range files {
		record, known := rows[file.Filename]

		// Only a loadable file earns a row. An `invalid` name is
		// not durable state, it is something the admin has to see
		// and delete.
		if file.Status == content.AssetStatusOK {
			if !known {
				// A file the database has never seen — dropped in
				// by hand, or created before the index existed. It
				// starts its life here, dated now.
				record = store.MarkdownTemplateRecord{
					Filename:  file.Filename,
					CreatedAt: now, UpdatedAt: now,
				}
			}
			// The tree wins on every field: this index is
			// derived from it (ID-41).
			record.Enabled = file.Enabled
			record.SizeBytes = file.Size
			record.Checksum = file.Checksum
			record.UpdatedAt = file.Modified
			if err := d.DB.UpsertMarkdownTemplate(ctx, record); err != nil {
				return nil, err
			}
		}

		createdAt, updatedAt := record.CreatedAt, record.UpdatedAt
		if createdAt == "" {
			createdAt, updatedAt = file.Modified, file.Modified
		}
		views = append(views, MarkdownTemplateView{
			ID:        file.Filename,
			Filename:  file.Filename,
			Enabled:   file.Enabled,
			Order:     content.AssetOrder(file.Filename),
			Size:      file.Size,
			Checksum:  file.Checksum,
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
			Status:    file.Status,
			Problem:   file.Problem,
		})
		seen[file.Filename] = true
	}

	// Metadata with no file. Reported, never deleted: the row is
	// the only evidence that something was here, and ID-46 asks the
	// admin to decide.
	for _, record := range records {
		if seen[record.Filename] {
			continue
		}
		views = append(views, MarkdownTemplateView{
			ID:        record.Filename,
			Filename:  record.Filename,
			Enabled:   record.Enabled,
			Order:     content.AssetOrder(record.Filename),
			Size:      record.SizeBytes,
			Checksum:  record.Checksum,
			CreatedAt: record.CreatedAt,
			UpdatedAt: record.UpdatedAt,
			Status:    content.AssetStatusMissing,
			Problem:   "the file is gone",
		})
	}
	return views, nil
}

// markdownTemplatePathParam validates the id from the URL.
//
// Go's ServeMux cleans a request path before matching, so a
// traversal attempt arrives here already rewritten or as a 404 —
// but the grammar check is the real defence and it is cheap, and it
// means this handler never sees a name it has not proved harmless.
func markdownTemplatePathParam(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if err := content.ValidateMarkdownTemplateName(id); err != nil {
		return "", markdownTemplateError(err)
	}
	return id, nil
}

func (d Deps) listMarkdownTemplates(w http.ResponseWriter, r *http.Request) {
	views, err := d.markdownTemplateViews(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, markdownTemplateListResponse{
		Templates: views,
		MaxBytes:  content.BodyMaxBytes,
	})
}

func (d Deps) getMarkdownTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := markdownTemplatePathParam(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	views, err := d.markdownTemplateViews(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	for _, view := range views {
		if view.ID != id {
			continue
		}
		body, err := d.Content.ReadMarkdownTemplate(id)
		if err != nil {
			httpx.WriteError(w, r, markdownTemplateError(err))
			return
		}
		httpx.WriteJSON(w, http.StatusOK, markdownTemplateContentResponse{
			MarkdownTemplateView: view,
			Content:              string(body),
		})
		return
	}
	httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
		"no such markdown template"))
}

func (d Deps) createMarkdownTemplate(w http.ResponseWriter, r *http.Request) {
	var req markdownTemplateRequest
	if err := httpx.DecodeJSON(w, r, markdownTemplateBodyLimit(d.Cfg.MaxJSONBody), &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if req.Filename == nil {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
			"filename": "is required",
		}))
		return
	}
	if req.Enabled != nil && !*req.Enabled {
		// Creating a file already switched off is almost certainly
		// a mistake, and the honest reading of "create this,
		// disabled" is two requests.
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
			"enabled": "a new template is created enabled",
		}))
		return
	}
	body := ""
	if req.Content != nil {
		body = *req.Content
	}
	if err := d.Content.CreateMarkdownTemplate(*req.Filename, body); err != nil {
		httpx.WriteError(w, r, markdownTemplateError(err))
		return
	}

	// §22: the audit log records what happened and to what, never
	// the text.
	d.Audit(r.Context(), "markdown_template.created", markdownTemplateRef(*req.Filename))

	views, err := d.markdownTemplateViews(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, markdownTemplateContentResponse{
		MarkdownTemplateView: findMarkdownTemplateView(views, *req.Filename),
		Content:              body,
	})
}

func (d Deps) updateMarkdownTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := markdownTemplatePathParam(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var req markdownTemplateRequest
	if err := httpx.DecodeJSON(w, r, markdownTemplateBodyLimit(d.Cfg.MaxJSONBody), &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if req.Filename == nil && req.Content == nil && req.Enabled == nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"nothing to update: send filename, content or enabled"))
		return
	}
	if _, found, err := d.Content.MarkdownTemplateLocation(id); err != nil {
		httpx.WriteError(w, r, markdownTemplateError(err))
		return
	} else if !found {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
			"no such markdown template"))
		return
	}

	// Rename first: the body is then written to the new name, so
	// there is never a moment where the old name holds the new
	// text.
	final := id
	if req.Filename != nil && *req.Filename != id {
		if err := d.Content.RenameMarkdownTemplate(id, *req.Filename); err != nil {
			httpx.WriteError(w, r, markdownTemplateError(err))
			return
		}
		final = *req.Filename
		if err := d.DB.RenameMarkdownTemplate(r.Context(), id, final); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, r, err)
			return
		}
	}
	if req.Content != nil {
		if err := d.Content.WriteMarkdownTemplate(final, *req.Content); err != nil {
			httpx.WriteError(w, r, markdownTemplateError(err))
			return
		}
	}
	if req.Enabled != nil {
		if err := d.Content.SetMarkdownTemplateEnabled(final, *req.Enabled); err != nil {
			httpx.WriteError(w, r, markdownTemplateError(err))
			return
		}
	}

	// One event per decision. A rename and a content edit are both
	// "updated" and are audited as one event each; an enable is its
	// own decision and gets its own, because "who turned this on,
	// and when" is the question the audit log exists to answer.
	ref := markdownTemplateRef(final)
	d.Audit(r.Context(), "markdown_template.updated", ref)
	if req.Enabled != nil {
		if *req.Enabled {
			d.Audit(r.Context(), "markdown_template.enabled", ref)
		} else {
			d.Audit(r.Context(), "markdown_template.disabled", ref)
		}
	}

	views, err := d.markdownTemplateViews(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	body := ""
	if data, err := d.Content.ReadMarkdownTemplate(final); err == nil {
		body = string(data)
	}
	httpx.WriteJSON(w, http.StatusOK, markdownTemplateContentResponse{
		MarkdownTemplateView: findMarkdownTemplateView(views, final),
		Content:              body,
	})
}

func (d Deps) deleteMarkdownTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := markdownTemplatePathParam(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ref := markdownTemplateRef(id)

	// ID-43: a delete needs the file gone, not just the row. The
	// file is removed first: if it survives and the row does not,
	// the aggregator keeps serving CSS the admin believes they
	// removed.
	_, found, err := d.Content.MarkdownTemplateLocation(id)
	if err != nil {
		httpx.WriteError(w, r, markdownTemplateError(err))
		return
	}
	if found {
		if err := d.Content.DeleteMarkdownTemplate(id); err != nil {
			httpx.WriteError(w, r, markdownTemplateError(err))
			return
		}
	} else {
		// ID-44: a row whose file has already gone is the *repair*
		// case. Answering 404 here would leave the admin with a
		// row they cannot clear — the one thing the screen promises
		// them. Dropping the row cannot touch a file, because there
		// is none. With neither a file nor a row there is nothing to
		// delete, and that is a 404.
		if _, err := d.DB.GetMarkdownTemplate(r.Context(), id); errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
				"no such markdown template"))
			return
		} else if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	if err := d.DB.DeleteMarkdownTemplate(r.Context(), id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.Audit(r.Context(), "markdown_template.deleted", ref)
	w.WriteHeader(http.StatusNoContent)
}

// markdownTemplateRef is the audit reference: a path relative to
// CONTENT_ROOT, which is the one path form that appears in a log
// for content files everywhere in this service. No absolute path,
// and no file body.
func markdownTemplateRef(filename string) string {
	return "system/markdown/" + filename
}

func findMarkdownTemplateView(views []MarkdownTemplateView, id string) MarkdownTemplateView {
	for _, view := range views {
		if view.ID == id {
			return view
		}
	}
	return MarkdownTemplateView{ID: id, Filename: id, Status: content.AssetStatusMissing}
}
