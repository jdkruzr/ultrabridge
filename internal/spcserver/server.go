// Package spcserver is the device-facing reimplementation of the Supernote
// Private Cloud (SPC) protocol, letting an unmodified Supernote device talk to
// UltraBridge as if it were the real SPC server. It owns the HTTP listener and
// (from 1c) the Engine.IO server, wiring the handlers/auth/socketio subpackages
// onto a single mux. See internal/spcserver/CLAUDE.md and docs/spc-protocol.md.
package spcserver

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/sysop/ultrabridge/internal/spcserver/handlers"
)

// Config holds the SPC server's runtime configuration, populated from appconfig
// in cmd/ultrabridge/main.go.
type Config struct {
	Mode       string // "client" (no listener) | "server"
	ListenAddr string
	TLSCert    string
	TLSKey     string
	// DB is the shared notedb handle. Handlers persist/read SPC runtime state
	// (e.g. the harvested spc_user_id) through it via notedb.GetSetting/SetSetting.
	DB     *sql.DB
	Logger *slog.Logger
}

// Server is the SPC HTTP (and, from 1c, Engine.IO) server. It is constructed
// only when Mode == "server"; in "client" mode main.go never calls New.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New builds the server, registering all routes on its mux.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.registerRoutes()
	return s
}

// Handler exposes the mux for in-process tests (httptest) without binding a
// socket.
func (s *Server) Handler() http.Handler { return s.mux }

// registerRoutes wires the device-facing endpoints. Go 1.22 method+path
// patterns match the routing style already used in cmd/ultrabridge/main.go.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("POST /api/equipment/bind/status", handlers.BindStatus)
}

// Run binds the listener and serves until error. TLS is used when both cert and
// key are set; otherwise plain HTTP (TLS is typically terminated upstream by
// the reverse proxy in this deployment).
func (s *Server) Run() error {
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		return http.ListenAndServeTLS(s.cfg.ListenAddr, s.cfg.TLSCert, s.cfg.TLSKey, s.mux)
	}
	return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
}
