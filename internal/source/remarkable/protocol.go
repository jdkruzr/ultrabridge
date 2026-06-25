package remarkable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// The device treats its device token as long-lived: it uses the device
	// token to mint short-lived user tokens, so a short device TTL would log
	// the device out (here, ~30 min after pairing). Real reMarkable / rmfakecloud
	// device tokens effectively never expire.
	deviceTokenTTL = 10 * 365 * 24 * time.Hour
	userTokenTTL   = 12 * time.Hour
	urlTokenTTL    = 30 * time.Minute
	rootBlobID     = "root"
)

type protocol struct {
	cfg     Config
	store   *store
	logger  *slog.Logger
	hub     *hub
	indexer *metadataIndexer
	ocr     *ocrProcessor
}

func newProtocol(cfg Config, store *store, logger *slog.Logger, h *hub, indexer *metadataIndexer, ocr *ocrProcessor) *protocol {
	return &protocol{cfg: cfg, store: store, logger: logger, hub: h, indexer: indexer, ocr: ocr}
}

func (p *protocol) refreshMetadataIndex(ctx context.Context) {
	if p.indexer != nil {
		if err := p.indexer.indexAll(ctx); err != nil {
			p.logger.Warn("remarkable metadata indexing failed", "error", err)
		}
	}
	if p.ocr != nil {
		go func() {
			if err := p.ocr.EnqueueMissingStale(context.Background()); err != nil {
				p.logger.Warn("remarkable OCR enqueue failed", "error", err)
			}
		}()
	}
}

func (p *protocol) deleteMetadataIndex(ctx context.Context, id string) {
	if p.indexer != nil {
		if err := p.indexer.deleteDocument(ctx, id); err != nil {
			p.logger.Warn("remarkable metadata delete failed", "document_id", id, "error", err)
		}
	}
	if p.ocr != nil {
		if err := p.ocr.DeleteDocument(ctx, id); err != nil {
			p.logger.Warn("remarkable OCR delete failed", "document_id", id, "error", err)
		}
	}
}

