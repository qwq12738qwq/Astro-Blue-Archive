package api

import (
	"net/http"

	"blogcms/internal/httpx"
)

// CustomCodeRequest carries both custom-code files.
//
// ARCHITECTURE.md §9/§16/§17: content/system/custom.css and custom.js are the
// single source of truth. Go writes the files; Astro serves them as external
// resources at /custom.css and /custom.js.
type CustomCodeRequest struct {
	CSS string `json:"css"`
	JS  string `json:"js"`
}

func registerCustomCodeRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/admin/custom-code", d.requireSession(d.getCustomCode))
	mux.HandleFunc("PUT /api/v1/admin/custom-code", d.requireSession(d.putCustomCode))
}

func (d Deps) getCustomCode(w http.ResponseWriter, r *http.Request) {
	css, err := d.Content.ReadSystemFile("custom.css")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	js, err := d.Content.ReadSystemFile("custom.js")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, CustomCodeRequest{CSS: css, JS: js})
}

func (d Deps) putCustomCode(w http.ResponseWriter, r *http.Request) {
	var req CustomCodeRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Write CSS first: if the JS write then fails, the site keeps working with
	// valid CSS rather than being left half-updated.
	if err := d.Content.WriteSystemFile("custom.css", req.CSS); err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}
	if err := d.Content.WriteSystemFile("custom.js", req.JS); err != nil {
		httpx.WriteError(w, r, contentError(err))
		return
	}

	d.Audit(r.Context(), "custom_css.updated", "system/custom.css")
	d.Audit(r.Context(), "custom_js.updated", "system/custom.js")

	httpx.WriteJSON(w, http.StatusOK, CustomCodeRequest{CSS: req.CSS, JS: req.JS})
}
