package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"blogcms/internal/httpx"
	"blogcms/internal/media"
	"blogcms/internal/store"
)

// Settings are runtime configuration, not content (ARCHITECTURE.md §3).
// They live in the `setting` table because they cannot be derived from files.
//
// Only allowlisted keys are accepted. An arbitrary key/value store would grow
// into a second, unvalidated configuration source.
const (
	settingSiteTitle           = "siteTitle"
	settingSiteSubtitle        = "siteSubtitle"
	settingSiteDescription     = "siteDescription"
	settingSiteIconMediaID     = "siteIconMediaId"
	settingWebPQuality         = "webpQuality"
	settingImageMemoryCacheMB  = "imageMemoryCacheMB"
	settingRSSEnabled          = "rssEnabled"
	settingRSSTitle            = "rssTitle"
	settingRSSDescription      = "rssDescription"
	settingRSSItemLimit        = "rssItemLimit"
	settingCommentsEnabled     = "commentsEnabled"
	settingCommentAutoModerate = "commentAutoModerate"
	settingThemeID             = "themeId"
)

// defaultThemeID is used when nothing is stored, or when a stored value no longer
// names an installed theme. It must match DEFAULT_THEME_ID in
// astro/src/theme-system/ids.ts.
const defaultThemeID = "bluearchive"

// knownThemeIDs is the backend's mirror of the frontend Theme Registry.
//
// ARCHITECTURE.md §26: a theme is server code, so choosing one must be exactly as
// constrained as choosing a binary. The browser sends an id; this list is what
// decides whether it is one of the themes the deployer actually installed. There is
// deliberately no way to send a path, a module specifier or a URL — an
// arbitrary value here would turn "pick a theme" into "run uploaded code on the
// server".
//
// scripts/arch-check.mjs asserts that this list, THEME_IDS in
// astro/src/theme-system/ids.ts and the keys of astro/src/theme-system/registry.ts are the same
// set, so a theme cannot be added to one and forgotten in another.
var knownThemeIDs = []string{"bluearchive"}

// isKnownThemeID reports whether id names an installed theme. The comparison is
// exact: no trimming, no case folding, no prefix matching.
func isKnownThemeID(id string) bool {
	for _, known := range knownThemeIDs {
		if id == known {
			return true
		}
	}
	return false
}

// Field limits. Every setting is validated on write rather than clamped, because a
// silently clamped value is a setting the admin believes they changed.
const (
	SiteTitleMax       = 120
	SiteSubtitleMax    = 200
	SiteDescriptionMax = 300
	MediaIDMax         = 64

	// WebPQualityMin/Max bound the encoder quality. 1 is unusably small and 100 is
	// larger than the original; neither is an accident worth rejecting a post over,
	// so they are clamped rather than refused, but anything outside them is a
	// mistake worth surfacing.
	WebPQualityMin = 1
	WebPQualityMax = 100

	// ImageMemoryCacheMBMin/Max keep the admin from setting a cache that either
	// does nothing (1 MB for a site of 200 large images) or quietly reserves a
	// gigabyte of heap.
	ImageMemoryCacheMBMin = 1
	ImageMemoryCacheMBMax = 512

	// RSSItemLimitMin/Max: a feed is not infinite (ARCHITECTURE.md §68), and one
	// with a million items is a denial of service aimed at every reader.
	RSSItemLimitMin = 1
	RSSItemLimitMax = 100
)

// Settings is the whole runtime settings document.
//
// ARCHITECTURE.md §40: every one of these is site metadata, not theme metadata.
// A theme reads them; none of them is defined by, or defaulting to, a theme.
type Settings struct {
	SiteTitle       string `json:"siteTitle"`
	SiteSubtitle    string `json:"siteSubtitle"`
	SiteDescription string `json:"siteDescription"`
	// SiteIconMediaID points at a `media` row, never at a path. The icon is
	// therefore an ordinary asset: it is validated on upload like any other image,
	// served through the delivery layer like any other image, and cannot be deleted
	// while it is in use (ARCHITECTURE.md §116).
	SiteIconMediaID string `json:"siteIconMediaId"`

	WebPQuality        int `json:"webpQuality"`
	ImageMemoryCacheMB int `json:"imageMemoryCacheMB"`

	RSSEnabled     bool   `json:"rssEnabled"`
	RSSTitle       string `json:"rssTitle"`
	RSSDescription string `json:"rssDescription"`
	RSSItemLimit   int    `json:"rssItemLimit"`

	CommentsEnabled     bool   `json:"commentsEnabled"`
	CommentAutoModerate bool   `json:"commentAutoModerate"`
	ThemeID             string `json:"themeId"`
}

