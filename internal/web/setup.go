package web

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"

	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/notedb"
)

// SetupMiddleware redirects all requests to /setup when no credentials exist,
// except for the setup endpoints themselves and static assets.
// Uses an atomic flag to cache setup state — once credentials are saved,
// subsequent requests skip the DB query entirely.
func SetupMiddleware(db *sql.DB, next http.Handler) http.Handler {
	var setupDone atomic.Bool

	// Check once at startup
	if !appconfig.IsSetupRequired(context.Background(), db) {
		setupDone.Store(true)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow setup page and setup save endpoint through
		if r.URL.Path == "/setup" || r.URL.Path == "/setup/save" {
			next.ServeHTTP(w, r)
			return
		}

		// Fast path: setup already complete
		if setupDone.Load() {
			next.ServeHTTP(w, r)
			return
		}

		// Slow path: check DB (only during setup mode)
		if appconfig.IsSetupRequired(r.Context(), db) {
			http.Redirect(w, r, "/setup", http.StatusTemporaryRedirect)
			return
		}

		// Setup just completed (credentials saved via /setup/save)
		setupDone.Store(true)
		next.ServeHTTP(w, r)
	})
}

// handleSetup renders the credential setup page.
// Only accessible when IsSetupRequired is true.
func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	if h.noteDB == nil || !appconfig.IsSetupRequired(r.Context(), h.noteDB) {
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	// Keep the shared template unexecuted so normal pages can still clone it
	// after the administrator completes setup.
	t, err := h.tmpl.Clone()
	if err != nil {
		h.logger.Error("failed to clone setup template", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, "setup.html", nil); err != nil {
		h.logger.Error("failed to render setup template", "error", err)
		http.Error(w, "failed to render page", http.StatusInternalServerError)
	}
}

// handleSetupSave processes the setup form: validates, hashes password, saves credentials.
func (h *Handler) handleSetupSave(w http.ResponseWriter, r *http.Request) {
	if h.noteDB == nil {
		http.Error(w, "Database not available", http.StatusInternalServerError)
		return
	}

	if !appconfig.IsSetupRequired(r.Context(), h.noteDB) {
		http.Error(w, "Setup already complete", http.StatusForbidden)
		return
	}

	// Parse form
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")

	// Validate: both non-empty and match
	if username == "" {
		http.Error(w, "Username is required", http.StatusBadRequest)
		return
	}

	if password == "" {
		http.Error(w, "Password is required", http.StatusBadRequest)
		return
	}

	if len(password) < 8 {
		http.Error(w, "Password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	if password != confirm {
		http.Error(w, "Passwords do not match", http.StatusBadRequest)
		return
	}

	// Hash password with bcrypt
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		h.logger.Error("failed to hash password", "error", err)
		http.Error(w, "Failed to process password", http.StatusInternalServerError)
		return
	}

	// Save credentials to DB
	if err := notedb.SetSetting(r.Context(), h.noteDB, appconfig.KeyUsername, username); err != nil {
		h.logger.Error("failed to save username", "error", err)
		http.Error(w, "Failed to save credentials", http.StatusInternalServerError)
		return
	}

	if err := notedb.SetSetting(r.Context(), h.noteDB, appconfig.KeyPasswordHash, string(hash)); err != nil {
		h.logger.Error("failed to save password hash", "error", err)
		http.Error(w, "Failed to save credentials", http.StatusInternalServerError)
		return
	}

	// Redirect to home (now auth is enforced)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
