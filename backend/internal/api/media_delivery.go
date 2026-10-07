package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"blogcms/internal/media"
	"blogcms/internal/store"
)

// Image delivery.
//
// ARCHITECTURE.md §10–§32: the browser asks for `/media/<storage path>` and gets
// either a WebP representation or the original bytes, chosen from `Accept`. The URL
// never changes and the stored file is never rewritten, so Markdown keeps pointing
// at `/media/2026/10/abc.jpg` whether or not WebP is in play (ARCHITECTURE.md §11).
//
// ARCHITECTURE.md §14: this handler is the only way an image leaves the system. The
// browser is never told a filesystem path, because the day it is, the storage
// layout has become public API and can no longer change.

// mediaCacheControl is deliberately NOT `immutable`.
//
// ARCHITECTURE.md §30: the URL carries no content hash, because it is the
// Markdown-visible original path and rewriting Markdown on upload would break the
// file-first model. That means an admin *can* replace `photo.jpg` in place, and a
// browser holding an immutable year-long entry would never notice. A week is long
// enough to cover repeat visits and short enough that a replaced image shows up.
const mediaCacheControl = "public, max-age=604800, stale-while-revalidate=86400"

func registerMediaDeliveryRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /media/{path...}", d.deliverMedia)
	mux.HandleFunc("HEAD /media/{path...}", d.deliverMedia)
}

func (d Deps) deliverMedia(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	// The wildcard arrives URL-decoded. A NUL byte or a traversal segment is refused
	// before anything touches the filesystem (ARCHITECTURE.md §20).
	if rel == "" || strings.ContainsRune(rel, 0) || strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") {
		http.NotFound(w, r)
		return
	}

	// Only the vetted image types are ever served as an image. Anything else that
	// somehow reached MEDIA_ROOT is served as an inert download, so it cannot execute
	// in this origin (ARCHITECTURE.md D6).
	if !media.IsImagePath(rel) {
		d.serveInertMedia(w, r, rel)
		return
	}

	rec, err := d.deliveryRecord(r.Context(), rel)
	if err != nil {
		if errors.Is(err, media.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "media unavailable", http.StatusInternalServerError)
		return
	}

	body, contentType, etag, err := d.representation(r, rel, rec)
	if err != nil {
		if errors.Is(err, media.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Warn("image_delivery_failed", "path", rel, "error", err)
		// Fall back to the original rather than failing the image: a cache or encoder
		// problem must not take a page's pictures down (ARCHITECTURE.md §60).
		body, contentType, etag, err = d.originalBytes(rec)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}

	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("Cache-Control", mediaCacheControl)
	header.Set("X-Content-Type-Options", "nosniff")
	// Images render in <img>, so this must be inline rather than attachment.
	header.Set("Content-Disposition", "inline")
	if etag != "" {
		header.Set("ETag", etag)
	}
	// ARCHITECTURE.md §18: the body depends on Accept, so a shared cache that stored
	// the WebP and then served it to a client that cannot read WebP is a correctness
	// bug, not a performance one. This header is mandatory whenever Content-Type is
	// negotiated.
	header.Set("Vary", "Accept")

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	header.Set("Content-Length", strconv.Itoa(len(body)))

	// ARCHITECTURE.md §31: HEAD returns exactly the headers GET would, and no body.
	// The representation is still negotiated, because those headers must describe
	// what a GET would have produced.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Debug("image_client_gone", "path", rel, "error", err)
	}
}

// deliveryRecord loads the indexed metadata for a request and confirms the file is
// still on disk.
//
// ARCHITECTURE.md §5: the filesystem is the source of truth. A row whose file has
// gone yields `missing` and a 404; a file with no row is still served, as the
// original, because the index is an index and not a gate.
func (d Deps) deliveryRecord(ctx context.Context, rel string) (media.MediaRecord, error) {
	rec, err := d.mediaRepo().MediaRecord(ctx, rel)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return media.MediaRecord{}, err
	}
	if errors.Is(err, store.ErrNotFound) {
		rec = media.MediaRecord{Rel: rel}
	}

	if _, statErr := d.Media.Stat(rel); statErr != nil {
		if errors.Is(statErr, media.ErrNotFound) {
			rec.Missing = true
			return rec, nil
		}
		return media.MediaRecord{}, statErr
	}
	if rec.ID == "" {
		rec.Rel = rel
		rec.MIME = MimeFromPath(rel)
	}
	return rec, nil
}