func defaultSettings() Settings {
	return Settings{
		SiteTitle:           "Blog",
		SiteSubtitle:        "",
		SiteDescription:     "",
		SiteIconMediaID:     "",
		WebPQuality:         media.DefaultQuality,
		ImageMemoryCacheMB:  64,
		RSSEnabled:          true,
		RSSTitle:            "",
		RSSDescription:      "",
		RSSItemLimit:        20,
		CommentsEnabled:     true,
		CommentAutoModerate: true,
		ThemeID:             defaultThemeID,
	}
}

// settingOrder fixes the lookup order so the result is deterministic.
var settingOrder = []string{
	settingSiteTitle,
	settingSiteSubtitle,
	settingSiteDescription,
	settingSiteIconMediaID,
	settingWebPQuality,
	settingImageMemoryCacheMB,
	settingRSSEnabled,
	settingRSSTitle,
	settingRSSDescription,
	settingRSSItemLimit,
	settingCommentsEnabled,
	settingCommentAutoModerate,
	settingThemeID,
}

func registerSettingsRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/admin/settings", d.requireSession(d.getSettings))
	mux.HandleFunc("PUT /api/v1/admin/settings", d.requireSession(d.putSettings))
}

// loadSettings reads the settings, falling back to defaults for missing or
// corrupt rows so a bad value cannot take the site down.
func (d Deps) loadSettings(ctx context.Context) (Settings, error) {
	out := defaultSettings()

	// The key list is a fixed Go slice, never request input, so it is safe to expand
	// into the argument list. `IN (?,?,…)` rather than a built query string keeps the
	// statement shape constant, which is what makes it cacheable and injectable.
	args := make([]any, 0, len(settingOrder))
	for _, key := range settingOrder {
		args = append(args, key)
	}

	rows, err := d.DB.QueryContext(ctx,
		`SELECT key, value FROM setting WHERE key IN (`+placeholders(len(settingOrder))+`)`, args...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return out, err
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue
		}
		switch key {
		case settingSiteTitle:
			if s, ok := v.(string); ok {
				out.SiteTitle = s
			}
		case settingSiteSubtitle:
			if s, ok := v.(string); ok {
				out.SiteSubtitle = s
			}
		case settingSiteDescription:
			if s, ok := v.(string); ok {
				out.SiteDescription = s
			}
		case settingSiteIconMediaID:
			if s, ok := v.(string); ok {
				out.SiteIconMediaID = s
			}

		case settingWebPQuality:
			if n, ok := intValue(v); ok && n >= WebPQualityMin && n <= WebPQualityMax {
				out.WebPQuality = n
			}
		case settingImageMemoryCacheMB:
			if n, ok := intValue(v); ok && n >= ImageMemoryCacheMBMin && n <= ImageMemoryCacheMBMax {
				out.ImageMemoryCacheMB = n
			}

		case settingRSSEnabled:
			if b, ok := v.(bool); ok {
				out.RSSEnabled = b
			}
		case settingRSSTitle:
			if s, ok := v.(string); ok {
				out.RSSTitle = s
			}
		case settingRSSDescription:
			if s, ok := v.(string); ok {
				out.RSSDescription = s
			}
		case settingRSSItemLimit:
			if n, ok := intValue(v); ok && n >= RSSItemLimitMin && n <= RSSItemLimitMax {
				out.RSSItemLimit = n
			}

		case settingCommentsEnabled:
			if b, ok := v.(bool); ok {
				out.CommentsEnabled = b
			}
		case settingCommentAutoModerate:
			if b, ok := v.(bool); ok {
				out.CommentAutoModerate = b
			}
		case settingThemeID:
			// A stored value can stop being valid — the theme was uninstalled
			// after the setting was saved. Fall back rather than render nothing:
			// the frontend resolver does the same, so both halves agree.
			if s, ok := v.(string); ok && isKnownThemeID(s) {
				out.ThemeID = s
			}
		}
	}
	return out, rows.Err()
}

// intValue accepts both 42 and 42.0, because a JSON round trip through another
// language can turn an integer into a float and a rejected settings save would be
// baffling.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// LoadSettings reads the settings directly from the database.
//
// ARCHITECTURE.md §84: startup reads the stored image settings so a saved WebP
// quality or cache size survives a restart. It is exported rather than smuggled
// through a handler because cmd/server is a different package from api, and a
// synthetic HTTP round trip at boot would be a strange way to read one row.
func LoadSettings(db *store.DB) (Settings, error) {
	return Deps{DB: db}.loadSettings(context.Background())
}

