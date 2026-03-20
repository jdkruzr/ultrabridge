package processor

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// spcCatalog updates the Supernote Private Cloud MariaDB catalog after a
// successful OCR injection. All SQL operations are best-effort.
type spcCatalog struct {
	db *sql.DB
}

// NewSPCCatalog returns a CatalogUpdater backed by the given MariaDB connection.
func NewSPCCatalog(db *sql.DB) CatalogUpdater {
	return &spcCatalog{db: db}
}

// AfterInject updates the SPC catalog for the file at path. It performs five
// steps — stat+MD5, SELECT f_user_file, UPDATE f_user_file, INSERT f_file_action,
// UPDATE f_capacity — where each step after the SELECT is independent: a failure
// in one is logged and does not prevent the others from executing. If the SELECT
// finds no row, the remaining steps are skipped.
func (c *spcCatalog) AfterInject(ctx context.Context, path string) error {
	// Step 1: stat the file and compute its MD5.
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("spc catalog: stat failed", "path", path, "err", err)
		return nil
	}
	newSize := info.Size()

	f, err := os.Open(path)
	if err != nil {
		slog.Warn("spc catalog: open for md5 failed", "path", path, "err", err)
		return nil
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		slog.Warn("spc catalog: md5 compute failed", "path", path, "err", err)
		return nil
	}
	newMD5 := hex.EncodeToString(h.Sum(nil))

	// Step 2: look up the catalog row. inner_name is the file's basename — SPC
	// uses the filename as the stable identifier in f_user_file.
	innerName := filepath.Base(path)
	var fileID, userID, oldSize int64
	var fileName string
	err = c.db.QueryRowContext(ctx,
		"SELECT id, user_id, size, COALESCE(file_name, ?) FROM f_user_file WHERE inner_name = ?",
		innerName, innerName,
	).Scan(&fileID, &userID, &oldSize, &fileName)
	if err == sql.ErrNoRows {
		slog.Warn("spc catalog: no f_user_file row", "inner_name", innerName)
		return nil
	}
	if err != nil {
		slog.Warn("spc catalog: select f_user_file failed", "inner_name", innerName, "err", err)
		return nil
	}

	now := time.Now().UnixMilli()

	// Step 3: update f_user_file with new size, md5, and timestamp.
	if _, err := c.db.ExecContext(ctx,
		"UPDATE f_user_file SET size=?, md5=?, update_time=? WHERE id=?",
		newSize, newMD5, now, fileID,
	); err != nil {
		slog.Warn("spc catalog: update f_user_file failed", "file_id", fileID, "err", err)
	}

	// Step 4: insert an audit record. id uses UnixNano for uniqueness (fits bigint).
	actionID := time.Now().UnixNano()
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO f_file_action
			(id, user_id, file_id, file_name, inner_name, path, is_folder, size, md5, action, create_time, update_time)
			VALUES (?, ?, ?, ?, ?, 'NOTE/Note/', 'N', ?, ?, 'A', ?, ?)`,
		actionID, userID, fileID, fileName, innerName, newSize, newMD5, now, now,
	); err != nil {
		slog.Warn("spc catalog: insert f_file_action failed", "file_id", fileID, "err", err)
	}

	// Step 5: adjust quota by the size delta (may be negative if file shrank).
	delta := newSize - oldSize
	if _, err := c.db.ExecContext(ctx,
		"UPDATE f_capacity SET used_capacity = used_capacity + ?, update_time=? WHERE user_id=?",
		delta, now, userID,
	); err != nil {
		slog.Warn("spc catalog: update f_capacity failed", "user_id", userID, "err", err)
	}

	return nil
}
