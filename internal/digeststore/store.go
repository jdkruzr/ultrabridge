package digeststore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store is the canonical digest store over the shared notedb handle. It owns no
// connection lifecycle (the handle is opened/closed by notedb.Open in main).
type Store struct {
	db  *sql.DB
	now func() time.Time // injectable for tests
}

// New returns a Store backed by db (the shared notedb pool).
func New(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

func boolToYN(b bool) string {
	if b {
		return "Y"
	}
	return "N"
}

// digestColumns is the SELECT column list in struct-field order, shared by all
// read paths so scanDigest stays in lockstep.
const digestColumns = `id, file_id, user_id, name, unique_identifier, parent_unique_identifier,
	content, source_path, data_source, source_type, is_group, description, tags, md5_hash,
	metadata, comment_str, comment_handwrite_name, handwrite_inner_name, handwrite_md5,
	creation_time, last_modified_time, author, is_deleted, created_at, updated_at`

func scanDigest(sc interface{ Scan(...any) error }) (Digest, error) {
	var d Digest
	var isGroup, isDeleted string
	err := sc.Scan(
		&d.ID, &d.FileID, &d.UserID, &d.Name, &d.UniqueIdentifier, &d.ParentUniqueIdentifier,
		&d.Content, &d.SourcePath, &d.DataSource, &d.SourceType, &isGroup, &d.Description, &d.Tags, &d.MD5Hash,
		&d.Metadata, &d.CommentStr, &d.CommentHandwriteName, &d.HandwriteInnerName, &d.HandwriteMD5,
		&d.CreationTime, &d.LastModifiedTime, &d.Author, &isDeleted, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return Digest{}, err
	}
	d.IsGroup = isGroup == "Y"
	d.IsDeleted = isDeleted == "Y"
	return d, nil
}

// Create inserts a digest (item or group) and returns its assigned id. CreatedAt
// and UpdatedAt are stamped to now if unset.
func (s *Store) Create(ctx context.Context, d *Digest) (int64, error) {
	now := s.now().UnixMilli()
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}
	if d.UpdatedAt == 0 {
		d.UpdatedAt = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO digests
		(file_id, user_id, name, unique_identifier, parent_unique_identifier, content, source_path,
		 data_source, source_type, is_group, description, tags, md5_hash, metadata, comment_str,
		 comment_handwrite_name, handwrite_inner_name, handwrite_md5, creation_time, last_modified_time,
		 author, is_deleted, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.FileID, d.UserID, d.Name, d.UniqueIdentifier, d.ParentUniqueIdentifier, d.Content, d.SourcePath,
		d.DataSource, d.SourceType, boolToYN(d.IsGroup), d.Description, d.Tags, d.MD5Hash, d.Metadata, d.CommentStr,
		d.CommentHandwriteName, d.HandwriteInnerName, d.HandwriteMD5, d.CreationTime, d.LastModifiedTime,
		d.Author, boolToYN(d.IsDeleted), d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("digeststore Create: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("digeststore Create lastid: %w", err)
	}
	d.ID = id
	return id, nil
}

// Update overwrites the mutable fields of an existing, non-deleted digest owned
// by d.UserID. Returns ErrNotFound if no such row. UpdatedAt is bumped to now.
func (s *Store) Update(ctx context.Context, d *Digest) error {
	res, err := s.db.ExecContext(ctx, `UPDATE digests SET
		name=?, unique_identifier=?, parent_unique_identifier=?, content=?, source_path=?, data_source=?,
		source_type=?, is_group=?, description=?, tags=?, md5_hash=?, metadata=?, comment_str=?,
		comment_handwrite_name=?, handwrite_inner_name=?, handwrite_md5=?, last_modified_time=?, author=?,
		updated_at=?
		WHERE id=? AND user_id=? AND is_deleted='N'`,
		d.Name, d.UniqueIdentifier, d.ParentUniqueIdentifier, d.Content, d.SourcePath, d.DataSource,
		d.SourceType, boolToYN(d.IsGroup), d.Description, d.Tags, d.MD5Hash, d.Metadata, d.CommentStr,
		d.CommentHandwriteName, d.HandwriteInnerName, d.HandwriteMD5, d.LastModifiedTime, d.Author,
		s.now().UnixMilli(), d.ID, d.UserID,
	)
	if err != nil {
		return fmt.Errorf("digeststore Update: %w", err)
	}
	return errIfNoRows(res)
}

// SoftDelete marks a digest is_deleted='Y'. Returns ErrNotFound if absent.
func (s *Store) SoftDelete(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE digests SET is_deleted='Y', updated_at=? WHERE id=? AND user_id=? AND is_deleted='N'`,
		s.now().UnixMilli(), id, userID)
	if err != nil {
		return fmt.Errorf("digeststore SoftDelete: %w", err)
	}
	return errIfNoRows(res)
}

// SoftDeleteByParent soft-deletes every item whose parent_unique_identifier
// matches (used when a group is deleted). Returns the number affected.
func (s *Store) SoftDeleteByParent(ctx context.Context, userID int64, parentUID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE digests SET is_deleted='Y', updated_at=? WHERE user_id=? AND parent_unique_identifier=? AND is_deleted='N'`,
		s.now().UnixMilli(), userID, parentUID)
	if err != nil {
		return 0, fmt.Errorf("digeststore SoftDeleteByParent: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetByID returns a non-deleted digest owned by userID, or ErrNotFound.
func (s *Store) GetByID(ctx context.Context, userID, id int64) (*Digest, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+digestColumns+` FROM digests WHERE id=? AND user_id=? AND is_deleted='N'`, id, userID)
	d, err := scanDigest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("digeststore GetByID: %w", err)
	}
	return &d, nil
}

// GetByUniqueIdentifier returns a non-deleted digest by its UUID, or ErrNotFound.
func (s *Store) GetByUniqueIdentifier(ctx context.Context, userID int64, uid string) (*Digest, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+digestColumns+` FROM digests WHERE unique_identifier=? AND user_id=? AND is_deleted='N'`, uid, userID)
	d, err := scanDigest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("digeststore GetByUniqueIdentifier: %w", err)
	}
	return &d, nil
}

