// Package spcserver is the device-facing reimplementation of the Supernote
// Private Cloud (SPC) protocol, letting an unmodified Supernote device talk to
// UltraBridge as if it were the real SPC server. It owns the HTTP listener and
// (from 1c) the Engine.IO server, wiring the handlers/auth/socketio subpackages
// onto a single mux. See internal/spcserver/CLAUDE.md and docs/spc-protocol.md.
package spcserver

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
	"github.com/sysop/ultrabridge/internal/spcserver/capacity"
	"github.com/sysop/ultrabridge/internal/spcserver/dedup"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/groups"
	"github.com/sysop/ultrabridge/internal/spcserver/handlers"
	"github.com/sysop/ultrabridge/internal/spcserver/login"
	"github.com/sysop/ultrabridge/internal/spcserver/notify"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
	"github.com/sysop/ultrabridge/internal/spcserver/socketio"
	"github.com/sysop/ultrabridge/internal/spcserver/staging"
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
	// FileRoot is the dedicated storage root the device browses (Phase 2). Empty
	// disables file listing. QuotaBytes is the fake total capacity reported.
	FileRoot   string
	QuotaBytes int64
	// OssSecret signs/verifies the presigned download URLs UB issues to itself
	// (Phase 3). Auto-generated and persisted on first boot (see
	// appconfig.EnsureSPCOssSecret); the device treats these URLs as opaque.
	OssSecret string
	// UploadEnqueuer (optional, Phase 4d) kicks the OCR pipeline for uploaded
	// .note/.pdf files; OCRWatchDir restricts the kick to that directory (the
	// Supernote source's NotesPath). Both nil/empty ⇒ no OCR kick.
	UploadEnqueuer handlers.Enqueuer
	OCRWatchDir    string
	// DigestStore (optional, Phase D) is the canonical digest ("summary") store
	// the device syncs digests to/from. Nil ⇒ the summary query endpoints fall
	// back to empty-success stubs and the write endpoints stay 404 (pre-Phase-D).
	DigestStore DigestStore
	// DigestIndexer (optional, Phase D2) surfaces synced digests in UB's shared
	// FTS5/RAG index. Nil ⇒ digests still round-trip to the device but are not
	// searchable in UB.
	DigestIndexer DigestIndexer
	// ContentIndex/EmbedIndex (optional) keep the FTS index (search.Store) and
	// RAG embeddings (*rag.Store) in step with device file mutations: delete →
	// drop, move → repoint, copy → duplicate. Nil ⇒ no index upkeep. EmbedIndex
	// must be left nil (not a typed-nil *rag.Store) when embedding is disabled.
	ContentIndex IndexStore
	EmbedIndex   IndexStore
	// FileRecords (optional) repoints the notes/jobs inventory on move so the
	// Files tab + job history track the new path. *notestore.Store satisfies it.
	FileRecords FileMover
	// DigestDeliverer (optional, Phase D2) drains durable digest tombstones to the
	// device on its heartbeat and clears them on ack. *notify.TombstoneQueue
	// satisfies it; nil disables digest-delete propagation.
	DigestDeliverer DigestDeliverer
	Logger          *slog.Logger
}

// IndexStore and FileMover are aliased from handlers so main can hold
// interface-typed values (search.Store, *rag.Store, *notestore.Store) without
// importing handlers directly.
type IndexStore = handlers.IndexStore
type FileMover = handlers.FileMover

// DigestDeliverer is the digest-tombstone delivery seam wired into the socket
// handler (Phase D2). Aliased from socketio so main can hold the value without
// importing socketio directly.
type DigestDeliverer = socketio.DigestQueue

// DigestStore is the digest store the SPC server needs (Phase D). Aliased from
// the handlers package so main can hold an interface-typed value (and a true nil
// when digest migration fails) without importing handlers directly.
type DigestStore = handlers.DigestStore

// DigestIndexer surfaces digests in UB's search/RAG index (Phase D2). Aliased
// from handlers so main can pass a *digestindex.Bridge without spcserver
// importing digestindex.
type DigestIndexer = handlers.DigestIndexer

// UploadEnqueuerFunc adapts a plain func to handlers.Enqueuer (the processor's
// own Enqueue is variadic, so it can't satisfy the interface directly). The
// method is nil-safe: a nil func value assigned to the interface field is a
// no-op rather than a panic (the typed-nil-interface gotcha when no Supernote
// source is configured).
type UploadEnqueuerFunc func(ctx context.Context, path string) error

