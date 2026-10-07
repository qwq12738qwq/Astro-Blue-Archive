package api

// Custom CSS/JS asset management.
//
// ARCHITECTURE.md ID-33: `content/system/css/NNN-name.css` and
// `content/system/js/NNN-name.js` are managed assets, one file each. The legacy pair
// `custom.css` / `custom.js` is untouched by all of this and is still editable at
// /admin/custom-code; it appears in the list as a read-only row so the screen shows
// the whole picture in one place.
//
// The division of labour is the same as everywhere else in this service:
//
//   - Go writes, validates, orders, audits and holds the metadata.
//   - The database stores metadata only. There is no body column, and losing the
//     table costs one rescan of two directories.
//   - Astro aggregates the directories at request time to answer /custom.css and
//     /custom.js, so nothing here has to be "published" and no build is involved.
//
// Every route goes through `requireSession`, which is also where the Origin check and
// the CSRF header live: writing CSS is a write, and a 512 KB stylesheet is still a
// write.
import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"blogcms/internal/content"
	"blogcms/internal/httpx"
	"blogcms/internal/store"
)

// customAssetBodyLimit is the JSON body ceiling for these routes.
//
// The general MAX_JSON_BODY default is 1 MiB, and a custom asset is capped at
// BodyMaxBytes = 512 KiB. Those two only fit together because JSON escapes expand:
// every newline in the file becomes two bytes, so a 512 KiB file of newlines is over
// 1 MiB on the wire and the editor would refuse to save a file it is entitled to
// save. Rather than loosen the limit for every endpoint in the service, these routes
// ask for enough room for the largest legal body plus its worst-case escaping.
func customAssetBodyLimit(cfgMax int64) int64 {
	worst := int64(content.BodyMaxBytes) * 6 // \u00XX is 6 bytes for one input byte
	if cfgMax >= worst {
		return cfgMax
	}
	return worst
}

