package api

// ARCHITECTURE.md §5/§20/§30/§60/§87/§103: the media
// delivery layer's refusals and its fallback.
//
// What these tests are for, and what they are not for. The
// fullstack suite proves what a browser sees — the negotiated
// representation, the headers, the caches. What it cannot
// cheaply observe is the behaviour around the failure: a
// conversion that must fall back to the original rather than
// 500ing the image, a non-image that must be sandboxed, a
// traversal that must be refused before the filesystem is
// ever touched. Those properties need a stack, and a stack
// is built once here and shared by every test below.

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"blogcms/internal/auth"
	"blogcms/internal/comments"
	"blogcms/internal/config"
	"blogcms/internal/content"
	"blogcms/internal/media"
	"blogcms/internal/ratelimit"
	"blogcms/internal/store"
)

// A stack in a temporary tree, shared by the handler tests below.
func testDeps(t *testing.T) Deps {
	t.Helper()

	root := t.TempDir()
	for _, dir := range []string{"content/posts", "content/pages", "content/system", "media", "data"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		ContentRoot:    filepath.Join(root, "content"),
		MediaRoot:      filepath.Join(root, "media"),
		DataRoot:       filepath.Join(root, "data"),
		DBPath:         filepath.Join(root, "data", "blog.db"),
		MaxJSONBody:    1 << 20,
		MaxUploadBytes: 5 << 20,
		ImageMaxPixels: 20_000_000,
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mediaStore := media.NewStore(cfg.MediaRoot)
	d := Deps{
		Cfg:            cfg,
		DB:             db,
		Auth:           auth.NewManager(db, time.Hour, time.Hour, nil, false),
		Admins:         auth.NewAdminStore(db),
		Throttle:       auth.NewLoginThrottle(db, 5, 20),
		Content:        content.NewStore(cfg.ContentRoot),
		Media:          mediaStore,
		Comments:       comments.NewStore(db),
		CommentLimiter: ratelimit.New(db, "comment:ip", 5, 1),
	}
	d.Images = media.NewPipeline(mediaStore, d.mediaRepo(), filepath.Join(cfg.DataRoot, "media-cache"),
		media.ImageConfig{Quality: media.DefaultQuality, MemoryCacheBytes: 1 << 20, MaxPixels: cfg.ImageMaxPixels})
	return d
}

func fetchMedia(t *testing.T, d Deps, rel, accept string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/media/"+rel, nil)
	req.SetPathValue("path", rel)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	d.deliverMedia(rec, req)
	return rec
}

// -----------------------------------------------------------------------------
// §60 / §103 a conversion failure falls back to the original
// -----------------------------------------------------------------------------

// TestUndecodableImageFallsBackToTheOriginal is the failure this phase could have
// shipped and did not.
//
// A file can pass every upload check and still be undecodable: the magic bytes say
// PNG and the body says nothing of the sort. That happens when something outside the
// application writes to MEDIA_ROOT. The delivery layer must then serve the original
// bytes with the original content type — not a 500, and not a truncated WebP that
// renders as a broken image with no explanation.
func TestUndecodableImageFallsBackToTheOriginal(t *testing.T) {
	d := testDeps(t)

	// Valid PNG signature, garbage after it.
	body := append([]byte("\x89PNG\r\n\x1a\n"), []byte("not really a png")...)
	rel := "2026/10/aaaaaaaaaaaaaaaabbbbbbbbbbbb.png"
	full := filepath.Join(d.Cfg.MediaRoot, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// A row exists, so the index points at a real asset. The checksum is of the file
	// as written, which is what the cache key is built from.
	sum := sha256.Sum256(body)
	if _, err := d.DB.ExecContext(t.Context(),
		`INSERT INTO media (id, filename, path, mime, size, width, height, alt, sha256, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		"aaaaaaaaaaaaaaaabbbbbbbbbbbb", "broken.png", rel, "image/png", len(body),
		nil, nil, sum[:], "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	res := fetchMedia(t, d, rel, "image/webp,*/*")

	if res.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 — a bad conversion must not fail the image", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("content type %q, want image/png: the original is served unchanged", got)
	}
	if got := res.Body.String(); got != string(body) {
		t.Errorf("the body is not the original (%d bytes vs %d)", len(got), len(body))
	}
	if got := res.Header().Get("Vary"); got != "Accept" {
		t.Errorf("Vary is %q, want Accept even on the fallback path", got)
	}
	if got := res.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff is %q", got)
	}
	// The failure is counted, which is what makes it diagnosable (§86).
	if stats := d.Images.Stats(); stats.Failures == 0 {
		t.Error("the conversion failure was not counted")
	}
}

// TestMissingFileIsFourOhFour covers §5 and §61: a row without a file is a 404, and
// nothing panics on the way there.
func TestMissingFileIsFourOhFour(t *testing.T) {
	d := testDeps(t)

	rel := "2026/10/ccccccccccccccccdddddddddddd.png"
	if _, err := d.DB.ExecContext(t.Context(),
		`INSERT INTO media (id, filename, path, mime, size, width, height, alt, sha256, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		"ccccccccccccccccdddddddddddd", "gone.png", rel, "image/png", 10,
		nil, nil, make([]byte, 32), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	for _, accept := range []string{"image/webp", "image/png", ""} {
		res := fetchMedia(t, d, rel, accept)
		if res.Code != http.StatusNotFound {
			t.Errorf("accept %q: status %d, want 404", accept, res.Code)
		}
	}
}

// TestUnknownExtensionIsServedInertly covers §87: only the vetted image types are
// ever served as an image, and anything else cannot execute in this origin.
func TestUnknownExtensionIsServedInertly(t *testing.T) {
	d := testDeps(t)

	rel := "2026/10/eeeeeeeeeeeeeeeeffffffff.html"
	full := filepath.Join(d.Cfg.MediaRoot, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("<script>alert(1)</script>"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := fetchMedia(t, d, rel, "image/webp")

	if got := res.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content type %q, want application/octet-stream", got)
	}
	if got := res.Header().Get("Content-Disposition"); got != "attachment" {
		t.Errorf("disposition %q, want attachment", got)
	}
	if got := res.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Errorf("a non-image must be sandboxed, got %q", got)
	}
}

// TestTraversalIsRefusedBeforeTheFilesystem covers §20.
func TestTraversalIsRefusedBeforeTheFilesystem(t *testing.T) {
	d := testDeps(t)

	for _, rel := range []string{
		"../../etc/passwd",
		"2026/10/../../../etc/passwd",
		"/etc/passwd",
		"",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/media/"+rel, nil)
		req.SetPathValue("path", rel)
		d.deliverMedia(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q: status %d, want 404", rel, rec.Code)
		}
	}
}