// representation returns the bytes to send for this request.
func (d Deps) representation(r *http.Request, rel string, rec media.MediaRecord) ([]byte, string, string, error) {
	if rec.Missing {
		return nil, "", "", media.ErrNotFound
	}
	// ARCHITECTURE.md §17: negotiated from Accept, never from User-Agent.
	if !media.AcceptsWebP(r.Header.Get("Accept")) {
		return d.originalBytes(rec)
	}

	quality := d.Images.Quality()
	body, err := d.Images.WebP(r.Context(), rel, rec)
	if err == nil {
		return body, "image/webp", RepresentationETag("webp-q"+strconv.Itoa(quality),
			d.Images.WebPKey(rec)), nil
	}
	// ErrNotWebP is not a failure: there is no conversion to do, whether because the
	// file already is WebP or because it is a GIF that must be served as stored.
	if errors.Is(err, media.ErrNotWebP) {
		return d.originalBytes(rec)
	}
	return nil, "", "", err
}

// originalBytes reads the stored file.
func (d Deps) originalBytes(rec media.MediaRecord) ([]byte, string, string, error) {
	if rec.Missing {
		return nil, "", "", media.ErrNotFound
	}
	f, err := d.Media.Open(rec.Rel)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, "", "", err
	}
	// One byte past the declared size, so a file that grew behind our back is
	// detected rather than silently truncated.
	body, err := io.ReadAll(io.LimitReader(f, info.Size()+1))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(body)) != info.Size() {
		return nil, "", "", errors.New("media: file changed size while being read")
	}

	mimeType := rec.MIME
	if mimeType == "" {
		mimeType = MimeFromPath(rec.Rel)
	}
	// The ETag must identify content, so a replaced file gets a new one even though
	// the URL is unchanged. The recorded checksum is exactly that; a file with no row
	// falls back to size and mtime, which is weaker but still changes on replacement.
	identity := rec.Checksum()
	if identity == "" {
		identity = strconv.FormatInt(info.Size(), 10) + "-" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	}
	return body, mimeType, RepresentationETag("original", identity), nil
}

// serveInertMedia serves a file that is not a vetted image type.
//
// ARCHITECTURE.md §87: uploads of anything but an image are refused at the door, so
// reaching this branch means the filesystem was modified outside the application. It
// is served as a download inside a sandbox rather than interpreted in this origin.
func (d Deps) serveInertMedia(w http.ResponseWriter, r *http.Request, rel string) {
	body, err := d.readWholeMediaFile(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Debug("media_client_gone", "path", rel, "error", err)
	}
}

func (d Deps) readWholeMediaFile(rel string) ([]byte, error) {
	f, err := d.Media.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(f, info.Size()+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != info.Size() {
		return nil, errors.New("media: file changed size while being read")
	}
	return body, nil
}

// RepresentationETag builds a tag that identifies one representation.
//
// ARCHITECTURE.md §89: `photo.jpg` produces two different bodies — a WebP and a
// JPEG — so a strong ETag shared between them would let a client holding the JPEG be
// answered 304 when it asked for the WebP. The representation name is part of the
// tag for exactly that reason.
func RepresentationETag(representation, identity string) string {
	sum := sha256.Sum256([]byte(representation + "\x00" + identity))
	return `"` + representation + "-" + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches implements If-None-Match comparison, including `*` and the weak form.
func etagMatches(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// MimeFromPath maps a stored extension to the type the delivery layer serves.
//
// The extension here is one the *server* generated from detected content, so it is
// not the untrusted value an upload's filename would have been
// (ARCHITECTURE.md §19).
func MimeFromPath(rel string) string {
	dot := strings.LastIndex(rel, ".")
	if dot < 0 {
		return "application/octet-stream"
	}
	switch strings.ToLower(rel[dot+1:]) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// mediaRepo adapts the API's database handle to the media pipeline.
//
// ARCHITECTURE.md §21: SQLite has one connection, so this is exactly one indexed
// lookup per image request and no second open result set.
type mediaRepoAdapter struct {
	db *store.DB
}

func (d Deps) mediaRepo() media.Repo { return mediaRepoAdapter{db: d.DB} }

// MediaRecord implements media.Repo.
func (a mediaRepoAdapter) MediaRecord(ctx context.Context, rel string) (media.MediaRecord, error) {
	var rec media.MediaRecord
	err := a.db.QueryRowContext(ctx,
		`SELECT id, path, mime, size, sha256 FROM media WHERE path = ?`, rel).
		Scan(&rec.ID, &rec.Rel, &rec.MIME, &rec.Size, &rec.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, store.ErrNotFound
	}
	if err != nil {
		return rec, err
	}
	return rec, nil
}
