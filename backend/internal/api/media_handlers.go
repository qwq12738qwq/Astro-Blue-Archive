package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blogcms/internal/httpx"
	"blogcms/internal/media"
)

// mediaRow is a row of the `media` table plus the DERIVED facts the library screen
// needs.
//
// The JSON tags are the API contract the Astro admin UI reads. `usage*` and
// `missing` are computed, never stored: `usage` comes from the derived index and
// `missing` from a filesystem check, because the filesystem is the source of truth
// (ARCHITECTURE.md §5).
type mediaRow struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
	// URL is the public, delivery-layer URL. It is built by the media package so
	// no caller assembles "/media/" + path itself (ARCHITECTURE.md §12).
	URL            string       `json:"url"`
	MIME           string       `json:"mime"`
	Size           int64        `json:"size"`
	Width          *int         `json:"width,omitempty"`
	Height         *int         `json:"height,omitempty"`
	Alt            string       `json:"alt"`
	Created        string       `json:"createdAt"`
	Usage          int          `json:"usageCount"`
	Missing        bool         `json:"missing"`
	UsedAs         string       `json:"usedAs,omitempty"`
	UsedBy         []mediaUsage `json:"usedBy"`
	ReferencedFrom []string     `json:"referenceUrls"`
}

// mediaUsage is one reference from one content file.
type mediaUsage struct {
	Kind  string `json:"kind"`
	Slug  string `json:"slug"`
	Href  string `json:"href"`
	Count int    `json:"referenceCount"`
}

func registerMediaRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/admin/media", d.requireSession(d.listMedia))
	mux.HandleFunc("POST /api/v1/admin/media", d.requireSession(d.uploadMedia))
	mux.HandleFunc("PATCH /api/v1/admin/media/{id}", d.requireSession(d.patchMedia))
	mux.HandleFunc("DELETE /api/v1/admin/media/{id}", d.requireSession(d.deleteMedia))

	// ARCHITECTURE.md §51: rebuilding the index is a first-class operation, not a
	// side effect of an upload, because the index is derived and must be
	// recoverable. It runs in-process through the API rather than as a shell command
	// over HTTP (ARCHITECTURE.md §4).
	mux.HandleFunc("POST /api/v1/admin/media/rebuild-usage", d.requireSession(d.rebuildMediaUsage))
	mux.HandleFunc("GET /api/v1/admin/media/cache-stats", d.requireSession(d.mediaCacheStats))
	mux.HandleFunc("POST /api/v1/admin/media/clear-cache", d.requireSession(d.clearMediaCache))
}

// mediaQuery is the parsed filter set of the library screen.
type mediaQuery struct {
	search  string
	mime    string
	unused  bool
	missing bool
	limit   int
	offset  int
}

func parseMediaQuery(r *http.Request) (mediaQuery, error) {
	q := r.URL.Query()
	out := mediaQuery{
		search:  strings.TrimSpace(q.Get("q")),
		mime:    strings.TrimSpace(q.Get("type")),
		limit:   200,
		unused:  q.Get("unused") == "1",
		missing: q.Get("missing") == "1",
	}
	if out.search != "" && len([]rune(out.search)) > 100 {
		return out, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"q must be at most 100 characters")
	}
	if out.mime != "" {
		switch out.mime {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
		default:
			return out, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
				"type must be a supported image MIME type")
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			out.limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			out.offset = n
		}
	}
	return out, nil
}

