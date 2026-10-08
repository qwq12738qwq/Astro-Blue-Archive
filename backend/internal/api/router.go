// Package api exposes the JSON API. Every handler in this package returns
// JSON. ARCHITECTURE.md §1 forbids HTML, templates or UI markup here.
package api

import (
	"context"
	"net/http"
	"os"
	"time"

	"blogcms/internal/auth"
	"blogcms/internal/comments"
	"blogcms/internal/config"
	"blogcms/internal/content"
	"blogcms/internal/httpx"
	"blogcms/internal/media"
	"blogcms/internal/ratelimit"
	"blogcms/internal/store"
)

// Deps are the shared dependencies of all handlers.
type Deps struct {
	Cfg            *config.Config
	DB             *store.DB
	Auth           *auth.Manager
	Admins         *auth.AdminStore
	Throttle       *auth.LoginThrottle
	Content        *content.Store
	Media          *media.Store
	Comments       *comments.Store
	CommentLimiter *ratelimit.Limiter
	// Images is the representation pipeline: WebP negotiation, conversion and both
	// caches. It is separate from Media because Media owns *storage* (accept,
	// delete, path containment) while Images owns *delivery* (what bytes to send
	// for a given Accept header). Two responsibilities, two objects, one truth on
	// disk.
	Images *media.Pipeline
}

// auditSingular maps a content kind onto the audit-log noun.
//
// ARCHITECTURE.md §22 prescribes the exact event names, which are singular
// (post.created) even though the directories are plural (content/posts/).
var auditSingular = map[content.Kind]string{
	content.KindPosts: "post",
	content.KindPages: "page",
}

// Audit appends to the audit log (ARCHITECTURE.md §22).
//
// This is an event log, never a revision history: it records that something
// happened and to what slug, never any content.
func (d Deps) Audit(ctx context.Context, kind, ref string) {
	_, _ = d.DB.ExecContext(ctx,
		`INSERT INTO content_event (kind, ref, actor, created_at) VALUES (?, ?, 'admin', ?)`,
		kind, ref, store.Now())
}

// AuditContent records a content mutation under the prescribed event name.
func (d Deps) AuditContent(ctx context.Context, kind content.Kind, action, ref string) {
	noun, ok := auditSingular[kind]
	if !ok {
		noun = string(kind)
	}
	d.Audit(ctx, noun+"."+action, ref)
}

// NewRouter builds the complete HTTP handler chain.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	registerHealth(mux, d)
	registerAuthRoutes(mux, d)
	registerSettingsRoutes(mux, d)
	registerAdminRoutes(mux, d)
	registerCustomCodeRoutes(mux, d)
	registerCustomAssetRoutes(mux, d)
	registerMarkdownTemplateRoutes(mux, d)
	registerMediaRoutes(mux, d)
	registerCommentsRoutes(mux, d)
	registerSiteRoutes(mux, d)
	registerRSSRoute(mux, d)

	// ARCHITECTURE.md §14: image bytes are an asset response, not a document. Go
	// serves them here because it owns the storage, the checksum and the caches;
	// Astro is the only process a browser can reach, and it forwards /media/* to
	// this route. Every handler in this package still returns JSON or an asset —
	// never an HTML page (ARCHITECTURE.md §1).
	registerMediaDeliveryRoutes(mux, d)

	// Catch-alls so an unrouted request still produces the JSON envelope.
	// net/http's default 404 is text/plain, which would violate ARCHITECTURE.md
	// §1 ("JSON only").
	mux.HandleFunc("/api/", jsonNotFound)
	mux.HandleFunc("/", jsonNotFound)

	var h http.Handler = mux
	h = httpx.RecoverMiddleware(h)
	h = httpx.LoggingMiddleware(h)
	h = httpx.RequestIDMiddleware(h)
	return h
}

func jsonNotFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
		"no such endpoint"))
}

func registerHealth(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/healthz", d.healthz)
	mux.HandleFunc("GET /api/v1/readyz", d.readyz)
}

// healthz reports process liveness only. It must not touch the database, so
// that a database outage cannot cause a restart loop (ARCHITECTURE.md §29).
func (d Deps) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

type readyCheck struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// readyz reports whether the service can actually serve traffic. It checks the
// database and every configured root, and returns 503 if any check fails.
func (d Deps) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := make([]readyCheck, 0, 5)

	// SQLite
	dbOK := true
	detail := ""
	if err := d.DB.PingContext(ctx); err != nil {
		dbOK, detail = false, err.Error()
	} else if err := d.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(new(int)); err != nil {
		dbOK, detail = false, err.Error()
	}
	checks = append(checks, readyCheck{Check: "sqlite", OK: dbOK, Detail: detail})

	// Filesystem roots. A missing root must fail readiness rather than yield an
	// empty blog (ARCHITECTURE.md §6).
	for _, c := range []struct {
		name string
		path string
	}{
		{"content_root", d.Cfg.ContentRoot},
		{"media_root", d.Cfg.MediaRoot},
		{"data_root", d.Cfg.DataRoot},
	} {
		checks = append(checks, dirCheck(c.name, c.path))
	}

	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}

	status := http.StatusOK
	state := "ready"
	if !allOK {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}

	httpx.WriteJSON(w, status, map[string]any{
		"status": state,
		"checks": checks,
	})
}

func dirCheck(name, path string) readyCheck {
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return readyCheck{Check: name, OK: false, Detail: err.Error()}
	case !st.IsDir():
		return readyCheck{Check: name, OK: false, Detail: "not a directory"}
	default:
		return readyCheck{Check: name, OK: true}
	}
}
