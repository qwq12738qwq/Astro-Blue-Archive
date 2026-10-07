package api

// ARCHITECTURE.md §28: the RSS feed belongs to the Go backend.
//
// The feed was an Astro endpoint for as long as Astro was the only
// process a browser could reach. That was never a reason for it to
// live there: the items come from content/ — which this backend
// already owns as its sole writer — and the settings that shape the
// channel are rows in the database this backend already owns. One
// process now holds every fact the feed states, and Astro's only
// remaining job on the path is the rewrite that keeps the public
// URL stable.
//
// The invariants the endpoint carries over from its Astro life: a
// disabled feed is a 404 rather than an empty channel (§65), the
// item count is bounded (§68), every link is absolute (§70), covers
// are media:content rather than an enclosure carrying a lie for a
// length (§70), drafts and comments never appear (§107), and one
// bad file never takes the feed down for every reader (§7).

import (
	"net/http"
	"strings"
	"time"

	"blogcms/internal/content"
	"blogcms/internal/httpx"
	"blogcms/internal/media"
)

// registerRSSRoute exposes the feed. It is a public route: a reader
// subscribes without an account, and the settings it reads are the
// same ones /api/v1/site already publishes.
func registerRSSRoute(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/rss", d.getRSS)
}

func (d Deps) getRSS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	settings, err := d.loadSettings(ctx)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// §65: a disabled feed is not a feed. A 404 rather than an empty
	// channel is what makes the setting observable, and it is why the
	// discovery link is omitted too — an <link rel="alternate"> to a
	// 404 teaches readers to ignore the element entirely.
	if !settings.RSSEnabled {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Feed disabled"))
		return
	}

	// §64: items come from content/, newest first — that is the order
	// List already returns. Drafts are excluded here rather than by
	// the store, because the admin legitimately lists them; and a
	// malformed file is skipped by List itself (§7), so one bad file
	// cannot take the feed down.
	summaries, _, err := d.Content.List(content.KindPosts)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	limit := clampRSSLimit(settings.RSSItemLimit)
	items := make([]content.Summary, 0, limit)
	for _, s := range summaries {
		if s.Draft {
			continue
		}
		if len(items) == limit {
			break
		}
		items = append(items, s)
	}

	body := rssDocument(d.Cfg.PublicOrigin, settings, items)

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	// §106: a short TTL, because the feed is generated per request and
	// a new post should appear promptly. It is not no-cache — a reader
	// hammering a personal blog every second helps nobody.
	w.Header().Set("Cache-Control", "public, max-age=300")
	// Not negotiated, but a shared cache must still key on the settings
	// that produced it: switching the feed off has to be observable.
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// rssDocument assembles the channel. The origin is the public origin
// the backend was configured with, so every URL a reader resolves is
// the address the site was meant to be reached at (§70).
//
// The channel title and description resolve the same way the frontend
// always did (§66/§67): the RSS-specific setting first, then the site
// identity it falls back to — a feed with an empty title would show
// the setting, not the site, as empty.
func rssDocument(origin string, settings Settings, items []content.Summary) string {
	origin = strings.TrimSuffix(origin, "/")
	self := origin + "/rss.xml"

	title := strings.TrimSpace(settings.RSSTitle)
	if title == "" {
		title = settings.SiteTitle
	}
	description := strings.TrimSpace(settings.RSSDescription)
	if description == "" {
		description = strings.TrimSpace(settings.SiteDescription)
	}
	if description == "" {
		description = strings.TrimSpace(settings.SiteSubtitle)
	}
	if description == "" {
		description = settings.SiteTitle
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/">
  <channel>
`)
	b.WriteString("    <title>" + xmlEscape(title) + "</title>\n")
	b.WriteString("    <link>" + xmlEscape(origin) + "</link>\n")
	b.WriteString("    <description>" + xmlEscape(description) + "</description>\n")
	b.WriteString("    <language>en</language>\n")
	b.WriteString(`    <atom:link href="` + xmlEscape(self) + `" rel="self" type="application/rss+xml"/>` + "\n")
	b.WriteString("    <lastBuildDate>" + time.Now().UTC().Format(time.RFC1123Z) + "</lastBuildDate>\n")

	for _, p := range items {
		link := origin + "/posts/" + p.Slug
		b.WriteString("    <item>\n")
		b.WriteString("      <title>" + xmlEscape(p.Title) + "</title>\n")
		b.WriteString("      <link>" + xmlEscape(link) + "</link>\n")
		b.WriteString(`      <guid isPermaLink="true">` + xmlEscape(link) + "</guid>\n")
		b.WriteString("      <pubDate>" + rssPubDate(p) + "</pubDate>\n")
		if p.Description != "" {
			b.WriteString("      <description>" + xmlEscape(p.Description) + "</description>\n")
		}
		// §70: media:content rather than <enclosure>. An enclosure is
		// supposed to carry a byte length, and the backend does not stat
		// the file to produce one; a length="0" would be a lie a reader
		// could act on, and an absent length is what every real publisher
		// sends. The URL is the original, because WebP is chosen per
		// request from the reader's Accept header and never appears in a
		// URL (§24).
		if p.Cover != "" {
			cover := origin + media.PublicURL(p.Cover)
			b.WriteString(`      <media:content url="` + xmlEscape(cover) + `" medium="image" />` + "\n")
		}
		for _, t := range p.Tags {
			b.WriteString("      <category>" + xmlEscape(t) + "</category>\n")
		}
		b.WriteString("    </item>\n")
	}

	b.WriteString("  </channel>\n</rss>\n")
	return b.String()
}

// rssPubDate stamps an item. A post without a declared date falls back
// to the file's own modification time, so the feed still orders it.
func rssPubDate(p content.Summary) string {
	when := p.ModTime
	if p.Date != nil {
		when = *p.Date
	}
	return when.UTC().Format(time.RFC1123Z)
}

// The item limit, clamped to the range the backend accepts (§68).
func clampRSSLimit(value int) int {
	if value < RSSItemLimitMin {
		return RSSItemLimitMin
	}
	if value > RSSItemLimitMax {
		return RSSItemLimitMax
	}
	return value
}

// xmlEscape escapes a value for XML.
//
// XML 1.0 forbids most control characters outright, and a parser
// rejects the entire document when it meets one — not just the element
// it appears in. A single post title carrying a stray form feed would
// take the whole feed down for every reader. The write path refuses
// these, but content/ is the source of truth and an author can edit a
// file by hand, so the feed has to survive that too. Tab, newline and
// carriage return are legal and kept.
func xmlEscape(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 0x20 && r != 0x7f) || r == '\t' || r == '\n' || r == '\r' {
			switch r {
			case '&':
				b.WriteString("&amp;")
			case '<':
				b.WriteString("&lt;")
			case '>':
				b.WriteString("&gt;")
			case '"':
				b.WriteString("&quot;")
			case '\'':
				b.WriteString("&apos;")
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