// CustomAssetView is the API shape of one asset.
//
// There is no path field, absolute or relative: §105 — a public or admin response
// must not tell a caller where CONTENT_ROOT is or how it is laid out. `id` is the
// filename, which is the identity the filesystem already uses.
type CustomAssetView struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	Type      string `json:"type"`
	Enabled   bool   `json:"enabled"`
	Legacy    bool   `json:"legacy"`
	Order     int    `json:"order"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	// Status is `ok`, `missing` or `invalid`. Anything other than `ok` is not
	// served, and says why in Problem.
	Status  string `json:"status"`
	Problem string `json:"problem,omitempty"`
}

// customAssetListResponse carries the list plus the limits an editor needs to
// validate before the server does.
type customAssetListResponse struct {
	Assets   []CustomAssetView `json:"assets"`
	MaxBytes int               `json:"maxBytes"`
}

// customAssetContentResponse is one asset plus its body.
type customAssetContentResponse struct {
	CustomAssetView
	Content string `json:"content"`
}

// customAssetRequest creates or updates one asset.
//
// Every field is a pointer. §27's rule: an omitted field means "leave it alone", so
// saving a renamed file must not blank its body, and toggling `enabled` must not
// require resending 512 KB of text the server already has. An explicit `false` is a
// value and is applied.
type customAssetRequest struct {
	Filename *string `json:"filename"`
	Content  *string `json:"content"`
	Enabled  *bool   `json:"enabled"`
}

// legacyAssetID is the only id the legacy files answer to. It is refused by every
// managed write path: the legacy pair is edited at /admin/custom-code and must not
// be deletable through the asset manager (ID-34).
const legacyAssetID = "legacy"

func registerCustomAssetRoutes(mux *http.ServeMux, d Deps) {
	for _, kind := range []content.AssetKind{content.AssetCSS, content.AssetJS} {
		base := "/api/v1/admin/custom/" + string(kind)
		mux.HandleFunc("GET "+base, d.requireSession(d.listCustomAssets(kind)))
		mux.HandleFunc("POST "+base, d.requireSession(d.createCustomAsset(kind)))
		mux.HandleFunc("GET "+base+"/{id}", d.requireSession(d.getCustomAsset(kind)))
		mux.HandleFunc("PUT "+base+"/{id}", d.requireSession(d.updateCustomAsset(kind)))
		mux.HandleFunc("DELETE "+base+"/{id}", d.requireSession(d.deleteCustomAsset(kind)))
	}
}

// customAssetError maps content-package errors onto the API error envelope.
func customAssetError(err error) error {
	var ve *content.ValidationError
	if errors.As(err, &ve) {
		return httpx.ValidationError(ve.Fields)
	}
	switch {
	case errors.Is(err, content.ErrInvalidAssetName):
		return httpx.Errorf(http.StatusUnprocessableEntity, httpx.CodeValidation, "%s", err.Error())
	case errors.Is(err, content.ErrAssetNotFound):
		return httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound, "no such custom asset")
	case errors.Is(err, content.ErrConflict):
		return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "%s", err.Error())
	default:
		return err
	}
}

// customAssetViews merges the filesystem and the metadata index into the list the
// admin sees, reconciling the index on the way through.
//
// The filesystem is read first and is authoritative; the index only adds the one
// thing the tree cannot say, which is when an admin first created the file. A row
// with no file is reported as `missing` and a file with no grammar as `invalid`, and
// neither is deleted or repaired here — §92: startup and reads record an anomaly,
// they do not quietly destroy it.
//
// Doing this on read rather than at startup is deliberate. It is two directory
// listings and a handful of upserts on a screen only one signed-in admin ever loads,
// and it means the index can never be the thing that is stale.
func (d Deps) customAssetViews(ctx context.Context, kind content.AssetKind) ([]CustomAssetView, error) {
	files, err := d.Content.ListCustomAssetFiles(kind)
	if err != nil {
		return nil, err
	}
	records, err := d.DB.ListCustomAssets(ctx, string(kind))
	if err != nil {
		return nil, err
	}

	rows := make(map[string]store.CustomAssetRecord, len(records))
	for _, record := range records {
		rows[record.Filename] = record
	}

	views := make([]CustomAssetView, 0, len(files)+1)
	// The legacy row is best-effort and says so. A missing custom.css is not an
	// error — it is an install that has never saved any — and the filesystem walk
	// above already reports a genuine read failure as `missing`, so this cannot
	// invent a wrong row either way. What it must not do is fail silently.
	if legacy, err := d.legacyAssetView(kind); err != nil {
		slog.Error("read legacy custom-code file", "type", string(kind), "error", err)
	} else {
		views = append(views, legacy)
	}

	seen := map[string]bool{}
	now := store.Now()

	for _, file := range files {
		record, known := rows[file.Filename]

		// Only a loadable file earns a row. An `invalid` name is not durable state,
		// it is something the admin has to see and delete.
		if file.Status == content.AssetStatusOK {
			if !known {
				// A file the database has never seen — dropped in by hand, or created
				// before the index existed. It starts its life here, dated now.
				record = store.CustomAssetRecord{
					Type: string(kind), Filename: file.Filename,
					CreatedAt: now, UpdatedAt: now,
				}
			}
			// The tree wins on every field: this index is derived from it (§90).
			record.Enabled = file.Enabled
			record.SizeBytes = file.Size
			record.Checksum = file.Checksum
			record.UpdatedAt = file.Modified
			if err := d.DB.UpsertCustomAsset(ctx, record); err != nil {
				return nil, err
			}
		}

		createdAt, updatedAt := record.CreatedAt, record.UpdatedAt
		if createdAt == "" {
			createdAt, updatedAt = file.Modified, file.Modified
		}
		views = append(views, CustomAssetView{
			ID:        file.Filename,
			Filename:  file.Filename,
			Type:      string(kind),
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

	// Metadata with no file. Reported, never deleted: the row is the only evidence
	// that something was here, and §57 asks the admin to decide.
	for _, record := range records {
		if seen[record.Filename] {
			continue
		}
		views = append(views, CustomAssetView{
			ID:        record.Filename,
			Filename:  record.Filename,
			Type:      record.Type,
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

// legacyAssetView describes custom.css or custom.js for the same list.
//
// Order -1 is the whole point: the legacy file loads first, ahead of every managed
// asset, so a stylesheet that predates the manager keeps winning the cascade it has
// always won (ID-34).
func (d Deps) legacyAssetView(kind content.AssetKind) (CustomAssetView, error) {
	name := "custom." + string(kind)
	body, err := d.Content.ReadSystemFile(name)
	if err != nil {
		return CustomAssetView{}, err
	}
	size, modified, exists, err := d.Content.StatSystemFile(name)
	if err != nil {
		return CustomAssetView{}, err
	}
	if !exists {
		modified = ""
	}
	return CustomAssetView{
		ID:        legacyAssetID,
		Filename:  name,
		Type:      string(kind),
		Enabled:   true,
		Legacy:    true,
		Order:     -1,
		Size:      size,
		Checksum:  content.Checksum([]byte(body)),
		CreatedAt: modified,
		UpdatedAt: modified,
		Status:    content.AssetStatusOK,
	}, nil
}

// customAssetPathParam validates the id from the URL.
//
// Go's ServeMux cleans a request path before matching, so a traversal attempt
// arrives here already rewritten or as a 404 — but the grammar check is the real
// defence and it is cheap, and it means this handler never sees a name it has not
// proved harmless.
func customAssetPathParam(r *http.Request, kind content.AssetKind) (string, error) {
	id := r.PathValue("id")
	if id == legacyAssetID {
		return "", httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"%s is a legacy file: edit it at /admin/custom-code", "custom."+string(kind))
	}
	if err := content.ValidateAssetFilename(kind, id); err != nil {
		return "", customAssetError(err)
	}
	return id, nil
}

func (d Deps) listCustomAssets(kind content.AssetKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		views, err := d.customAssetViews(r.Context(), kind)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, customAssetListResponse{
			Assets:   views,
			MaxBytes: content.BodyMaxBytes,
		})
	}
}

func (d Deps) getCustomAsset(kind content.AssetKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := customAssetPathParam(r, kind)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		views, err := d.customAssetViews(r.Context(), kind)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		for _, view := range views {
			if view.ID != id || view.Legacy {
				continue
			}
			body, err := d.Content.ReadCustomAsset(kind, id)
			if err != nil {
				httpx.WriteError(w, r, customAssetError(err))
				return
			}
			httpx.WriteJSON(w, http.StatusOK, customAssetContentResponse{
				CustomAssetView: view,
				Content:         string(body),
			})
			return
		}
		httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
			"no such custom asset"))
	}
}

func (d Deps) createCustomAsset(kind content.AssetKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req customAssetRequest
		if err := httpx.DecodeJSON(w, r, customAssetBodyLimit(d.Cfg.MaxJSONBody), &req); err != nil {
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
			// Creating a file already switched off is almost certainly a mistake, and
			// the honest reading of "create this, disabled" is two requests.
			httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
				"enabled": "a new asset is created enabled",
			}))
			return
		}
		body := ""
		if req.Content != nil {
			body = *req.Content
		}
		if err := d.Content.CreateCustomAsset(kind, *req.Filename, body); err != nil {
			httpx.WriteError(w, r, customAssetError(err))
			return
		}

		// §22: the audit log records what happened and to what, never the text.
		d.Audit(r.Context(), "custom_"+string(kind)+".created", customAssetRef(kind, *req.Filename))

		views, err := d.customAssetViews(r.Context(), kind)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		view := findCustomAssetView(views, *req.Filename)
		httpx.WriteJSON(w, http.StatusCreated, customAssetContentResponse{
			CustomAssetView: view,
			Content:         body,
		})
	}
}

func (d Deps) updateCustomAsset(kind content.AssetKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := customAssetPathParam(r, kind)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		var req customAssetRequest
		if err := httpx.DecodeJSON(w, r, customAssetBodyLimit(d.Cfg.MaxJSONBody), &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if req.Filename == nil && req.Content == nil && req.Enabled == nil {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
				"nothing to update: send filename, content or enabled"))
			return
		}
		if _, found, err := d.Content.CustomAssetLocation(kind, id); err != nil {
			httpx.WriteError(w, r, customAssetError(err))
			return
		} else if !found {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
				"no such custom asset"))
			return
		}

		// Rename first: the body is then written to the new name, so there is never a
		// moment where the old name holds the new text.
		final := id
		if req.Filename != nil && *req.Filename != id {
			if err := d.Content.RenameCustomAsset(kind, id, *req.Filename); err != nil {
				httpx.WriteError(w, r, customAssetError(err))
				return
			}
			final = *req.Filename
			if err := d.DB.RenameCustomAsset(r.Context(), string(kind), id, final); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				httpx.WriteError(w, r, err)
				return
			}
		}
		if req.Content != nil {
			if err := d.Content.WriteCustomAsset(kind, final, *req.Content); err != nil {
				httpx.WriteError(w, r, customAssetError(err))
				return
			}
		}
		if req.Enabled != nil {
			if err := d.Content.SetCustomAssetEnabled(kind, final, *req.Enabled); err != nil {
				httpx.WriteError(w, r, customAssetError(err))
				return
			}
		}

		// One event per decision. A rename and a content edit are both "updated" and
		// are audited as one event each; an enable is its own decision and gets its
		// own, because "who turned this on, and when" is the question the audit log
		// exists to answer.
		ref := customAssetRef(kind, final)
		d.Audit(r.Context(), "custom_"+string(kind)+".updated", ref)
		if req.Enabled != nil {
			if *req.Enabled {
				d.Audit(r.Context(), "custom_"+string(kind)+".enabled", ref)
			} else {
				d.Audit(r.Context(), "custom_"+string(kind)+".disabled", ref)
			}
		}

		views, err := d.customAssetViews(r.Context(), kind)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		body := ""
		if data, err := d.Content.ReadCustomAsset(kind, final); err == nil {
			body = string(data)
		}
		httpx.WriteJSON(w, http.StatusOK, customAssetContentResponse{
			CustomAssetView: findCustomAssetView(views, final),
			Content:         body,
		})
	}
}

func (d Deps) deleteCustomAsset(kind content.AssetKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := customAssetPathParam(r, kind)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		ref := customAssetRef(kind, id)

		// §35: a delete needs the file gone, not just the row. The file is removed
		// first: if it survives and the row does not, the aggregator keeps serving
		// code the admin believes they removed.
		_, found, err := d.Content.CustomAssetLocation(kind, id)
		if err != nil {
			httpx.WriteError(w, r, customAssetError(err))
			return
		}
		if found {
			if err := d.Content.DeleteCustomAsset(kind, id); err != nil {
				httpx.WriteError(w, r, customAssetError(err))
				return
			}
		} else {
			// §106: a row whose file has already gone is the *repair* case. Answering
			// 404 here would leave the admin with a row they cannot clear — the one
			// thing the screen promises them. Dropping the row cannot touch a file,
			// because there is none. With neither a file nor a row there is nothing
			// to delete, and that is a 404.
			if _, err := d.DB.GetCustomAsset(r.Context(), string(kind), id); errors.Is(err, store.ErrNotFound) {
				httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
					"no such custom asset"))
				return
			} else if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
		}

		if err := d.DB.DeleteCustomAsset(r.Context(), string(kind), id); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		d.Audit(r.Context(), "custom_"+string(kind)+".deleted", ref)
		w.WriteHeader(http.StatusNoContent)
	}
}

// customAssetRef is the audit reference: a path relative to CONTENT_ROOT, which is
// the one path form that appears in a log for content files everywhere in this
// service (content/posts/<slug>.md). No absolute path, and no file body.
func customAssetRef(kind content.AssetKind, filename string) string {
	return "system/" + kind.Dir() + "/" + filename
}

func findCustomAssetView(views []CustomAssetView, id string) CustomAssetView {
	for _, view := range views {
		if view.ID == id {
			return view
		}
	}
	return CustomAssetView{ID: id, Filename: id, Type: "", Status: content.AssetStatusMissing}
}
