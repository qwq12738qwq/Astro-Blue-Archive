package api

// Administrator management.
//
// The CMS keeps its own accounts: a username, an argon2id hash, a role and a
// live/disabled state. A GitHub account is a *separate* kind of fact, bound through
// github_handlers.go, and binding one grants no permission here — this file never
// reads a GitHub identity.
//
// Two roles exist, and only two: owner and admin. An owner may create, disable and
// reset other administrators and manage any binding; an admin may change its own
// password and its own bindings. There is no permission table, because a third role
// would be a design for a problem this does not have.
//
// Every route below is behind requireSession, so the session, the Origin check and
// the CSRF double-submit header all apply, and the *authorisation* check is a second
// layer inside the handler: hiding a button is not a permission model, and the API
// is reachable without the button.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"blogcms/internal/auth"
	"blogcms/internal/httpx"
)

// adminSummary is what a caller may see of an account. What is absent matters as
// much as what is present: no password hash, no session token, no CSRF secret. A
// response that echoed the hash would make a browser console a credential store.
type adminSummary struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	LastLoginAt string `json:"lastLoginAt,omitempty"`
	// GitHubAccounts is how many external accounts are bound. The bindings
	// themselves are a different endpoint, because they are a different kind of
	// fact and a list of administrators must not become a list of identities.
	GitHubAccounts int `json:"githubAccounts"`
}

type adminListResponse struct {
	Admins []adminSummary `json:"admins"`
}

type adminDetailResponse struct {
	Admin adminSummary `json:"admin"`
}

type adminResponse struct {
	Admin adminSummary `json:"admin"`
}

// meResponse is the current account, which every admin may read about itself.
type meResponse struct {
	Admin adminSummary `json:"admin"`
}

type createAdminRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// setStatusRequest carries the target state, not a toggle: a button that said
// "disable" on a disabled account would be a control whose label is two steps
// behind its effect.
type setStatusRequest struct {
	Status string `json:"status"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type resetPasswordRequest struct {
	NewPassword string `json:"newPassword"`
}

func registerAdminRoutes(mux *http.ServeMux, d Deps) {
	// Any authenticated administrator: itself.
	mux.HandleFunc("GET /api/v1/admin/me", d.requireSession(d.getMe))
	mux.HandleFunc("POST /api/v1/admin/me/password", d.requireSession(d.changeOwnPassword))
	// Reading one's own record is allowed; reading another's is checked in the
	// handler, which can tell "self" from "other".
	mux.HandleFunc("GET /api/v1/admin/admins/{id}", d.requireSession(d.getAdmin))

	// Owner only. requireSession first, so the CSRF and Origin checks apply before
	// the role check can even be reached.
	mux.HandleFunc("GET /api/v1/admin/admins", d.requireSession(d.requireOwner(d.listAdmins)))
	mux.HandleFunc("POST /api/v1/admin/admins", d.requireSession(d.requireOwner(d.createAdmin)))
	mux.HandleFunc("POST /api/v1/admin/admins/{id}/status", d.requireSession(d.requireOwner(d.setAdminStatus)))
	mux.HandleFunc("POST /api/v1/admin/admins/{id}/password", d.requireSession(d.requireOwner(d.resetAdminPassword)))
}

// requireOwner refuses anything that is not an owner.
//
// It is a separate wrapper rather than an inline check so that "which routes are
// owner-only" is one grep rather than a reading of seven handlers.
func (d Deps) requireOwner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := SessionFrom(r)
		if session == nil || session.Role != auth.RoleOwner {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
				"the owner role is required for this action"))
			return
		}
		next(w, r)
	}
}

// pathID reads the {id} wildcard as an administrator id.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"id must be a positive integer"))
		return 0, false
	}
	return id, true
}

func toAdminSummary(admin auth.Admin, githubAccounts int) adminSummary {
	return adminSummary{
		ID:             admin.ID,
		Username:       admin.Username,
		Role:           admin.Role,
		Status:         admin.Status,
		CreatedAt:      admin.CreatedAt,
		UpdatedAt:      admin.UpdatedAt,
		LastLoginAt:    admin.LastLoginAt,
		GitHubAccounts: githubAccounts,
	}
}

func (d Deps) getMe(w http.ResponseWriter, r *http.Request) {
	session := SessionFrom(r)
	admin, err := d.Admins.Get(r.Context(), session.AdminID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	count, err := d.DB.CountGitHubIdentitiesByAdmin(r.Context(), admin.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, meResponse{Admin: toAdminSummary(*admin, count)})
}

func (d Deps) listAdmins(w http.ResponseWriter, r *http.Request) {
	admins, err := d.Admins.List(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]adminSummary, 0, len(admins))
	for _, admin := range admins {
		count, err := d.DB.CountGitHubIdentitiesByAdmin(r.Context(), admin.ID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		out = append(out, toAdminSummary(admin, count))
	}
	httpx.WriteJSON(w, http.StatusOK, adminListResponse{Admins: out})
}

func (d Deps) getAdmin(w http.ResponseWriter, r *http.Request) {
	session := SessionFrom(r)
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// An admin may read its own record; only an owner may read another's.
	if id != session.AdminID && session.Role != auth.RoleOwner {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
			"the owner role is required to view another administrator"))
		return
	}
	admin, err := d.Admins.Get(r.Context(), id)
	if err != nil {
		adminError(w, r, err)
		return
	}
	count, err := d.DB.CountGitHubIdentitiesByAdmin(r.Context(), admin.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, adminDetailResponse{Admin: toAdminSummary(*admin, count)})
}

func (d Deps) createAdmin(w http.ResponseWriter, r *http.Request) {
	var req createAdminRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Role = strings.TrimSpace(req.Role)
	if req.Role == "" {
		req.Role = auth.RoleAdmin
	}
	if fields := validateNewAdmin(req); len(fields) > 0 {
		httpx.WriteError(w, r, httpx.ValidationError(fields))
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusUnprocessableEntity, httpx.CodeValidation, "%s", err))
		return
	}
	admin, err := d.Admins.Create(r.Context(), req.Username, hash, req.Role)
	if err != nil {
		if errors.Is(err, auth.ErrDuplicateUsername) {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "admin_username_taken",
				"an administrator with that username already exists"))
			return
		}
		httpx.WriteError(w, r, err)
		return
	}
	d.auditAs(r, "admin.created", admin.Username)

	httpx.WriteJSON(w, http.StatusCreated, adminResponse{Admin: toAdminSummary(*admin, 0)})
}

func (d Deps) setAdminStatus(w http.ResponseWriter, r *http.Request) {
	session := SessionFrom(r)
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	var req setStatusRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if req.Status != auth.StatusActive && req.Status != auth.StatusDisabled {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{"status": "must be active or disabled"}))
		return
	}

	target, err := d.Admins.Get(r.Context(), id)
	if err != nil {
		adminError(w, r, err)
		return
	}
	if target.ID == session.AdminID {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "admin_status_self",
			"you cannot change your own account status"))
		return
	}
	// The lockout guards, in the order that matters. Both are conflicts rather
	// than errors: the request is well-formed and refused by a rule, and the
	// screen can say so.
	if req.Status == auth.StatusDisabled {
		active, err := d.Admins.CountActive(r.Context())
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if active <= 1 {
			httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "admin_last_active",
				"this is the only administrator that can still sign in; enabling another is the way to retire this one"))
			return
		}
		if target.Role == auth.RoleOwner {
			owners, err := d.Admins.CountActiveOwners(r.Context())
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if owners <= 1 {
				httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "admin_last_owner",
					"this is the last owner; administrator management would be permanently unavailable"))
				return
			}
		}
	}

	if err := d.Admins.SetStatus(r.Context(), target.ID, req.Status); err != nil {
		adminError(w, r, err)
		return
	}
	if req.Status == auth.StatusDisabled {
		// A disabled account's sessions stop resolving, so they are removed here
		// rather than left to be rejected one request at a time. The account, its
		// history, its bindings and its audit events are all left alone.
		if err := d.Auth.DestroySessionsForAdmin(r.Context(), target.ID, ""); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	refreshed, err := d.Admins.Get(r.Context(), target.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if req.Status == auth.StatusDisabled {
		d.auditAs(r, "admin.disabled", refreshed.Username)
	} else {
		d.auditAs(r, "admin.enabled", refreshed.Username)
	}

	count, err := d.DB.CountGitHubIdentitiesByAdmin(r.Context(), refreshed.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, adminResponse{Admin: toAdminSummary(*refreshed, count)})
}

func (d Deps) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	session := SessionFrom(r)

	var req changePasswordRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if req.NewPassword == "" {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{"newPassword": "required"}))
		return
	}

	admin, err := d.Admins.Get(r.Context(), session.AdminID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := auth.VerifyPassword(req.CurrentPassword, admin.PasswordHash); err != nil {
		// A field error rather than a generic message: the caller is the account's
		// owner changing their own password, so telling them the current one is
		// wrong discloses nothing to anyone else.
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{"currentPassword": "the current password is not correct"}))
		return
	}
	if n := utf8.RuneCountInString(req.NewPassword); n < minPasswordLen || n > maxPasswordLen {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
			"newPassword": "must be at least 8 characters and at most 200",
		}))
		return
	}
	if req.NewPassword == req.CurrentPassword {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
			"newPassword": "the new password must be different from the current one",
		}))
		return
	}

	if err := d.rotatePassword(r, admin, req.NewPassword, session.ID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.auditAs(r, "admin.password_changed", admin.Username)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (d Deps) resetAdminPassword(w http.ResponseWriter, r *http.Request) {
	session := SessionFrom(r)
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	var req resetPasswordRequest
	if err := httpx.DecodeJSON(w, r, d.Cfg.MaxJSONBody, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if n := utf8.RuneCountInString(req.NewPassword); n < minPasswordLen || n > maxPasswordLen {
		httpx.WriteError(w, r, httpx.ValidationError(map[string]string{
			"newPassword": "must be at least 8 characters and at most 200",
		}))
		return
	}

	target, err := d.Admins.Get(r.Context(), id)
	if err != nil {
		adminError(w, r, err)
		return
	}
	if target.ID == session.AdminID {
		// Resetting your own password is changing your own password, which has to
		// prove the current one. Routing it here would be a way around that.
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "admin_password_self",
			"use the change-password form for your own account"))
		return
	}

	if err := d.rotatePassword(r, target, req.NewPassword, ""); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d.auditAs(r, "admin.password_reset", target.Username)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// rotatePassword hashes, stores and revokes.
//
// The plaintext password arrives in the request body and is used exactly once, to
// produce an argon2id hash: it is not returned, not logged and not written to the
// audit log. Revoking the account's sessions is the other half — a password change
// that left an old cookie working would only be a change of secret, not a change of
// access.
//
// `keepSessionID` is the session the change was made from. It is empty for a reset
// of somebody else's password, which ends every session of that account including
// anything an attacker was using.
func (d Deps) rotatePassword(r *http.Request, admin *auth.Admin, newPassword, keepSessionID string) error {
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return httpx.Errorf(http.StatusUnprocessableEntity, httpx.CodeValidation, "%s", err)
	}
	if err := d.Admins.UpdatePassword(r.Context(), admin.ID, hash); err != nil {
		return err
	}
	return d.Auth.DestroySessionsForAdmin(r.Context(), admin.ID, keepSessionID)
}

// validateNewAdmin applies the account rules shared with the setup form, plus the
// role grammar.
func validateNewAdmin(req createAdminRequest) map[string]string {
	fields := map[string]string{}

	switch {
	case req.Username == "":
		fields["username"] = "required"
	case len(req.Username) > maxUsernameLen:
		fields["username"] = "too long"
	case !validUsername(req.Username):
		fields["username"] = "use 3-64 characters: letters, digits, dot, dash, underscore"
	}

	switch n := utf8.RuneCountInString(req.Password); {
	case n < minPasswordLen:
		fields["password"] = "must be at least 8 characters"
	case n > maxPasswordLen:
		fields["password"] = "must be at most 200 characters"
	}

	if req.Role != auth.RoleAdmin && req.Role != auth.RoleOwner {
		fields["role"] = "must be owner or admin"
	}
	return fields
}

// adminError maps administrator failures onto the uniform envelope.
func adminError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrNotFound):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, httpx.CodeNotFound,
			"no such administrator"))
	case errors.Is(err, auth.ErrDuplicateUsername):
		httpx.WriteError(w, r, httpx.Errorf(http.StatusConflict, "admin_username_taken",
			"an administrator with that username already exists"))
	default:
		httpx.WriteError(w, r, err)
	}
}
