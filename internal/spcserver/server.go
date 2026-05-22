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

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
	"github.com/sysop/ultrabridge/internal/spcserver/dedup"
	"github.com/sysop/ultrabridge/internal/spcserver/groups"
	"github.com/sysop/ultrabridge/internal/spcserver/handlers"
	"github.com/sysop/ultrabridge/internal/spcserver/login"
	"github.com/sysop/ultrabridge/internal/spcserver/socketio"
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
	DB *sql.DB
	// JWTSecret signs/verifies device tokens (Constant.SECRET by default).
	JWTSecret string
	// DeviceAccount/DevicePassword are the expected terminal-login credentials;
	// DeviceAccount "" accepts any account. DevicePassword is the raw password.
	DeviceAccount  string
	DevicePassword string
	// TaskStore is the CalDAV task store the schedule handlers map to/from.
	TaskStore handlers.TaskStore
	// CollectionName titles the single synthesized task group (the CalDAV
	// collection name).
	CollectionName string
	Logger         *slog.Logger
}

// Server is the SPC HTTP + Engine.IO server, both served on one listener. It is
// constructed only when Mode == "server"; in "client" mode main.go never calls New.
type Server struct {
	cfg Config
	mux *http.ServeMux
	reg *socketio.Registry
}

// New builds the server, registering all routes on its mux.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux(), reg: socketio.NewRegistry()}
	s.registerRoutes()
	return s
}

// Handler exposes the mux for in-process tests (httptest) without binding a
// socket.
func (s *Server) Handler() http.Handler { return s.mux }

// SocketRegistry returns the Engine.IO connection registry so other subsystems
// (e.g. the 1d STARTSYNC notifier) can push events to connected devices.
func (s *Server) SocketRegistry() *socketio.Registry { return s.reg }

// registerRoutes wires the device-facing endpoints. Go 1.22 method+path
// patterns match the routing style already used in cmd/ultrabridge/main.go.
// Login/challenge/boot routes are unauthenticated (the device has no token yet);
// business endpoints are wrapped with auth.Middleware.
func (s *Server) registerRoutes() {
	store := settingStore{db: s.cfg.DB}
	lh := &handlers.LoginHandler{
		DeviceAccount:  s.cfg.DeviceAccount,
		DevicePassword: s.cfg.DevicePassword,
		JWTSecret:      s.cfg.JWTSecret,
		Codes:          login.NewStore(),
		Store:          store,
	}

	// Equipment status (1a) — unauthenticated stub the device polls.
	s.mux.HandleFunc("POST /api/equipment/bind/status", handlers.BindStatus)

	// Login + challenge + boot handshake — all unauthenticated.
	s.mux.HandleFunc("POST /api/official/user/query/random/code", lh.RandomCode)
	s.mux.HandleFunc("POST /api/official/user/check/exists/server", lh.CheckExistsServer)
	s.mux.HandleFunc("POST /api/official/user/account/login/equipment", lh.Login)
	s.mux.HandleFunc("POST /api/official/user/account/login/new", lh.Login)
	s.mux.HandleFunc("POST /api/user/query/token", lh.QueryToken)
	s.mux.HandleFunc("POST /api/user/logout", lh.Logout)
	s.mux.HandleFunc("POST /api/terminal/user/bindEquipment", lh.BindEquipment)
	s.mux.HandleFunc("POST /api/terminal/equipment/unlink", lh.Unlink)
	s.mux.HandleFunc("GET /api/file/query/server", lh.FileQueryServer)

	// Protected probe — requires a valid x-access-token (1b).
	protect := func(fn http.HandlerFunc) http.Handler {
		return auth.Middleware(s.cfg.JWTSecret, store, fn)
	}
	s.mux.Handle("POST /api/user/query", protect(handlers.UserQuery))

	// Schedule: groups, tasks, sort, summary stubs (1d) — all JWT-protected.
	sched := &handlers.ScheduleHandler{
		Store:  s.cfg.TaskStore,
		Groups: groups.NewSingle(s.cfg.CollectionName),
		Dedup:  dedup.NewChecker(),
	}
	s.mux.Handle("POST /api/file/schedule/group/all", protect(sched.GroupAll))
	s.mux.Handle("POST /api/file/schedule/group", protect(sched.GroupNoOp))
	s.mux.Handle("PUT /api/file/schedule/group", protect(sched.GroupNoOp))
	s.mux.Handle("DELETE /api/file/schedule/group/{taskListId}", protect(sched.GroupNoOp))
	s.mux.Handle("POST /api/file/schedule/group/clear", protect(sched.GroupNoOp))
	s.mux.Handle("GET /api/file/schedule/group/{taskListId}", protect(sched.GroupNoOp))
	s.mux.Handle("POST /api/file/schedule/task/all", protect(sched.TaskAll))
	s.mux.Handle("POST /api/file/schedule/task", protect(sched.TaskCreate))
	s.mux.Handle("PUT /api/file/schedule/task", protect(sched.TaskUpdate))
	s.mux.Handle("PUT /api/file/schedule/task/list", protect(sched.TaskListUpdate))
	s.mux.Handle("DELETE /api/file/schedule/task/{taskId}", protect(sched.TaskDelete))
	s.mux.Handle("GET /api/file/schedule/task/{taskId}", protect(sched.TaskGet))
	s.mux.Handle("POST /api/file/schedule/sort", protect(sched.SortNoOp))
	s.mux.Handle("PUT /api/file/schedule/sort", protect(sched.SortNoOp))
	s.mux.Handle("DELETE /api/file/schedule/sort/{taskListId}", protect(sched.SortNoOp))
	s.mux.Handle("POST /api/file/query/schedule/sort", protect(sched.QuerySort))
	s.mux.Handle("POST /api/file/query/summary/hash", protect(sched.SummaryStub))
	s.mux.Handle("POST /api/file/query/summary/group", protect(sched.SummaryStub))
	s.mux.Handle("POST /api/file/query/summary/id", protect(sched.SummaryStub))

	// Engine.IO v3 websocket on the same listener (1c). The device connects to
	// /socket.io/ directly over websocket; demux is by path.
	s.mux.Handle("/socket.io/", socketio.NewHandler(s.cfg.JWTSecret, s.reg, s.cfg.Logger))
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