// RebuildUsageIndex rescans content/ and replaces the derived media usage index.
//
// It is exported so the server can warm the index at startup (ARCHITECTURE.md §51)
// using the same code path the admin button uses, rather than a second
// implementation that could disagree about what a reference is.
func RebuildUsageIndex(ctx context.Context, d *Deps) (stored, unmatched int, err error) {
	refs, err := media.ScanUsage(d.Cfg.ContentRoot)
	if err != nil {
		return 0, 0, err
	}
	lookup, err := resolveMediaIDs(ctx, d.DB, refs)
	if err != nil {
		return 0, 0, err
	}
	now := store.Now()
	err = d.DB.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_usage`); err != nil {
			return err
		}
		for _, ref := range refs {
			id, ok := lookup[ref.MediaPath]
			if !ok {
				unmatched++
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO media_usage (media_id, content_type, content_slug, reference_count, last_seen_at)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(media_id, content_type, content_slug)
				 DO UPDATE SET reference_count = excluded.reference_count, last_seen_at = excluded.last_seen_at`,
				id, ref.Kind, ref.Slug, ref.ReferenceCount, now); err != nil {
				return err
			}
			stored++
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return stored, unmatched, nil
}

// usageIndexIsStale reports whether content/ has been touched since the index was
// last built.
//
// ARCHITECTURE.md §50 calls this table derived, and a derived table that silently
// disagrees with its source is worse than no table: the media screen would say an
// image is unused while a post visibly uses it, and the delete guard (§54) would let
// the admin remove a file that is in use. Comparing the newest content mtime with the
// newest `last_seen_at` is one stat pass over two directories and one indexed MAX —
// cheap enough to run on every media screen load, which is what makes the index look
// live without anyone pressing Rebuild.
func (d Deps) usageIndexIsStale(ctx context.Context) (bool, error) {
	newestContent, err := media.NewestContentModTime(d.Cfg.ContentRoot)
	if err != nil {
		return false, err
	}
	if newestContent.IsZero() {
		return false, nil
	}

	var indexed sql.NullString
	err = d.DB.QueryRowContext(ctx, `SELECT max(last_seen_at) FROM media_usage`).Scan(&indexed)
	if err != nil {
		return false, err
	}
	if !indexed.Valid {
		// An empty index is stale whenever there is content at all: it says nothing is
		// referenced, which is a claim, not an absence of one.
		return true, nil
	}

	built, err := time.Parse(time.RFC3339, indexed.String)
	if err != nil {
		return true, nil
	}
	// A whole second of slack: an mtime is only second-granular on some
	// filesystems, and a file written in the same second as the scan is not stale.
	return newestContent.After(built.Add(time.Second)), nil
}

// resolveMediaIDs maps scanned storage paths onto media row ids.
//
// A reference to a path with no row is left out rather than stored: the index
// answers "which of MY files is this", and a row pointing at nothing would be a
// broken foreign key in spirit.
func resolveMediaIDs(ctx context.Context, db *store.DB, refs []media.UsageRef) (map[string]string, error) {
	paths := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		if !seen[ref.MediaPath] {
			seen[ref.MediaPath] = true
			paths = append(paths, ref.MediaPath)
		}
	}

	out := make(map[string]string, len(paths))
	for _, chunk := range chunkStrings(paths, 100) {
		query := `SELECT id, path FROM media WHERE path IN (` + placeholders(len(chunk)) + `)`
		args := make([]any, 0, len(chunk))
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, path string
			if err := rows.Scan(&id, &path); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[path] = id
		}
		_ = rows.Close()
	}
	return out, nil
}

func (d Deps) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := d.loadSettings(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, settings.settingsResponse(r.Context(), d))
}

// settingsResponse adds the derived, non-stored facts the admin UI needs.
//
// The icon URL is computed here rather than in the frontend so that the storage
// layout stays in the backend: the admin screen must never learn where the file
// actually lives on disk.
func (s Settings) settingsResponse(ctx context.Context, d Deps) map[string]any {
	iconURL, iconVersion := d.iconRef(ctx, s.SiteIconMediaID)
	return map[string]any{
		"siteTitle":           s.SiteTitle,
		"siteSubtitle":        s.SiteSubtitle,
		"siteDescription":     s.SiteDescription,
		"siteIconMediaId":     s.SiteIconMediaID,
		"siteIconUrl":         iconURL,
		"siteIconVersion":     iconVersion,
		"webpQuality":         s.WebPQuality,
		"imageMemoryCacheMB":  s.ImageMemoryCacheMB,
		"rssEnabled":          s.RSSEnabled,
		"rssTitle":            s.RSSTitle,
		"rssDescription":      s.RSSDescription,
		"rssItemLimit":        s.RSSItemLimit,
		"commentsEnabled":     s.CommentsEnabled,
		"commentAutoModerate": s.CommentAutoModerate,
		"themeId":             s.ThemeID,
	}
}