// notifyUpgrader upgrades the device's notification handshake to a websocket.
// The reMarkable device drives the channel; origin checks don't apply.
var notifyUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func (p *protocol) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /discovery/v1/endpoints", p.handleDiscoveryEndpoints)
	mux.HandleFunc("GET /discovery/v1/webapp", p.handleDiscoveryWebapp)
	// NOTE: /health is intentionally NOT registered here. The main mux
	// (cmd/ultrabridge/main.go) already owns "GET /health" for the Docker
	// health check, and Go 1.22's ServeMux panics on a duplicate pattern. The
	// shared endpoint returns 200, which satisfies the device's liveness probe.
	mux.HandleFunc("POST /token/json/2/device/new", p.handleNewDevice)
	mux.HandleFunc("POST /token/json/2/user/new", p.handleNewUserToken)
	mux.HandleFunc("POST /token/json/3/user/new", p.handleNewUserToken)
	mux.HandleFunc("POST /token/json/2/device/delete", p.handleDeleteDevice)
	mux.HandleFunc("POST /token/json/3/device/delete", p.handleDeleteDevice)
	mux.HandleFunc("GET /service/json/1/{service}", p.handleLocateService)
	mux.HandleFunc("GET /settings/v1/beta", p.handleBetaSettings)
	mux.HandleFunc("POST /settings/v1/beta", p.handleBetaSettings)

	mux.HandleFunc("PUT /document-storage/json/2/upload/request", p.withUserAuth(p.handleUploadRequest))
	mux.HandleFunc("PUT /document-storage/json/2/upload/update-status", p.withUserAuth(p.handleUpdateStatus))
	mux.HandleFunc("PUT /document-storage/json/2/delete", p.withUserAuth(p.handleDelete))
	mux.HandleFunc("GET /document-storage/json/2/docs", p.withUserAuth(p.handleListDocuments))

	mux.HandleFunc("POST /api/v1/page", p.withUserAuth(p.handleHWR))
	mux.HandleFunc("POST /convert/v1/handwriting", p.withUserAuth(p.handleHWR))

	mux.HandleFunc("GET /search/v1/settings", p.withUserAuth(p.handleSearchSettings))
	mux.HandleFunc("GET /search/v1/delta", p.withUserAuth(p.handleSearchDelta))
	mux.HandleFunc("GET /search/v1/{docId}/{pageId}", p.withUserAuth(p.handleSearchIndex))
	mux.HandleFunc("POST /search/v1/error", p.withUserAuth(p.handleSearchError))

	mux.HandleFunc("POST /api/v1/signed-urls/downloads", p.withUserAuth(p.handleSignedBlobDownload))
	mux.HandleFunc("POST /api/v1/signed-urls/uploads", p.withUserAuth(p.handleSignedBlobUpload))
	mux.HandleFunc("POST /api/v1/sync-complete", p.withUserAuth(p.handleSyncComplete))
	mux.HandleFunc("POST /sync/v2/signed-urls/downloads", p.withUserAuth(p.handleSignedBlobDownload))
	mux.HandleFunc("POST /sync/v2/signed-urls/uploads", p.withUserAuth(p.handleSignedBlobUpload))
	mux.HandleFunc("POST /sync/v2/sync-complete", p.withUserAuth(p.handleSyncComplete))
	mux.HandleFunc("GET /sync/v3/root", p.withUserAuth(p.handleGetRootV3))
	mux.HandleFunc("PUT /sync/v3/root", p.withUserAuth(p.handlePutRootV3))
	mux.HandleFunc("GET /sync/v3/files/{file}", p.withUserAuth(p.handleGetBlobDirect))
	mux.HandleFunc("PUT /sync/v3/files/{file}", p.withUserAuth(p.handlePutBlobDirect))
	mux.HandleFunc("POST /sync/v3/check-files", p.withUserAuth(p.handleCheckFiles))
	mux.HandleFunc("GET /sync/v4/root", p.withUserAuth(p.handleGetRootV4))

	// Telemetry / integrations endpoints the device calls but UB has no use
	// for. If they aren't registered here they fall through to the top-level
	// catch-all (Basic/bearer auth) and return 401; the device reads that 401
	// as a *token* failure and refetches its user token in a tight loop, which
	// starves real document sync ("unable to sync document content"). rmfakecloud
	// stubs them the same way (nullReport / syncReports / empty integrations).
	mux.HandleFunc("POST /report/v1", p.handleNullReport)
	mux.HandleFunc("POST /v1/reports", p.handleNullReport)
	mux.HandleFunc("POST /v2/reports", p.handleNullReport)
	mux.HandleFunc("POST /sync/reports/v1", p.handleNullReport)
	mux.HandleFunc("POST /analytics/v2/events", p.handleNullReport)
	mux.HandleFunc("POST /post", p.handleNullReport)
	mux.HandleFunc("GET /mdm/devices/v0/instruction", p.handleMDMInstruction)
	mux.HandleFunc("GET /integrations/v2/instances", p.handleIntegrations)

	mux.HandleFunc("GET /notifications/ws/json/1", p.handleNotificationsWS)

	mux.HandleFunc("GET /storage/{token}", p.handleStorageGet)
	mux.HandleFunc("PUT /storage/{token}", p.handleStoragePut)
	mux.HandleFunc("GET /blobstorage/{token}", p.handleBlobGet)
	mux.HandleFunc("PUT /blobstorage/{token}", p.handleBlobPut)
}

type deviceTokenRequest struct {
	Code       string `json:"code"`
	DeviceDesc string `json:"deviceDesc"`
	DeviceID   string `json:"deviceID"`
}

type uploadRequest struct {
	ID      string `json:"ID"`
	Parent  string `json:"Parent"`
	Type    string `json:"Type"`
	Version int    `json:"Version"`
}

type uploadResponse struct {
	ID                string `json:"ID"`
	Message           string `json:"Mesasge"`
	Success           bool   `json:"Success"`
	BlobURLPut        string `json:"BlobURLPut"`
	BlobURLPutExpires string `json:"BlobURLPutExpires"`
	Version           int    `json:"Version"`
}

type signedBlobRequest struct {
	Method       string `json:"http_method"`
	Initial      bool   `json:"initial_sync"`
	RelativePath string `json:"relative_path"`
}

type signedBlobResponse struct {
	Expires        string `json:"expires"`
	Method         string `json:"method"`
	RelativePath   string `json:"relative_path"`
	URL            string `json:"url"`
	MaxRequestSize int64  `json:"maxuploadsize_bytes,omitempty"`
}

type syncRootRequest struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash,omitempty"`
	Broadcast  bool   `json:"broadcast"`
}

