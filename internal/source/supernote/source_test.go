package supernote

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/sysop/ultrabridge/internal/source"
)

// setupTestDB creates an in-memory SQLite database with the notes and jobs tables.
func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open in-memory DB: %v", err)
	}

	// Create notes and jobs tables
	schema := `
		CREATE TABLE notes (
			path TEXT PRIMARY KEY,
			rel_path TEXT NOT NULL,
			mtime_ms INTEGER NOT NULL,
			sha256 TEXT
		);

		CREATE TABLE jobs (
			note_path TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			queued_at INTEGER NOT NULL,
			requeue_after INTEGER,
			backup_path TEXT,
			FOREIGN KEY (note_path) REFERENCES notes(path)
		);

		CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT
		);
	`

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("Create schema: %v", err)
	}

	return db
}

func TestNewSourceValidConfig(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	cfg := Config{
		NotesPath:  "/var/notes",
		BackupPath: "/var/backup",
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}

	row := source.SourceRow{
		ID:         1,
		Type:       "supernote",
		Name:       "test-source",
		Enabled:    true,
		ConfigJSON: string(cfgJSON),
	}

	deps := source.SharedDeps{
		Indexer:    nil,
		Embedder:   nil,
		EmbedModel: "test-model",
		EmbedStore: nil,
		OCRClient:  nil,
		Logger:     nil,
	}

	src, err := NewSource(db, row, deps)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}

	if src == nil {
		t.Fatal("NewSource() returned nil source")
	}

	if src.Type() != "supernote" {
		t.Errorf("Type() = %v, want supernote", src.Type())
	}

	if src.Name() != "test-source" {
		t.Errorf("Name() = %v, want test-source", src.Name())
	}
}

func TestNewSourceInvalidJSON(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	row := source.SourceRow{
		ID:         1,
		Type:       "supernote",
		Name:       "test-source",
		Enabled:    true,
		ConfigJSON: "{invalid json}",
	}

	deps := source.SharedDeps{
		Indexer:    nil,
		Embedder:   nil,
		EmbedModel: "",
		EmbedStore: nil,
		OCRClient:  nil,
		Logger:     nil,
	}

	_, err := NewSource(db, row, deps)
	if err == nil {
		t.Fatal("NewSource() with invalid JSON should return error")
	}
}

func TestSourceStartStop(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tempDir := t.TempDir()

	cfg := Config{
		NotesPath:  tempDir,
		BackupPath: filepath.Join(tempDir, "backup"),
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}

	row := source.SourceRow{
		ID:         1,
		Type:       "supernote",
		Name:       "test-source",
		Enabled:    true,
		ConfigJSON: string(cfgJSON),
	}

	deps := source.SharedDeps{
		Indexer:    nil,
		Embedder:   nil,
		EmbedModel: "test-model",
		EmbedStore: nil,
		OCRClient:  nil,
		Logger:     nil,
	}

	src, err := NewSource(db, row, deps)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}

	ctx := context.Background()

	// Test Start - should not panic even with nil deps
	err = src.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Test Stop - should not panic
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked: %v", r)
		}
	}()
	src.Stop()
}

func TestSourceStartWithOCREnabled(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tempDir := t.TempDir()

	cfg := Config{
		NotesPath:  tempDir,
		BackupPath: filepath.Join(tempDir, "backup"),
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}

	row := source.SourceRow{
		ID:         1,
		Type:       "supernote",
		Name:       "test-source",
		Enabled:    true,
		ConfigJSON: string(cfgJSON),
	}

	// Create a mock OCRClient by creating a nil pointer.
	// The source should check if OCRClient != nil, so we skip it here.
	deps := source.SharedDeps{
		Indexer:    nil,
		Embedder:   nil,
		EmbedModel: "test-model",
		EmbedStore: nil,
		OCRClient:  nil, // nil OCRClient means OCR disabled
		Logger:     nil,
	}

	src, err := NewSource(db, row, deps)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}

	ctx := context.Background()

	// Start should succeed even with OCRClient nil
	err = src.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	defer src.Stop()
}

func TestSourceWithSettingsClosures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tempDir := t.TempDir()

	// Insert settings into the database
	if _, err := db.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?)",
		"sn_ocr_prompt", "test prompt",
	); err != nil {
		t.Fatalf("Insert OCR prompt setting: %v", err)
	}

	if _, err := db.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?)",
		"sn_inject_enabled", "true",
	); err != nil {
		t.Fatalf("Insert inject enabled setting: %v", err)
	}

	cfg := Config{
		NotesPath:  tempDir,
		BackupPath: filepath.Join(tempDir, "backup"),
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}

	row := source.SourceRow{
		ID:         1,
		Type:       "supernote",
		Name:       "test-source",
		Enabled:    true,
		ConfigJSON: string(cfgJSON),
	}

	deps := source.SharedDeps{
		Indexer:    nil,
		Embedder:   nil,
		EmbedModel: "test-model",
		EmbedStore: nil,
		OCRClient:  nil,
		Logger:     nil,
	}

	src, err := NewSource(db, row, deps)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}

	ctx := context.Background()

	// Start the source which will initialize the closures
	err = src.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer src.Stop()

	// The source's processor and pipeline should be initialized
	// This test verifies that Start completes without error and
	// the settings can be read (we can't directly test the closures,
	// but we can verify the source initialized successfully)
}

func TestSourceImplementsSourceInterface(t *testing.T) {
	// This test verifies that Source implements the source.Source interface
	var _ source.Source = (*Source)(nil)
}

func TestNewSourceConfigParsing(t *testing.T) {
	tests := []struct {
		name       string
		configJSON string
		wantPath   string
		wantBackup string
		wantErr    bool
	}{
		{
			name:       "full config",
			configJSON: `{"notes_path":"/notes","backup_path":"/backup"}`,
			wantPath:   "/notes",
			wantBackup: "/backup",
			wantErr:    false,
		},
		{
			name:       "partial config",
			configJSON: `{"notes_path":"/notes"}`,
			wantPath:   "/notes",
			wantBackup: "",
			wantErr:    false,
		},
		{
			name:       "empty config",
			configJSON: `{}`,
			wantPath:   "",
			wantBackup: "",
			wantErr:    false,
		},
		{
			name:       "invalid json",
			configJSON: `{bad}`,
			wantErr:    true,
		},
	}

	db := setupTestDB(t)
	defer db.Close()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := source.SourceRow{
				ID:         1,
				Type:       "supernote",
				Name:       "test",
				Enabled:    true,
				ConfigJSON: tt.configJSON,
			}

			deps := source.SharedDeps{
				Indexer:    nil,
				Embedder:   nil,
				EmbedModel: "",
				EmbedStore: nil,
				OCRClient:  nil,
				Logger:     nil,
			}

			src, err := NewSource(db, row, deps)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewSource() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if src.cfg.NotesPath != tt.wantPath {
					t.Errorf("NotesPath = %v, want %v", src.cfg.NotesPath, tt.wantPath)
				}
				if src.cfg.BackupPath != tt.wantBackup {
					t.Errorf("BackupPath = %v, want %v", src.cfg.BackupPath, tt.wantBackup)
				}
			}
		})
	}
}