// CommentsEnabled reports whether anonymous comments are accepted. It is read on
// every submission so an admin toggle takes effect without a restart.
func (d Deps) CommentsEnabled(ctx context.Context) bool {
	settings, err := d.loadSettings(ctx)
	if err != nil {
		return defaultSettings().CommentsEnabled
	}
	return settings.CommentsEnabled
}

// AutoModerate reports whether new comments are held for review.
func (d Deps) AutoModerate(ctx context.Context) bool {
	settings, err := d.loadSettings(ctx)
	if err != nil {
		return defaultSettings().CommentAutoModerate
	}
	return settings.CommentAutoModerate
}

// settingsPatch is a partial update: every field is a pointer, so an absent field
// means "leave it alone".
//
// This is not a convenience. A client that has been around for one field fewer than
// this one — an older admin screen, a script, a curl in a runbook — would otherwise be
// told `webpQuality must be 1-100` because of a field it never knew about, and the only
// way to satisfy the API would be to send a value the caller did not intend. Worse, a
// boolean sent as absent would silently switch a feature *off*: `commentsEnabled`
// defaulting to false is a plausible way to lose a setting nobody was editing. An
// explicit `false` is still applied; an omission is not an instruction.
type settingsPatch struct {
	SiteTitle           *string `json:"siteTitle"`
	SiteSubtitle        *string `json:"siteSubtitle"`
	SiteDescription     *string `json:"siteDescription"`
	SiteIconMediaID     *string `json:"siteIconMediaId"`
	WebPQuality         *int    `json:"webpQuality"`
	ImageMemoryCacheMB  *int    `json:"imageMemoryCacheMB"`
	RSSEnabled          *bool   `json:"rssEnabled"`
	RSSTitle            *string `json:"rssTitle"`
	RSSDescription      *string `json:"rssDescription"`
	RSSItemLimit        *int    `json:"rssItemLimit"`
	CommentsEnabled     *bool   `json:"commentsEnabled"`
	CommentAutoModerate *bool   `json:"commentAutoModerate"`
	ThemeID             *string `json:"themeId"`
}

// apply folds a patch onto the stored settings and returns the result.
func (p settingsPatch) apply(current Settings) Settings {
	next := current
	if p.SiteTitle != nil {
		next.SiteTitle = strings.TrimSpace(*p.SiteTitle)
	}
	if p.SiteSubtitle != nil {
		next.SiteSubtitle = strings.TrimSpace(*p.SiteSubtitle)
	}
	if p.SiteDescription != nil {
		next.SiteDescription = strings.TrimSpace(*p.SiteDescription)
	}
	if p.SiteIconMediaID != nil {
		next.SiteIconMediaID = strings.TrimSpace(*p.SiteIconMediaID)
	}
	if p.RSSTitle != nil {
		next.RSSTitle = strings.TrimSpace(*p.RSSTitle)
	}
	if p.RSSDescription != nil {
		next.RSSDescription = strings.TrimSpace(*p.RSSDescription)
	}
	if p.WebPQuality != nil {
		next.WebPQuality = *p.WebPQuality
	}
	if p.ImageMemoryCacheMB != nil {
		next.ImageMemoryCacheMB = *p.ImageMemoryCacheMB
	}
	if p.RSSItemLimit != nil {
		next.RSSItemLimit = *p.RSSItemLimit
	}
	if p.RSSEnabled != nil {
		next.RSSEnabled = *p.RSSEnabled
	}
	if p.CommentsEnabled != nil {
		next.CommentsEnabled = *p.CommentsEnabled
	}
	if p.CommentAutoModerate != nil {
		next.CommentAutoModerate = *p.CommentAutoModerate
	}
	if p.ThemeID != nil {
		// Deliberately NOT trimmed: an identifier is not a human string, so "plain "
		// and "plain" are different inputs and normalising would store something the
		// caller did not send. Reject instead.
		next.ThemeID = *p.ThemeID
	}
	return next
}