type syncRootResponse struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash,omitempty"`
}

type syncRootV4Response struct {
	Generation    int64  `json:"generation"`
	Hash          string `json:"hash,omitempty"`
	SchemaVersion int64  `json:"schemaVersion"`
}

type checkFilesRequest struct {
	Filename string   `json:"filename"`
	Files    []string `json:"files"`
	Reason   string   `json:"reason"`
}

type missingFilesResponse struct {
	MissingFiles []string `json:"missingFiles"`
}

func (p *protocol) handleDiscoveryEndpoints(w http.ResponseWriter, r *http.Request) {
	host := externalBaseURL(r)
	writeJSON(w, http.StatusOK, map[string]string{
		"notifications": host,
		"webapp":        host,
	})
}

func (p *protocol) handleDiscoveryWebapp(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"Host": externalBaseURL(r), "Status": "OK"})
}

func (p *protocol) handleNewDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Code) == "" || strings.TrimSpace(req.DeviceID) == "" {
		http.Error(w, "missing code or deviceID", http.StatusBadRequest)
		return
	}
	if p.cfg.PairingCode == "" || !strings.EqualFold(strings.TrimSpace(req.Code), strings.TrimSpace(p.cfg.PairingCode)) {
		http.Error(w, "invalid pairing code", http.StatusBadRequest)
		return
	}
	if err := p.store.touchDevice(r.Context(), req.DeviceID, req.DeviceDesc); err != nil {
		http.Error(w, "failed to register device", http.StatusInternalServerError)
		return
	}
	tok, err := p.store.issueToken(r.Context(), "device", req.DeviceID, req.DeviceDesc, "", deviceTokenTTL)
	if err != nil {
		http.Error(w, "failed to issue token", http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, tok)
}

func (p *protocol) handleNewUserToken(w http.ResponseWriter, r *http.Request) {
	deviceClaims, err := p.deviceClaims(r.Context(), readBearerToken(r))
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := p.store.touchDevice(r.Context(), deviceClaims.DeviceID, deviceClaims.DeviceDesc); err != nil {
		http.Error(w, "failed to update device", http.StatusInternalServerError)
		return
	}
	// The DB row (random token) is the validation anchor; the JWT we hand the
	// device carries that token id as its jti. See token.go.
	scopes := userScopesForConfig(p.cfg)
	jti, err := p.store.issueToken(r.Context(), "user", deviceClaims.DeviceID, deviceClaims.DeviceDesc, scopes, userTokenTTL)
	if err != nil {
		http.Error(w, "failed to issue token", http.StatusInternalServerError)
		return
	}
	tok, err := newUserJWT(jti, p.cfg.DeviceAccount, deviceClaims, scopes, userTokenTTL)
	if err != nil {
		http.Error(w, "failed to issue token", http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, tok)
}

func (p *protocol) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	if _, err := p.deviceClaims(r.Context(), readBearerToken(r)); err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *protocol) handleLocateService(w http.ResponseWriter, r *http.Request) {
	host := externalBaseURL(r)
	if r.PathValue("service") == "blob-storage" {
		host = externalBaseURL(r)
	}
	writeJSON(w, http.StatusOK, map[string]string{"Host": host, "Status": "OK"})
}

// handleNullReport accepts and discards device telemetry (crash/usage/sync
// reports). It is intentionally unauthenticated and always 200: the device
// must never see a 401 here (it would treat it as a token failure). Mirrors
// rmfakecloud's nullReport/syncReports.
func (p *protocol) handleNullReport(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

func (p *protocol) handleMDMInstruction(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleIntegrations returns an empty third-party integration list. UB has no
// integrations to advertise; the device is happy with an empty set ("received
// 0 integrations").
func (p *protocol) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"integrations": []any{}})
}

func (p *protocol) handleBetaSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		if strings.TrimSpace(string(body)) != "" {
			p.logger.Info("remarkable beta settings request", "body", string(body))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enrolled": false, "available": true})
}

