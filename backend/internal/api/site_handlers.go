package api

import (
	"net/http"

	"blogcms/internal/httpx"
)

// registerSiteRoutes exposes the settings a public page legitimately needs.
func registerSiteRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/site", d.getSite)
}

// getSite returns public presentation settings.
//
// ARCHITECTURE.md §3: settings are runtime state, not content, so they live in
// SQLite. ARCHITECTURE.md §40: every field here is site metadata and none of it is
// theme metadata. A theme reads these values and defines none of them, so swapping
// the theme cannot change the site's name, its description or its icon.
//
// The response is deliberately assembled rather than `settings.settingsResponse`:
// the admin document carries moderation and cache settings that a public reader has
// no business learning, even by inference. The public site needs the identity of the
// site and nothing else.
//
// `themeId` is here because the frontend resolves the theme in middleware, before
// any page renders. It is an identifier from a fixed list, not a path: exposing it
// lets the browser cache and render with the right theme without any of this
// backend's state becoming public.
func (d Deps) getSite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, err := d.loadSettings(ctx)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// ARCHITECTURE.md §76: the icon URL carries a version token, because a browser
	// caches a favicon far more aggressively than it caches a page. The token comes
	// from the asset's own checksum, so it changes exactly when the icon does and
	// the storage path is never rewritten.
	iconURL, iconVersion := d.iconRef(ctx, settings.SiteIconMediaID)

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"siteTitle":           settings.SiteTitle,
		"siteSubtitle":        settings.SiteSubtitle,
		"siteDescription":     settings.SiteDescription,
		"siteIconUrl":         iconURL,
		"siteIconVersion":     iconVersion,
		"rssEnabled":          settings.RSSEnabled,
		"rssTitle":            settings.RSSTitle,
		"rssDescription":      settings.RSSDescription,
		"rssItemLimit":        settings.RSSItemLimit,
		"commentsEnabled":     settings.CommentsEnabled,
		"commentAutoModerate": settings.CommentAutoModerate,
		"themeId":             settings.ThemeID,
	})
}