func (d Deps) putSettings(w http.ResponseWriter, r *http.Request) {
	var patch settingsPatch
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &patch); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	current, err := d.loadSettings(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	next := patch.apply(current)

	fields := map[string]string{}
	if n := len([]rune(next.SiteTitle)); n == 0 || n > SiteTitleMax {
		fields["siteTitle"] = fmt.Sprintf("must be 1-%d characters", SiteTitleMax)
	}
	if n := len([]rune(next.SiteSubtitle)); n > SiteSubtitleMax {
		fields["siteSubtitle"] = fmt.Sprintf("must be at most %d characters", SiteSubtitleMax)
	}
	if n := len([]rune(next.SiteDescription)); n > SiteDescriptionMax {
		fields["siteDescription"] = fmt.Sprintf("must be at most %d characters", SiteDescriptionMax)
	}
	if n := len([]rune(next.RSSTitle)); n > SiteTitleMax {
		fields["rssTitle"] = fmt.Sprintf("must be at most %d characters", SiteTitleMax)
	}
	if n := len([]rune(next.RSSDescription)); n > SiteDescriptionMax {
		fields["rssDescription"] = fmt.Sprintf("must be at most %d characters", SiteDescriptionMax)
	}
	if next.WebPQuality < WebPQualityMin || next.WebPQuality > WebPQualityMax {
		fields["webpQuality"] = fmt.Sprintf("must be %d-%d", WebPQualityMin, WebPQualityMax)
	}
	if next.ImageMemoryCacheMB < ImageMemoryCacheMBMin || next.ImageMemoryCacheMB > ImageMemoryCacheMBMax {
		fields["imageMemoryCacheMB"] = fmt.Sprintf("must be %d-%d", ImageMemoryCacheMBMin, ImageMemoryCacheMBMax)
	}
	if next.RSSItemLimit < RSSItemLimitMin || next.RSSItemLimit > RSSItemLimitMax {
		fields["rssItemLimit"] = fmt.Sprintf("must be %d-%d", RSSItemLimitMin, RSSItemLimitMax)
	}
	if !isKnownThemeID(next.ThemeID) {
		fields["themeId"] = "must be one of: " + strings.Join(knownThemeIDs, ", ")
	}

	// The icon must name a media row that exists. Accepting an unknown id would
	// store a reference the delivery layer can never resolve, and the site would
	// render with no icon and no error.
	if next.SiteIconMediaID != "" {
		if len(next.SiteIconMediaID) > MediaIDMax || !media.ValidID(next.SiteIconMediaID) {
			fields["siteIconMediaId"] = "is not a valid media id"
		} else {
			var exists int
			err := d.DB.QueryRowContext(r.Context(),
				`SELECT count(*) FROM media WHERE id = ?`, next.SiteIconMediaID).Scan(&exists)
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if exists == 0 {
				fields["siteIconMediaId"] = "no such media item"
			}
		}
	}

	if len(fields) > 0 {
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}

	ctx := r.Context()
	values := map[string]any{
		settingSiteTitle:           next.SiteTitle,
		settingSiteSubtitle:        next.SiteSubtitle,
		settingSiteDescription:     next.SiteDescription,
		settingSiteIconMediaID:     next.SiteIconMediaID,
		settingWebPQuality:         next.WebPQuality,
		settingImageMemoryCacheMB:  next.ImageMemoryCacheMB,
		settingRSSEnabled:          next.RSSEnabled,
		settingRSSTitle:            next.RSSTitle,
		settingRSSDescription:      next.RSSDescription,
		settingRSSItemLimit:        next.RSSItemLimit,
		settingCommentsEnabled:     next.CommentsEnabled,
		settingCommentAutoModerate: next.CommentAutoModerate,
		settingThemeID:             next.ThemeID,
	}

	err = d.DB.WithTx(ctx, func(tx *sql.Tx) error {
		for _, key := range settingOrder {
			raw, err := json.Marshal(values[key])
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO setting (key, value, updated_at) VALUES (?, ?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				key, string(raw), store.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// The image pipeline is stateful, so a settings write has to reach it. Doing it
	// here rather than reading the value per request is what makes the change take
	// effect immediately and without a restart (ARCHITECTURE.md §84), and doing it
	// here rather than at startup is what makes it survive a deploy.
	d.Images.Configure(media.ImageConfig{
		Quality:          next.WebPQuality,
		MemoryCacheBytes: int64(next.ImageMemoryCacheMB) << 20,
		MaxPixels:        d.Cfg.ImageMaxPixels,
	})

	d.Audit(ctx, "settings.update", "settings")
	httpx.WriteJSON(w, http.StatusOK, next.settingsResponse(ctx, d))
}