func (p *protocol) handleUploadRequest(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	var req []uploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	resp := make([]uploadResponse, 0, len(req))
	for _, item := range req {
		token, err := p.store.issuePresigned(r.Context(), presignedTarget{
			Kind:       "document",
			Scope:      "write",
			DocumentID: item.ID,
		}, urlTokenTTL)
		if err != nil {
			http.Error(w, "failed to issue upload URL", http.StatusInternalServerError)
			return
		}
		resp = append(resp, uploadResponse{
			ID:                item.ID,
			Success:           true,
			Version:           item.Version,
			BlobURLPut:        externalBaseURL(r) + "/storage/" + url.PathEscape(token),
			BlobURLPutExpires: time.Now().Add(urlTokenTTL).UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, resp)
	_ = claims
}

func (p *protocol) handleUpdateStatus(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	var req []documentMeta
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	for _, meta := range req {
		if err := p.store.upsertMetadata(r.Context(), meta); err != nil {
			http.Error(w, "failed to store metadata", http.StatusInternalServerError)
			return
		}
	}
	p.refreshMetadataIndex(r.Context())
	type statusResponse struct {
		ID      string `json:"ID"`
		Message string `json:"Message"`
		Success bool   `json:"Success"`
		Version int    `json:"Version"`
	}
	resp := make([]statusResponse, 0, len(req))
	for _, meta := range req {
		resp = append(resp, statusResponse{ID: meta.ID, Success: true, Version: meta.Version})
	}
	writeJSON(w, http.StatusOK, resp)
	_ = claims
}

func (p *protocol) handleDelete(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	var req []struct {
		ID string `json:"ID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	type statusResponse struct {
		ID      string `json:"ID"`
		Message string `json:"Message"`
		Success bool   `json:"Success"`
	}
	resp := make([]statusResponse, 0, len(req))
	for _, item := range req {
		err := p.store.deleteDocument(r.Context(), item.ID)
		if err == nil {
			p.deleteMetadataIndex(r.Context(), item.ID)
		}
		resp = append(resp, statusResponse{ID: item.ID, Success: err == nil})
	}
	writeJSON(w, http.StatusOK, resp)
	_ = claims
}

func (p *protocol) handleListDocuments(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	withBlob := r.URL.Query().Get("withBlob") == "true"
	docID := r.URL.Query().Get("doc")
	rows, err := p.store.listMetadata(r.Context(), docID)
	if err != nil {
		http.Error(w, "failed to list documents", http.StatusInternalServerError)
		return
	}
	resp := make([]documentMeta, 0, len(rows))
	for _, meta := range rows {
		meta.Success = true
		if withBlob {
			token, err := p.store.issuePresigned(r.Context(), presignedTarget{
				Kind:       "document",
				Scope:      "read",
				DocumentID: meta.ID,
			}, urlTokenTTL)
			if err != nil {
				http.Error(w, "failed to issue download URL", http.StatusInternalServerError)
				return
			}
			meta.BlobURLGet = externalBaseURL(r) + "/storage/" + url.PathEscape(token)
			meta.BlobURLExpires = time.Now().Add(urlTokenTTL).UTC().Format(time.RFC3339Nano)
		}
		resp = append(resp, meta)
	}
	writeJSON(w, http.StatusOK, resp)
	_ = claims
}

func (p *protocol) handleSignedBlobDownload(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	var req signedBlobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	token, err := p.store.issuePresigned(r.Context(), presignedTarget{
		Kind:   "blob",
		Scope:  "read",
		BlobID: req.RelativePath,
	}, urlTokenTTL)
	if err != nil {
		http.Error(w, "failed to issue URL", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, signedBlobResponse{
		Expires:      time.Now().Add(urlTokenTTL).UTC().Format(time.RFC3339Nano),
		Method:       http.MethodGet,
		RelativePath: req.RelativePath,
		URL:          externalBaseURL(r) + "/blobstorage/" + url.PathEscape(token),
	})
	_ = claims
}

func (p *protocol) handleSignedBlobUpload(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	var req signedBlobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	token, err := p.store.issuePresigned(r.Context(), presignedTarget{
		Kind:   "blob",
		Scope:  "write",
		BlobID: req.RelativePath,
	}, urlTokenTTL)
	if err != nil {
		http.Error(w, "failed to issue URL", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, signedBlobResponse{
		Expires:        time.Now().Add(urlTokenTTL).UTC().Format(time.RFC3339Nano),
		Method:         http.MethodPut,
		RelativePath:   req.RelativePath,
		URL:            externalBaseURL(r) + "/blobstorage/" + url.PathEscape(token),
		MaxRequestSize: 7_000_000_000,
	})
	_ = claims
}

func (p *protocol) handleSyncComplete(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	writeJSON(w, http.StatusOK, map[string]string{"id": claims.DeviceID})
	p.refreshMetadataIndex(r.Context())
	p.notify(claims) // legacy v2 sync-complete → push to peers
}

func (p *protocol) handlePutRootV3(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	var req syncRootRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	gen, err := p.store.putBlob(r.Context(), rootBlobID, strings.NewReader(req.Hash), req.Generation)
	if errors.Is(err, errGenerationMismatch) {
		http.Error(w, "generation mismatch", http.StatusPreconditionFailed)
		return
	}
	if err != nil {
		http.Error(w, "failed to update root", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, syncRootResponse{Generation: gen, Hash: req.Hash})
	p.refreshMetadataIndex(r.Context())
	p.notify(claims) // v3/v4 root commit → push to peers
}

func (p *protocol) handleGetRootV3(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	rec, err := p.store.getBlob(r.Context(), rootBlobID)
	if errors.Is(err, errBlobNotFound) {
		http.Error(w, `{"message":"root not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to load root", http.StatusInternalServerError)
		return
	}
	data, err := osReadFile(rec.Path)
	if err != nil {
		http.Error(w, "failed to read root", http.StatusInternalServerError)
		return
	}
	writeJSONHashed(w, http.StatusOK, syncRootResponse{Generation: rec.Generation, Hash: string(data)})
	_ = claims
}

func (p *protocol) handleGetRootV4(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	rec, err := p.store.getBlob(r.Context(), rootBlobID)
	if errors.Is(err, errBlobNotFound) {
		writeJSONHashed(w, http.StatusOK, syncRootV4Response{SchemaVersion: 3})
		return
	}
	if err != nil {
		http.Error(w, "failed to load root", http.StatusInternalServerError)
		return
	}
	data, err := osReadFile(rec.Path)
	if err != nil {
		http.Error(w, "failed to read root", http.StatusInternalServerError)
		return
	}
	writeJSONHashed(w, http.StatusOK, syncRootV4Response{
		Generation:    rec.Generation,
		Hash:          string(data),
		SchemaVersion: 3,
	})
	_ = claims
}

func (p *protocol) handleGetBlobDirect(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	p.serveBlobByID(w, r, r.PathValue("file"))
	_ = claims
}

func (p *protocol) handlePutBlobDirect(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	if _, err := p.store.putBlob(r.Context(), r.PathValue("file"), r.Body, 0); err != nil {
		http.Error(w, "failed to store blob", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = claims
}

func (p *protocol) handleCheckFiles(w http.ResponseWriter, r *http.Request, claims tokenClaims) {
	var req checkFilesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	resp := missingFilesResponse{}
	for _, blobID := range req.Files {
		if _, err := p.store.getBlob(r.Context(), blobID); err != nil {
			resp.MissingFiles = append(resp.MissingFiles, blobID)
		}
	}
	writeJSON(w, http.StatusOK, resp)
	_ = claims
}

func (p *protocol) handleStorageGet(w http.ResponseWriter, r *http.Request) {
	target, err := p.store.loadPresigned(r.Context(), r.PathValue("token"))
	if err != nil || target.Kind != "document" || target.Scope != "read" {
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}
	path, err := p.store.getDocument(target.DocumentID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, path)
}

func (p *protocol) handleStoragePut(w http.ResponseWriter, r *http.Request) {
	target, err := p.store.loadPresigned(r.Context(), r.PathValue("token"))
	if err != nil || target.Kind != "document" || target.Scope != "write" {
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}
	if err := p.store.putDocument(r.Context(), target.DocumentID, r.Body); err != nil {
		http.Error(w, "failed to store document", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (p *protocol) handleBlobGet(w http.ResponseWriter, r *http.Request) {
	target, err := p.store.loadPresigned(r.Context(), r.PathValue("token"))
	if err != nil || target.Kind != "blob" || target.Scope != "read" {
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}
	p.serveBlobByID(w, r, target.BlobID)
}

func (p *protocol) handleBlobPut(w http.ResponseWriter, r *http.Request) {
	target, err := p.store.loadPresigned(r.Context(), r.PathValue("token"))
	if err != nil || target.Kind != "blob" || target.Scope != "write" {
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}
	gen, err := p.store.putBlob(r.Context(), target.BlobID, r.Body, 0)
	if err != nil {
		http.Error(w, "failed to store blob", http.StatusInternalServerError)
		return
	}
	w.Header().Set("x-goog-generation", fmt.Sprintf("%d", gen))
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (p *protocol) serveBlobByID(w http.ResponseWriter, r *http.Request, blobID string) {
	rec, err := p.store.getBlob(r.Context(), blobID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("x-goog-generation", fmt.Sprintf("%d", rec.Generation))
	w.Header().Set("x-goog-hash", "crc32c="+rec.CRC32C)
	http.ServeFile(w, r, rec.Path)
}

func (p *protocol) handleHWR(w http.ResponseWriter, r *http.Request, _ tokenClaims) {
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		http.Error(w, "missing body", http.StatusBadRequest)
		return
	}
	resp, err := newHWRClient(p.cfg).Recognize(r.Context(), body)
	if err != nil {
		if errors.Is(err, errHWRNotConfigured) {
			http.Error(w, "handwriting recognition not configured", http.StatusInternalServerError)
			return
		}
		p.logger.Warn("remarkable hwr proxy failed", "error", err)
		http.Error(w, "handwriting recognition failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", hwrJIIX)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

// handleNotificationsWS upgrades the device's notification handshake to a
// websocket and joins it to the hub. Auth happens BEFORE the upgrade (we can't
// reuse withUserAuth, which writes a JSON response). The device sends its user
// token as a Bearer header on the handshake.
func (p *protocol) handleNotificationsWS(w http.ResponseWriter, r *http.Request) {
	claims, err := p.userClaims(r.Context(), readBearerToken(r))
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if p.hub == nil {
		http.Error(w, "notifications unavailable", http.StatusServiceUnavailable)
		return
	}
	conn, err := notifyUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		p.logger.Debug("remarkable: notification upgrade failed", "error", err)
		return
	}
	_ = p.store.touchDevice(r.Context(), claims.DeviceID, claims.DeviceDesc)
	p.hub.connectWS(claims.UserID, claims.DeviceID, conn)
}

// notify fans a SyncComplete event out to the user's other devices. No-op when
// the hub isn't wired. Fire-and-forget — never blocks or fails the caller.
func (p *protocol) notify(claims tokenClaims) {
	if p.hub == nil {
		return
	}
	p.hub.notifySync(claims.UserID, claims.DeviceID, claims.DeviceDesc)
}

func (p *protocol) withUserAuth(next func(http.ResponseWriter, *http.Request, tokenClaims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer := readBearerToken(r)
		claims, err := p.userClaims(r.Context(), bearer)
		if err != nil {
			if strings.HasPrefix(r.URL.Path, "/search/") {
				jti, ok := parseUserJTI(bearer)
				if len(jti) > 12 {
					jti = jti[:12] + "..."
				}
				p.logger.Warn("remarkable search auth failed", "path", r.URL.Path, "jwt", ok, "jti", jti, "error", err)
			}
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if err := p.store.touchDevice(r.Context(), claims.DeviceID, claims.DeviceDesc); err != nil {
			http.Error(w, "failed to update device", http.StatusInternalServerError)
			return
		}
		next(w, r, claims)
	}
}

func (p *protocol) deviceClaims(ctx context.Context, token string) (tokenClaims, error) {
	return p.store.loadToken(ctx, token, "device")
}

func (p *protocol) userClaims(ctx context.Context, token string) (tokenClaims, error) {
	// The device returns the user token as a JWT carrying the DB token id as
	// its jti; validate that id against the store. Fall back to a direct lookup
	// for legacy opaque tokens (and so a non-JWT bearer still fails cleanly).
	if jwtClaims, ok := parseUserClaims(token); ok {
		claims, err := p.store.loadToken(ctx, jwtClaims.ID, "user")
		if err == nil {
			return claims, nil
		}
		if errors.Is(err, errTokenNotFound) {
			return p.store.recoverUserToken(ctx, jwtClaims)
		}
		return tokenClaims{}, err
	}
	claims, err := p.store.loadToken(ctx, token, "user")
	if err == nil {
		return claims, nil
	}
	if errors.Is(err, errTokenNotFound) {
		return p.store.recoverOpaqueUserToken(ctx, token)
	}
	return tokenClaims{}, err
}

func readBearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

func externalBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var osReadFile = func(path string) ([]byte, error) {
	return os.ReadFile(path)
}