// List returns non-deleted digests of the requested kind (isGroup) owned by
// userID, newest-modified first, plus the total matching count. parentUID ""
// means no parent filter. page is 1-based; size <= 0 means no limit.
func (s *Store) List(ctx context.Context, userID int64, isGroup bool, parentUID string, page, size int) ([]Digest, int64, error) {
	where := `WHERE user_id=? AND is_deleted='N' AND is_group=?`
	args := []any{userID, boolToYN(isGroup)}
	if parentUID != "" {
		where += ` AND parent_unique_identifier=?`
		args = append(args, parentUID)
	}

	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM digests `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("digeststore List count: %w", err)
	}

	q := `SELECT ` + digestColumns + ` FROM digests ` + where + ` ORDER BY last_modified_time DESC, id DESC`
	if size > 0 {
		if page < 1 {
			page = 1
		}
		q += ` LIMIT ? OFFSET ?`
		args = append(args, size, (page-1)*size)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("digeststore List: %w", err)
	}
	defer rows.Close()
	out, err := scanRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListByIDs returns the non-deleted digests with the given ids owned by userID.
// A nil/empty id list returns an empty slice (not an error).
func (s *Store) ListByIDs(ctx context.Context, userID int64, ids []int64) ([]Digest, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	q := `SELECT ` + digestColumns + ` FROM digests WHERE user_id=? AND is_deleted='N' AND id IN (` +
		strings.Join(ph, ",") + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("digeststore ListByIDs: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows *sql.Rows) ([]Digest, error) {
	var out []Digest
	for rows.Next() {
		d, err := scanDigest(rows)
		if err != nil {
			return nil, fmt.Errorf("digeststore scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("digeststore rows: %w", err)
	}
	return out, nil
}

// --- Tags ---

// CreateTag inserts a user-scoped tag and returns its id.
func (s *Store) CreateTag(ctx context.Context, userID int64, name string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO digest_tags (user_id, name, created_at) VALUES (?,?,?)`,
		userID, name, s.now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("digeststore CreateTag: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("digeststore CreateTag lastid: %w", err)
	}
	return id, nil
}

// UpdateTag renames a tag owned by userID. Returns ErrNotFound if absent.
func (s *Store) UpdateTag(ctx context.Context, userID, id int64, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE digest_tags SET name=? WHERE id=? AND user_id=?`, name, id, userID)
	if err != nil {
		return fmt.Errorf("digeststore UpdateTag: %w", err)
	}
	return errIfNoRows(res)
}

// DeleteTag hard-deletes a tag owned by userID (tags are not soft-deleted; they
// carry no sync state). Returns ErrNotFound if absent.
func (s *Store) DeleteTag(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM digest_tags WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return fmt.Errorf("digeststore DeleteTag: %w", err)
	}
	return errIfNoRows(res)
}

// ListTags returns all tags owned by userID, oldest first.
func (s *Store) ListTags(ctx context.Context, userID int64) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, name, created_at FROM digest_tags WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("digeststore ListTags: %w", err)
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("digeststore ListTags scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func errIfNoRows(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