// Enqueue implements handlers.Enqueuer.
func (f UploadEnqueuerFunc) Enqueue(ctx context.Context, path string) error {
	if f == nil {
		return nil
	}
	return f(ctx, path)
}

// Server is the SPC HTTP + Engine.IO server, both served on one listener. It is
// constructed only when Mode == "server"; in "client" mode main.go never calls New.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	reg     *socketio.Registry
	staging *staging.Store // upload staging area; nil when FileRoot is empty
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
	s.mux.HandleFunc("GET /api/official/user/account/login/equipment", lh.LoginProbe)
	s.mux.HandleFunc("GET /api/official/user/account/login/new", lh.LoginProbe)
	s.mux.HandleFunc("POST /api/official/system/base/param", handlers.SystemBaseParam)
	s.mux.HandleFunc("GET /api/query/email/publickey", handlers.EmailPublicKey)
	s.mux.HandleFunc("POST /api/user/query/token", lh.QueryToken)
	s.mux.HandleFunc("POST /api/user/logout", lh.Logout)
	s.mux.HandleFunc("POST /api/terminal/user/bindEquipment", lh.BindEquipment)
	s.mux.HandleFunc("POST /api/terminal/equipment/unlink", lh.Unlink)
	s.mux.HandleFunc("GET /api/file/query/server", lh.FileQueryServer)
	s.mux.HandleFunc("POST /api/file/query/server", lh.FileQueryServer)

	// Protected probe — requires a valid x-access-token (1b).
	protect := func(fn http.HandlerFunc) http.Handler {
		return auth.Middleware(s.cfg.JWTSecret, store, fn)
	}
	s.mux.Handle("POST /api/user/query", protect(handlers.UserQuery))
	s.mux.Handle("GET /api/query/email/config", protect(handlers.EmailConfig))

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

	// File listing + capacity (Phase 2) — read path, all JWT-protected. Reads the
	// filesystem under FileRoot directly; the registry/meter are constructed once
	// here. An empty FileRoot leaves the handlers inert (empty/zero responses).
	reg := fileids.New(s.cfg.DB, s.cfg.FileRoot)
	files := &handlers.FileHandler{
		Root:   s.cfg.FileRoot,
		Reg:    reg,
		Meter:  capacity.New(s.cfg.FileRoot, s.cfg.QuotaBytes),
		Logger: s.cfg.Logger,
	}
	webFiles := &handlers.WebFileHandler{
		Root:   s.cfg.FileRoot,
		Reg:    reg,
		Logger: s.cfg.Logger,
	}
	s.mux.Handle("POST /api/file/2/files/synchronous/start", protect(files.SynchronousStart))
	s.mux.Handle("POST /api/file/2/files/synchronous/end", protect(files.SynchronousEnd))
	s.mux.Handle("POST /api/file/2/files/list_folder", protect(files.ListFolder))
	s.mux.Handle("POST /api/file/3/files/list_folder_v3", protect(files.ListFolderV3))
	s.mux.Handle("POST /api/file/3/files/list", protect(webFiles.ListByPath))
	s.mux.Handle("POST /api/file/3/files/query_v3", protect(files.QueryByID))
	s.mux.Handle("POST /api/file/3/files/query/by/path_v3", protect(files.QueryByPath))
	s.mux.Handle("POST /api/file/capacity/query", protect(files.CapacityQuery))
	s.mux.Handle("POST /api/file/2/users/get_space_usage", protect(files.GetSpaceUsage))
	s.mux.Handle("POST /api/file/2/files/create_folder_v2", protect(files.CreateFolderV2))
	s.mux.Handle("POST /api/file/2/files/query/deleteApi", protect(files.QueryByIDDeleteAPI))
	s.mux.Handle("POST /api/file/list/query", protect(webFiles.ListQuery))
	s.mux.Handle("POST /api/file/path/query", protect(webFiles.PathQuery))
	s.mux.Handle("POST /api/file/folder/list/query", protect(webFiles.FolderListQuery))
	s.mux.Handle("POST /api/file/list/search", protect(webFiles.Search))
	s.mux.Handle("POST /api/file/label/list/search", protect(webFiles.Search))
	s.mux.Handle("POST /api/file/folder/add", protect(webFiles.FolderAdd))

	// File download (Phase 3) — read path, byte transfer. download_v3 and
	// generate/download/url mint presigned URLs (JWT-protected business calls);
	// GET /api/oss/download streams the bytes and is authenticated by the
	// query-string signature alone (NOT JWT — the device fetches it opaquely).
	dl := &handlers.DownloadHandler{
		Root:   s.cfg.FileRoot,
		Reg:    reg,
		Signer: &oss.Signer{Secret: s.cfg.OssSecret},
		Logger: s.cfg.Logger,
	}
	s.mux.Handle("POST /api/file/3/files/download_v3", protect(dl.DownloadV3))
	s.mux.Handle("POST /api/oss/generate/download/url", protect(dl.GenerateDownloadURL))
	s.mux.Handle("POST /api/file/download/url", protect(dl.WebDownloadURL))
	s.mux.HandleFunc("GET /api/oss/download", dl.DownloadStream)

	// File upload (Phase 4) — write path. apply/finish are JWT-protected business
	// calls; POST /api/oss/upload sinks the bytes and is authenticated by the
	// query-string signature alone (NOT JWT — the device POSTs opaquely, like the
	// download GET). The staging store is kept on s for Run's orphan sweeper.
	// FileNotifier nudges the device to re-pull files after a finish (best-effort).
	if s.cfg.FileRoot != "" {
		s.staging = &staging.Store{Root: s.cfg.FileRoot, DB: s.cfg.DB}
	}
	fileNotifier := notify.NewSocketNotifier(
		s.reg,
		func(ctx context.Context) (string, error) { return store.Get(ctx, auth.UserIDSettingKey) },
		s.cfg.Logger,
	)
	up := &handlers.UploadHandler{
		Root:        s.cfg.FileRoot,
		Reg:         reg,
		Signer:      &oss.Signer{Secret: s.cfg.OssSecret},
		Staging:     s.staging,
		Notifier:    fileNotifier,
		Enqueuer:    s.cfg.UploadEnqueuer,
		OCRWatchDir: s.cfg.OCRWatchDir,
		Logger:      s.cfg.Logger,
	}
	s.mux.Handle("POST /api/file/3/files/upload/apply", protect(up.Apply))
	s.mux.Handle("POST /api/file/upload/apply", protect(up.WebApply))
	s.mux.Handle("POST /api/file/2/files/upload/finish", protect(up.Finish))
	s.mux.Handle("POST /api/file/3/files/upload/confirm", protect(up.FinishConfirm))
	s.mux.Handle("POST /api/file/upload/finish", protect(up.WebFinish))
	s.mux.HandleFunc("POST /api/oss/upload", up.UploadStream)

	// File mutations (Phase 4c) — delete (soft, to .recycle/), move, copy. All
	// JWT-protected business calls operating on the shared registry + FileRoot.
	mut := &handlers.MutationHandler{
		Root:         s.cfg.FileRoot,
		Reg:          reg,
		Notifier:     fileNotifier,
		ContentIndex: s.cfg.ContentIndex,
		EmbedIndex:   s.cfg.EmbedIndex,
		FileRecords:  s.cfg.FileRecords,
		Logger:       s.cfg.Logger,
	}
	s.mux.Handle("POST /api/file/3/files/delete_folder_v3", protect(mut.DeleteFolder))
	s.mux.Handle("POST /api/file/3/files/move_v3", protect(mut.Move))
	s.mux.Handle("POST /api/file/3/files/copy_v3", protect(mut.Copy))
	s.mux.Handle("POST /api/file/move", protect(mut.WebMove))
	s.mux.Handle("POST /api/file/rename", protect(mut.WebRename))
	s.mux.Handle("POST /api/file/copy", protect(mut.WebCopy))
	s.mux.Handle("POST /api/file/delete", protect(mut.WebDelete))
	s.mux.Handle("POST /api/file/recycle/list/query", protect(mut.RecycleList))
	s.mux.Handle("POST /api/file/recycle/revert", protect(mut.RecycleRevert))
	s.mux.Handle("POST /api/file/recycle/delete", protect(mut.RecycleDelete))
	s.mux.Handle("POST /api/file/recycle/clear", protect(mut.RecycleClear))

	conv := &handlers.ConvertHandler{
		Root:   s.cfg.FileRoot,
		Reg:    reg,
		Signer: &oss.Signer{Secret: s.cfg.OssSecret},
		Logger: s.cfg.Logger,
	}
	s.mux.Handle("POST /api/file/note/to/png", protect(conv.NoteToPNG))
	s.mux.Handle("POST /api/file/note/to/pdf", protect(conv.NoteToPDF))
	s.mux.Handle("POST /api/file/pdfwithmark/to/pdf", protect(conv.PDFWithMarkToPDF))

	// Digests / "summary" sync (Phase D) — the device-facing F_SummaryController.
	// All JWT-protected (digests ride the data-sync channel alongside tasks). The
	// .mark handwriting blobs reuse the shared OSS signer + staging area. When no
	// DigestStore is wired we fall back to the empty-success query stubs so task
	// sync stays unblocked (the device hits query/summary/* every sync) and the
	// write endpoints stay 404 — exactly the pre-Phase-D behavior.
	if s.cfg.DigestStore != nil {
		sum := &handlers.SummaryHandler{
			Store:   s.cfg.DigestStore,
			Root:    s.cfg.FileRoot,
			Signer:  &oss.Signer{Secret: s.cfg.OssSecret},
			Staging: s.staging,
			Indexer: s.cfg.DigestIndexer,
			Logger:  s.cfg.Logger,
		}
		s.mux.Handle("POST /api/file/add/summary", protect(sum.AddSummary))
		s.mux.Handle("PUT /api/file/update/summary", protect(sum.UpdateSummary))
		s.mux.Handle("DELETE /api/file/delete/summary", protect(sum.DeleteSummary))
		s.mux.Handle("POST /api/file/query/summary", protect(sum.QuerySummary))
		s.mux.Handle("POST /api/file/query/summary/hash", protect(sum.QuerySummaryHash))
		s.mux.Handle("POST /api/file/query/summary/id", protect(sum.QuerySummaryByID))
		s.mux.Handle("POST /api/file/add/summary/group", protect(sum.AddSummaryGroup))
		s.mux.Handle("PUT /api/file/update/summary/group", protect(sum.UpdateSummaryGroup))
		s.mux.Handle("DELETE /api/file/delete/summary/group", protect(sum.DeleteSummaryGroup))
		s.mux.Handle("POST /api/file/query/summary/group", protect(sum.QuerySummaryGroup))
		s.mux.Handle("POST /api/file/add/summary/tag", protect(sum.AddSummaryTag))
		s.mux.Handle("PUT /api/file/update/summary/tag", protect(sum.UpdateSummaryTag))
		s.mux.Handle("DELETE /api/file/delete/summary/tag", protect(sum.DeleteSummaryTag))
		s.mux.Handle("GET /api/file/query/summary/tag", protect(sum.QuerySummaryTag))
		s.mux.Handle("POST /api/file/upload/apply/summary", protect(sum.UploadApplySummary))
		s.mux.Handle("POST /api/file/download/summary", protect(sum.DownloadSummary))
	} else {
		s.mux.Handle("POST /api/file/query/summary/hash", protect(sched.SummaryStub))
		s.mux.Handle("POST /api/file/query/summary/group", protect(sched.SummaryStub))
		s.mux.Handle("POST /api/file/query/summary/id", protect(sched.SummaryStub))
	}

	// Engine.IO v3 websocket on the same listener (1c). The device connects to
	// /socket.io/ directly over websocket; demux is by path.
	sockHandler := socketio.NewHandler(s.cfg.JWTSecret, s.reg, s.cfg.Logger)
	sockHandler.SetUserIDResolver(func(ctx context.Context, verified string) string {
		return auth.CanonicalUserID(ctx, store, verified)
	})
	if s.cfg.DigestDeliverer != nil {
		sockHandler.SetDigestQueue(s.cfg.DigestDeliverer)
	}
	s.mux.Handle("/socket.io/", sockHandler)
}

// uploadSweepInterval is how often Run reclaims abandoned upload stages whose
// TTL has expired (applies that never finished).
const uploadSweepInterval = 10 * time.Minute

// Run binds the listener and serves until error. TLS is used when both cert and
// key are set; otherwise plain HTTP (TLS is typically terminated upstream by
// the reverse proxy in this deployment). It also starts the upload orphan
// sweeper (only when a staging area exists); the sweeper lives for the process,
// so it is started here rather than in New (which httptest also calls).
func (s *Server) Run() error {
	if s.staging != nil {
		go s.sweepUploads()
	}
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		return http.ListenAndServeTLS(s.cfg.ListenAddr, s.cfg.TLSCert, s.cfg.TLSKey, s.mux)
	}
	return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
}

// sweepUploads periodically reclaims expired upload stages.
func (s *Server) sweepUploads() {
	ticker := time.NewTicker(uploadSweepInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.staging.Sweep(context.Background()); err != nil && s.cfg.Logger != nil {
			s.cfg.Logger.Warn("spc upload stage sweep failed", "err", err)
		}
	}
}