func (d Deps) listMedia(w http.ResponseWriter, r *http.Request) {
	q, err := parseMediaQuery(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// ARCHITECTURE.md §50: the usage index is derived, so it is refreshed when the
	// files it was derived from have moved on. Without this the screen would say an
	// image is unused moments after a post started using it — and the delete guard in
	// §54 would then agree with the screen and remove a file that is in use.
	//
	// It is refreshed here rather than on a timer because "the index is stale" is not
	// a thing that can be noticed, only a thing that can be compared.
	if stale, err := d.usageIndexIsStale(r.Context()); err != nil {
		httpx.WriteError(w, r, err)
		return
	} else if stale {
		if _, _, err := RebuildUsageIndex(r.Context(), &d); err != nil {
			// A failed refresh must not fail the screen. It costs accuracy on the
			// usage column for one request, which is better than an error page on the
			// library an admin is trying to look at.
			slog.Warn("media usage index refresh failed", "error", err)
		}
	}

	// ARCHITECTURE.md §21: SQLite has one connection, so no caller may hold two open
	// result sets at once. Everything this handler needs from the database is read
	// *before* the list query opens its rows — an `iconMediaID` lookup in the middle
	// of the loop would wait forever for a connection the rows still hold, and the
	// request would hang rather than fail.
	iconID, err := d.iconMediaID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Built as one statement with a fixed shape: every filter is a parameter, so no
	// request value is ever concatenated into SQL (ARCHITECTURE.md §7).
	rows, err := d.DB.QueryContext(r.Context(), `
		SELECT m.id, m.filename, m.path, m.mime, m.size, m.width, m.height, m.alt, m.created_at,
		       coalesce((SELECT sum(reference_count) FROM media_usage u WHERE u.media_id = m.id), 0) AS usage
		  FROM media m
		 WHERE (? = '' OR m.filename LIKE ? OR m.path LIKE ?)
		   AND (? = '' OR m.mime = ?)
		 ORDER BY m.created_at DESC, m.id DESC
		 LIMIT ? OFFSET ?`,
		q.search, likeParam(q.search), likeParam(q.search),
		q.mime, q.mime,
		q.limit, q.offset)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	defer func() { _ = rows.Close() }()

	items := make([]mediaRow, 0, 32)
	kept := make([]mediaRow, 0, 32)
	for rows.Next() {
		var it mediaRow
		var created string
		if err := rows.Scan(&it.ID, &it.Filename, &it.Path, &it.MIME, &it.Size,
			&it.Width, &it.Height, &it.Alt, &created, &it.Usage); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		it.Created = created
		it.URL = media.PublicURL(it.Path)
		// The filesystem is the truth: a row whose file is gone is reported, not
		// hidden and not fatal (ARCHITECTURE.md §5). This is a stat, not a query, so
		// it is safe to run while the rows are open.
		it.Missing = !d.Media.Exists(it.Path)
		it.UsedBy = []mediaUsage{}
		it.ReferencedFrom = []string{}
		if it.ID == iconID {
			it.UsedAs = "siteIcon"
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Released before anything else touches the database.
	_ = rows.Close()

	// `unused` and `missing` are filesystem-derived, so they are applied after the
	// query rather than as SQL. Doing it here keeps SQL free of a filesystem
	// predicate, which no index could serve anyway.
	for _, it := range items {
		if q.unused && it.Usage > 0 {
			continue
		}
		if q.missing && !it.Missing {
			continue
		}
		kept = append(kept, it)
	}

	// The reference detail is a second, per-item lookup. It is deliberately not
	// joined into the list query: a library of 500 items would return 500 joined rows
	// to show detail for the handful the screen actually renders.
	if len(kept) > 0 && !q.unused && !q.missing {
		if err := d.attachUsage(r.Context(), kept); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": kept,
		"cache": d.Images.Stats(),
	})
}

// likeParam escapes the LIKE wildcards in a user-supplied search term, so a search
// for `100%` matches a literal `100%` instead of matching everything.
func likeParam(term string) string {
	if term == "" {
		return ""
	}
	return "%" + likeEscape(term) + "%"
}

// likeEscape escapes the characters LIKE treats as wildcards, without adding the
// `%`/`…` wrapper. Callers that build their own LIKE clause use this directly.
func likeEscape(term string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(term)
}

// attachUsage fills in `usedBy` for a page of results.
//
// One query for the whole page, keyed by media id, because the alternative is a
// query per thumbnail and ARCHITECTURE.md §21 gives SQLite a single connection.
func (d Deps) attachUsage(ctx context.Context, items []mediaRow) error {
	ids := make([]string, 0, len(items))
	byID := make(map[string]int, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
		byID[items[i].ID] = i
	}

	query := `SELECT media_id, content_type, content_slug, reference_count
	            FROM media_usage
	           WHERE media_id IN (` + placeholders(len(ids)) + `)
	           ORDER BY content_type, content_slug`
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := d.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id, kind, slug string
		var count int
		if err := rows.Scan(&id, &kind, &slug, &count); err != nil {
			return err
		}
		idx, ok := byID[id]
		if !ok {
			continue
		}
		items[idx].UsedBy = append(items[idx].UsedBy, mediaUsage{
			Kind:  kind,
			Slug:  slug,
			Href:  contentHref(kind, slug),
			Count: count,
		})
		items[idx].ReferencedFrom = append(items[idx].ReferencedFrom, contentHref(kind, slug))
	}
	return rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// contentHref is the admin URL for a piece of content, so the media screen can link
// to the editor that owns a reference.
func contentHref(kind, slug string) string {
	if kind == media.KindPage {
		return "/admin/pages/" + slug
	}
	return "/admin/posts/" + slug
}

// uploadMedia accepts a single multipart file.
//
// ARCHITECTURE.md §19: the client's filename is used only as a display label.
// The stored path is generated from the detected type.
func (d Deps) uploadMedia(w http.ResponseWriter, r *http.Request) {
	maxBytes := d.Cfg.MaxUploadBytes

	// Cap the whole request body: the multipart envelope plus the file.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024)

	if err := r.ParseMultipartForm(maxBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, httpx.CodeTooLarge,
				"upload exceeds %d bytes", maxBytes))
			return
		}
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"expected a multipart upload"))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"a file field named 'file' is required"))
		return
	}
	defer func() { _ = file.Close() }()

	// alt may accompany the upload, which is what the media picker does when an admin
	// types a description while the image is in front of them.
	alt := strings.TrimSpace(r.FormValue("alt"))
	if n := len([]rune(alt)); n > 200 {
		alt = string([]rune(alt)[:200])
	}

	// Save sniffs the magic bytes from the stream itself, so there is no prefix
	// bookkeeping here and no way to store a truncated file.
	result, err := d.Media.Save(header.Header.Get("Content-Type"), header.Filename, nil, file,
		maxBytes, d.Cfg.ImageMaxPixels)
	if err != nil {
		d.writeMediaError(w, r, err)
		return
	}

	if _, err := d.DB.ExecContext(r.Context(),
		`INSERT INTO media (id, filename, path, mime, size, width, height, alt, sha256, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		result.ID, result.Filename, result.Path, result.MIME, result.Size,
		nullInt(result.Width), nullInt(result.Height), alt, result.SHA256,
		result.CreatedAt.Format(time.RFC3339),
	); err != nil {
		// Do not leave an orphaned file behind if the row cannot be written.
		_ = d.Media.Delete(result.Path)
		httpx.WriteError(w, r, err)
		return
	}

	d.Audit(r.Context(), "media.created", result.ID)

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":        result.ID,
		"filename":  result.Filename,
		"path":      result.Path,
		"url":       media.PublicURL(result.Path),
		"mime":      result.MIME,
		"size":      result.Size,
		"width":     result.Width,
		"height":    result.Height,
		"alt":       alt,
		"createdAt": result.CreatedAt.Format(time.RFC3339),
	})
}

// patchMedia updates the editable metadata of one asset.
//
// Only alt text today. There is deliberately no field for a path: the stored path is
// generated by the server and a rename would break every Markdown file that
// references it (ARCHITECTURE.md §81).
func (d Deps) patchMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !media.ValidID(id) {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"malformed media id"))
		return
	}

	var body struct {
		Alt *string `json:"alt"`
	}
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if body.Alt == nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"nothing to update"))
		return
	}
	alt := strings.TrimSpace(*body.Alt)
	if n := len([]rune(alt)); n > 200 {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
			"alt": "must be at most 200 characters",
		}))
		return
	}

	res, err := d.DB.ExecContext(r.Context(),
		`UPDATE media SET alt = ? WHERE id = ?`, alt, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound, "no such media"))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "alt": alt})
}

func (d Deps) deleteMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var rel string
	err := d.DB.QueryRowContext(r.Context(), `SELECT path FROM media WHERE id = ?`, id).Scan(&rel)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound, "no such media"))
		return
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	force := r.URL.Query().Get("force") == "1"

	// ARCHITECTURE.md §54: a referenced asset is refused by default. Deleting it
	// leaves a broken image inside a Markdown file, which is content the admin
	// authored and this system does not own.
	usage, usedBy, err := d.usageOf(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if usage > 0 && !force {
		httpx.WriteError(w, r, &httpx.APIError{
			Status: http.StatusConflict,
			Code:   httpx.CodeConflict,
			Message: "this image is used by " + strconv.Itoa(usage) + " reference(s) in " +
				strconv.Itoa(len(usedBy)) + " file(s)",
			Fields: map[string]string{
				"usage": usedByString(usedBy),
			},
		})
		return
	}

	// ARCHITECTURE.md §117: the site icon is a reference too, and one that is not
	// visible in the usage index because it lives in settings rather than in
	// content.
	iconID, err := d.iconMediaID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if iconID == id && !force {
		httpx.WriteError(w, r, &httpx.APIError{
			Status:  http.StatusConflict,
			Code:    httpx.CodeConflict,
			Message: "this image is the site icon",
		})
		return
	}

	if err := d.Media.Delete(rel); err != nil && !errors.Is(err, media.ErrNotFound) {
		httpx.WriteError(w, r, err)
		return
	}
	if _, err := d.DB.ExecContext(r.Context(),
		`DELETE FROM media_usage WHERE media_id = ?`, id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if _, err := d.DB.ExecContext(r.Context(), `DELETE FROM media WHERE id = ?`, id); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// The derived representation of this file is deliberately left in place.
	//
	// An earlier version cleared the whole cache here, on the reasoning that the disk
	// should be reclaimed. It does reclaim disk, and it costs every *other* image on
	// the site its cached WebP: the next visitor to request any asset pays a full
	// re-encode. One unused image deleted, whole cache gone.
	//
	// It is also unnecessary. The cache is keyed by checksum (§20), so the entry cannot
	// be reached by path — and the delivery layer looks the row up before it touches
	// the cache at all, so a deleted asset is a 404 whether or not an entry survives.
	// The entry is unreachable garbage until an admin clears the cache themselves,
	// which is the control that actually exists for that job (§83).
	//
	// Making this targeted instead would mean keeping a reverse index from checksum to
	// key, which is persistent state that looks authoritative and is not — §27 is
	// explicit that a derived table must stay losable.
	d.Audit(r.Context(), "media.deleted", id)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// usageOf returns the total reference count and the files that reference it.
func (d Deps) usageOf(ctx context.Context, id string) (int, []mediaUsage, error) {
	var total int
	err := d.DB.QueryRowContext(ctx,
		`SELECT coalesce(sum(reference_count), 0) FROM media_usage WHERE media_id = ?`, id).Scan(&total)
	if err != nil {
		return 0, nil, err
	}
	rows, err := d.DB.QueryContext(ctx,
		`SELECT content_type, content_slug, reference_count FROM media_usage
		  WHERE media_id = ? ORDER BY content_type, content_slug`, id)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]mediaUsage, 0, 4)
	for rows.Next() {
		var u mediaUsage
		if err := rows.Scan(&u.Kind, &u.Slug, &u.Count); err != nil {
			return 0, nil, err
		}
		u.Href = contentHref(u.Kind, u.Slug)
		out = append(out, u)
	}
	return total, out, rows.Err()
}

func usedByString(used []mediaUsage) string {
	if len(used) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(used))
	for _, u := range used {
		parts = append(parts, u.Href)
	}
	return strings.Join(parts, ", ")
}

// rebuildMediaUsage rescans content/ and replaces the derived index.
//
// ARCHITECTURE.md §51: the index must be recoverable. Everything here is derived
// from files, so the table is dropped and refilled rather than diffed — a diff would
// need to know what changed, and the answer to that is "read the files again".
func (d Deps) rebuildMediaUsage(w http.ResponseWriter, r *http.Request) {
	stored, unmatched, err := RebuildUsageIndex(r.Context(), &d)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	d.Audit(r.Context(), "media.usage_rebuilt", "media")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"references": stored,
		// A reference with no media row is a Markdown file pointing at something that
		// does not exist. It is reported rather than hidden: it is a broken image on
		// a live page, and the admin is the only one who can fix it.
		"unmatched": unmatched,
	})
}

func chunkStrings(in []string, size int) [][]string {
	if len(in) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(in)+size-1)/size)
	for i := 0; i < len(in); i += size {
		end := i + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[i:end])
	}
	return out
}

func (d Deps) mediaCacheStats(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, d.Images.Stats())
}

// clearMediaCache drops every derived representation.
//
// ARCHITECTURE.md §21/§83: the next request for a WebP regenerates it from the
// original. Nothing under MEDIA_ROOT is touched.
func (d Deps) clearMediaCache(w http.ResponseWriter, r *http.Request) {
	if err := d.Images.ClearCache(); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "cache": d.Images.Stats()})
}

// iconMediaID reads the media id the settings point at as the site icon.
func (d Deps) iconMediaID(ctx context.Context) (string, error) {
	settings, err := d.loadSettings(ctx)
	if err != nil {
		return "", err
	}
	return settings.SiteIconMediaID, nil
}

// iconRef resolves the site icon to the two things a caller needs: its public URL and
// a cache-busting token.
//
// ARCHITECTURE.md §76: browsers cache a favicon far more aggressively than they cache
// a page, so replacing the file while keeping the URL means a visitor keeps the old
// icon with no error anywhere. The token comes from the asset's own checksum, so it
// changes exactly when the icon does — and the storage path is never rewritten.
//
// One query, not two. ARCHITECTURE.md §21 gives SQLite a single connection, and two
// sequential round trips for one row is one too many on a page that renders this on
// every request.
func (d Deps) iconRef(ctx context.Context, mediaID string) (url, version string) {
	if mediaID == "" {
		return "", ""
	}
	var rel, created string
	var sum []byte
	err := d.DB.QueryRowContext(ctx,
		`SELECT path, sha256, created_at FROM media WHERE id = ?`, mediaID).Scan(&rel, &sum, &created)
	if err != nil {
		return "", ""
	}
	// The storage layout never leaves the backend: the caller receives a URL, not a
	// path. created_at is folded in so a re-upload of identical bytes still busts a
	// stale cache entry.
	return media.PublicURL(rel), cacheToken(hex.EncodeToString(sum) + "|" + created)
}

// cacheToken is a short, stable digest used only for cache busting.
//
// It is a real digest rather than a hand-rolled mix because "a stable, non-security
// hash" is exactly the kind of thing that is stable on one machine and not another,
// and a version token that changed per process would defeat its own purpose.
func cacheToken(v string) string {
	sum := sha256.Sum256([]byte("media-cache-token\x00" + v))
	return hex.EncodeToString(sum[:8])
}

func (d Deps) writeMediaError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, media.ErrTooLarge):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, httpx.CodeTooLarge,
			"%s", err.Error()))
	case errors.Is(err, media.ErrTooManyPixels):
		// 422, not 413: the request is not too big in bytes, the image is too big in
		// pixels, and a client that retries the same file will fail identically.
		httpx.WriteError(w, r, &httpx.APIError{
			Status:  http.StatusUnprocessableEntity,
			Code:    httpx.CodeValidation,
			Message: err.Error(),
			Fields:  map[string]string{"file": err.Error()},
		})
	case errors.Is(err, media.ErrUnsupportedType):
		httpx.WriteError(w, r, &httpx.APIError{
			Status:  http.StatusUnprocessableEntity,
			Code:    httpx.CodeValidation,
			Message: err.Error(),
			Fields:  map[string]string{"file": err.Error()},
		})
	default:
		httpx.WriteError(w, r, err)
	}
}

func nullInt(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}
