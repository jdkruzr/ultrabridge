package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/appconfig"
	"github.com/sysop/ultrabridge/internal/libraryrestore"
	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/source"
	"github.com/sysop/ultrabridge/internal/syncidentity"
	"github.com/sysop/ultrabridge/internal/syncstore"
	"github.com/sysop/ultrabridge/internal/syncsvc"
	"golang.org/x/crypto/bcrypt"
)

// Exercise main's real auth/config/source/router assembly over TCP, not assetlab.
// The subprocess receives only fixture paths and no inherited UB configuration.
func TestProductionBinaryLibraryCutoverAndRestore(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ultrabridge")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	ctx := context.Background()
	dbPath := filepath.Join(dir, "notes.db")
	db, err := notedb.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash, _ := bcrypt.GenerateFromPassword([]byte("fixture"), bcrypt.MinCost)
	for key, value := range map[string]string{appconfig.KeyUsername: "owner", appconfig.KeyPasswordHash: string(hash), appconfig.KeyOCREnabled: "false", appconfig.KeyEmbedEnabled: "false", appconfig.KeyChatEnabled: "false"} {
		if err = notedb.SetSetting(ctx, db, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = source.AddSource(ctx, db, source.SourceRow{Type: "forestnote", Name: "fixture", Enabled: true, ConfigJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO note_content(note_path,page,body_text,indexed_at) VALUES('boox.note',0,'preserve me',1)`); err != nil {
		t.Fatal(err)
	}
	const site = "0000000000000000000000000A"
	const peer = "0000000000000000000000000B"
	client := &http.Client{Timeout: 10 * time.Second}
	start := func() (string, func()) {
		t.Helper()
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		addr := listener.Addr().String()
		listener.Close()
		log, e := os.CreateTemp(dir, "server-*.log")
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(binary)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "UB_DB_PATH=" + dbPath, "UB_TASK_DB_PATH=" + filepath.Join(dir, "tasks.db"), "UB_LISTEN_ADDR=" + addr, "UB_PASSWORD_HASH_PATH=" + filepath.Join(dir, "no-secret"), "UB_LOG_LEVEL=warn"}
		cmd.Stdout = log
		cmd.Stderr = log
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					_ = cmd.Process.Kill()
					<-done
					t.Error("server shutdown timed out")
				}
				log.Close()
			})
		}
		t.Cleanup(stop)
		url := "http://" + addr
		until := time.Now().Add(15 * time.Second)
		for {
			response, e := client.Get(url + "/sync/capabilities")
			if e == nil {
				response.Body.Close()
				break
			}
			if time.Now().After(until) {
				stop()
				out, _ := os.ReadFile(log.Name())
				t.Fatalf("server startup: %s", out)
			}
			time.Sleep(25 * time.Millisecond)
		}
		return url, stop
	}
	url, stop := start()
	call := func(method, path, token string, body any, want int) []byte {
		t.Helper()
		var data []byte
		if raw, ok := body.([]byte); ok {
			data = raw
		} else {
			data, _ = json.Marshal(body)
		}
		r, _ := http.NewRequest(method, url+path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(bounded.Header, "1")
		if token == "account" {
			r.SetBasicAuth("owner", "fixture")
		} else if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if strings.Contains(path, "/chunks/") {
			r.Header.Set("Content-Type", "application/octet-stream")
			r.Header.Set("X-Rhizome-Chunk-SHA256", assets.Digest(data))
		}
		response, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		out, e := io.ReadAll(response.Body)
		if e != nil {
			t.Fatal(e)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: %d wanted %d: %s", method, path, response.StatusCode, want, out)
		}
		return out
	}
	call("POST", "/sync/v1", "account", syncsvc.Request{ProtocolVersion: 1, SchemaHash: syncstore.SchemaHash(), SiteID: "0000000000000000000000000D"}, 200)
	call("GET", "/sync/capabilities", "account", nil, 404)
	stop()
	if _, err = db.Exec(`UPDATE sources SET config_json='{"shared_library":true,"compaction":false}' WHERE type='forestnote'`); err != nil {
		t.Fatal(err)
	}
	url, stop = start()
	call("POST", "/sync/v1", "account", nil, 401)
	token := func(letter string) string { return syncidentity.TokenPrefix + strings.Repeat(letter, 64) }
	tokenHash := func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }
	call("POST", "/sync/devices/v1/enroll", "account", syncidentity.Enrollment{SiteID: "0000000000000000000000000D", TokenHash: tokenHash(token("d"))}, 409)
	for id, key := range map[string]string{site: token("a"), peer: token("b")} {
		call("POST", "/sync/devices/v1/enroll", "account", syncidentity.Enrollment{SiteID: id, TokenHash: tokenHash(key)}, 204)
	}
	call("GET", "/sync/capabilities", token("a"), nil, 200)
	row := func(table, pk string, seq int, cols map[string]any) json.RawMessage {
		out, _ := json.Marshal(map[string]any{"table": table, "pk": pk, "site_id": site, "op_ts": seq, "op_seq": seq, "cols": cols})
		return out
	}
	notebook := row("notebook", "00000000000000000000000001", 1, map[string]any{"name": "TCP Notebook", "sort_order": 0, "created_at": 1, "deleted_at": nil, "folder_id": nil, "aspect_long_axis": nil, "page_width": nil, "page_height": nil})
	title := row("reader_book_title", strings.Repeat("a", 64), 2, map[string]any{"title": "TCP Book"})
	call("POST", "/sync/v1", token("a"), bounded.Request{ProtocolVersion: 1, SchemaHash: readercontract.CandidateCombined().SchemaHash(), SiteID: site, Ops: []json.RawMessage{notebook, title}}, 200)
	pulled := call("POST", "/sync/v1", token("b"), bounded.Request{ProtocolVersion: 1, SchemaHash: readercontract.CandidateCombined().SchemaHash(), SiteID: peer}, 200)
	if !bytes.Contains(pulled, []byte("TCP Notebook")) || !bytes.Contains(pulled, []byte("TCP Book")) {
		t.Fatalf("mixed rows missing: %s", pulled)
	}
	call("GET", "/reader/search?q=missing", token("a"), nil, 200)
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	manifest, _ := z.Create("manifest.json")
	_ = json.NewEncoder(manifest).Encode(libraryrestore.Manifest{Version: 1, Schema: readercontract.CandidateCombined().SchemaHash(), Publisher: site})
	_, _ = z.Create("rows.jsonl")
	_ = z.Close()
	data := archive.Bytes()
	snapshot := assets.Digest(data)
	path := "/sync/assets/v1/" + snapshot
	call("PUT", path, token("a"), map[string]any{"asset_id": snapshot, "byte_length": strconv.Itoa(len(data)), "chunk_bytes": assets.ChunkBytes}, 201)
	call("PUT", path+"/chunks/0", token("a"), data, 204)
	call("POST", path+"/complete", token("a"), nil, 200)
	var state struct {
		Generation string `json:"generation"`
	}
	if err = json.Unmarshal(call("GET", "/sync/restore/v1/state", token("a"), nil, 200), &state); err != nil {
		t.Fatal(err)
	}
	requestID := strings.Repeat("1", 64)
	publication := call("POST", "/sync/restore/v1/publish", "account", map[string]string{"request_id": requestID, "expected_generation": state.Generation, "snapshot": snapshot, "publisher": site}, 200)
	var baseline libraryrestore.Baseline
	if err = json.Unmarshal(publication, &baseline); err != nil {
		t.Fatal(err)
	}
	stop()
	url, stop = start()
	call("GET", "/sync/capabilities", token("b"), nil, 409)
	call("POST", "/sync/v1", "account", nil, 401)
	call("GET", "/sync/restore/v1/publications/"+requestID, token("a"), nil, 200)
	call("POST", "/sync/restore/v1/adopt", token("b"), libraryrestore.Adoption{Generation: baseline.Generation, Snapshot: snapshot, Site: "0000000000000000000000000C", TokenHash: tokenHash(token("c"))}, 200)
	call("GET", "/sync/capabilities", token("c"), nil, 200)
	call("GET", "/sync/capabilities", token("b"), nil, 409)
	stop()
	var body string
	if err = db.QueryRow(`SELECT body_text FROM note_content WHERE note_path='boox.note'`).Scan(&body); err != nil || body != "preserve me" {
		t.Fatal("unrelated source changed", err, body)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM fn_notebook`).Scan(&count); err != nil || count != 0 {
		t.Fatal("restore did not replace", err, count)
	}
}
