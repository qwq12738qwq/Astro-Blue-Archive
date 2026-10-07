// Command server runs the blog CMS JSON API.
//
// ARCHITECTURE.md §1: this process serves JSON only. It never renders HTML.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"blogcms/internal/api"
	"blogcms/internal/auth"
	"blogcms/internal/comments"
	"blogcms/internal/config"
	"blogcms/internal/content"
	"blogcms/internal/httpx"
	"blogcms/internal/media"
	"blogcms/internal/ratelimit"
	"blogcms/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	slog.Info("configuration loaded",
		"content_root", cfg.ContentRoot,
		"media_root", cfg.MediaRoot,
		"data_root", cfg.DataRoot,
		"public_origin", cfg.PublicOrigin,
		"secure_cookies", cfg.SecureCookie,
	)

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	authMgr := auth.NewManager(db, cfg.SessionTTL, cfg.SessionIdleTTL, cfg.PublicOrigins, cfg.SecureCookie)
	admins := auth.NewAdminStore(db)
	throttle := auth.NewLoginThrottle(db, cfg.LoginPerMinute, cfg.LoginPerHour)

	// Expired sessions and old login attempts are removed periodically so the
	// tables cannot grow without bound.
	stopJanitor := startJanitor(authMgr, throttle)
	defer stopJanitor()

	deps := api.Deps{
		Cfg:            cfg,
		DB:             db,
		Auth:           authMgr,
		Admins:         admins,
		Throttle:       throttle,
		Content:        content.NewStore(cfg.ContentRoot),
		Media:          media.NewStore(cfg.MediaRoot),
		Comments:       comments.NewStore(db),
		CommentLimiter: ratelimit.New(db, "comment:ip", cfg.CommentPerHour, time.Hour),
	}

	// The image pipeline starts on the environment defaults and is re-configured from
	// the stored settings once they are readable, so an admin's saved WebP quality
	// survives a restart instead of quietly reverting to the default.
	deps.Images = media.NewPipeline(deps.Media, mediaRepoFor(db, deps.Media), cfg.MediaCacheRoot,
		media.ImageConfig{
			Quality:          media.DefaultQuality,
			MemoryCacheBytes: int64(cfg.ImageMemoryCacheMB) << 20,
			MaxPixels:        cfg.ImageMaxPixels,
		})
	if settings, err := api.LoadSettings(db); err == nil {
		deps.Images.Configure(media.ImageConfig{
			Quality:          settings.WebPQuality,
			MemoryCacheBytes: int64(settings.ImageMemoryCacheMB) << 20,
			MaxPixels:        cfg.ImageMaxPixels,
		})
	} else {
		slog.Warn("image settings not applied", "error", err)
	}
	// Warm the derived usage index so the media screen is right on first load. It is
	// derived, so a failure here costs an empty index and nothing else.
	go warmUsageIndex(&deps)

	srv := httpx.NewServer(cfg.Addr, api.NewRouter(deps))

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("shutdown complete")
	return nil
}

// mediaRepoFor adapts the database handle to the one lookup the image pipeline
// needs. It lives here rather than in the pipeline so that package never depends on
// SQLite (ARCHITECTURE.md §36).
func mediaRepoFor(db *store.DB, files *media.Store) media.Repo {
	return media.RepoFunc(func(ctx context.Context, rel string) (media.MediaRecord, error) {
		var rec media.MediaRecord
		err := db.QueryRowContext(ctx,
			`SELECT id, path, mime, size, sha256 FROM media WHERE path = ?`, rel).
			Scan(&rec.ID, &rec.Rel, &rec.MIME, &rec.Size, &rec.SHA256)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rec, store.ErrNotFound
			}
			return rec, err
		}
		return rec, nil
	})
}

// warmUsageIndex rescans content/ in the background so the media screen has
// reference counts without waiting for an admin to press Rebuild.
//
// It runs after the server starts listening rather than before: a large content
// directory should not delay readiness, and the index is derived, so being a moment
// late costs nothing.
func warmUsageIndex(d *api.Deps) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, unmatched, err := api.RebuildUsageIndex(ctx, d)
	if err != nil {
		slog.Warn("media usage index warm-up failed", "error", err)
		return
	}
	slog.Info("media usage index rebuilt", "references", n, "unmatched", unmatched)
}
func startJanitor(m *auth.Manager, t *auth.LoginThrottle) func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
				if n, err := m.PurgeExpired(runCtx); err != nil {
					slog.Warn("purge expired sessions", "error", err)
				} else if n > 0 {
					slog.Info("purged expired sessions", "count", n)
				}
				if err := t.Purge(runCtx, 24*time.Hour); err != nil {
					slog.Warn("purge login attempts", "error", err)
				}
				runCancel()
			}
		}
	}()
	return cancel
}
