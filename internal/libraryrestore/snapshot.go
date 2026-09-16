// Package libraryrestore implements candidate-only authoritative library replacement.
// It never opens an uploaded SQLite database or copies unrelated Server state.
package libraryrestore

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jdkruzr/rhizome/server-go/assets"
	"github.com/jdkruzr/rhizome/server-go/registry"
	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

const MaxSnapshotBytes int64 = 64 << 30

var ErrSnapshot = errors.New("invalid_library_snapshot")

// The archive contains manifest.json, rows.jsonl, and complete original books at
// assets/<sha256>. Missing books stay missing; row metadata is never restamped.
type Manifest struct {
	Version   int    `json:"version"`
	Schema    string `json:"schema"`
	Publisher string `json:"publisher"`
	HighWater int64  `json:"high_water"`
}

type prepared struct {
	db       *sql.DB
	dir      string
	manifest Manifest
	seq      int64
}

func (p *prepared) close() { p.db.Close(); os.RemoveAll(p.dir) } // Private MkdirTemp-owned stage only.

func prepare(ctx context.Context, source *assets.SQLStore, id string, root string) (_ *prepared, err error) {
	i, err := source.Describe(ctx, id)
	if err != nil {
		return nil, err
	}
	if i.State != "ready" || i.ByteLength > MaxSnapshotBytes {
		return nil, ErrSnapshot
	}
	dir, err := os.MkdirTemp(root, "restore-stage-")
	if err != nil {
		return nil, err
	}
	p := &prepared{dir: dir}
	defer func() {
		if err != nil {
			if p.db != nil {
				p.db.Close()
			}
			os.RemoveAll(dir)
		}
	}()
	f, err := os.OpenFile(filepath.Join(dir, "snapshot.zip"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hash := sha256.New()
	for n := int64(0); n < i.ChunkCount(); n++ {
		c, e := source.ReadChunk(ctx, id, n)
		if e != nil {
			return nil, e
		}
		if _, e = io.MultiWriter(f, hash).Write(c.Bytes); e != nil {
			return nil, e
		}
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != id {
		return nil, ErrSnapshot
	}
	z, err := zip.NewReader(f, i.ByteLength)
	if err != nil {
		return nil, ErrSnapshot
	}
	if len(z.File) < 2 || len(z.File) > 100000 {
		return nil, ErrSnapshot
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, entry := range z.File {
		if files[entry.Name] != nil || entry.UncompressedSize64 > uint64(MaxSnapshotBytes)-total {
			return nil, ErrSnapshot
		}
		total += entry.UncompressedSize64
		if entry.Name != "manifest.json" && entry.Name != "rows.jsonl" && !(strings.HasPrefix(entry.Name, "assets/") && assets.ValidID(strings.TrimPrefix(entry.Name, "assets/"))) {
			return nil, ErrSnapshot
		}
		files[entry.Name] = entry
	}
	m, rows := files["manifest.json"], files["rows.jsonl"]
	if m == nil || rows == nil || m.UncompressedSize64 > 4096 {
		return nil, ErrSnapshot
	}
	r, err := m.Open()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(io.LimitReader(r, 4097))
	dec.DisallowUnknownFields()
	var wire struct {
		Version   int    `json:"version"`
		Schema    string `json:"schema"`
		Publisher string `json:"publisher"`
		HighWater *int64 `json:"high_water"`
	}
	err = dec.Decode(&wire)
	var extra any
	if err == nil && dec.Decode(&extra) != io.EOF {
		err = ErrSnapshot
	}
	r.Close()
	if wire.HighWater == nil {
		return nil, ErrSnapshot
	}
	p.manifest = Manifest{wire.Version, wire.Schema, wire.Publisher, *wire.HighWater}
	if err != nil || p.manifest.Version != 1 || p.manifest.Schema != readercontract.CandidateCombined().SchemaHash() || !syncstore.IsULID(p.manifest.Publisher) || p.manifest.Publisher[0] > '7' || p.manifest.HighWater < 0 {
		return nil, ErrSnapshot
	}
	p.db, err = sql.Open("sqlite", filepath.Join(dir, "prepared.sqlite"))
	if err != nil {
		return nil, err
	}
	p.db.SetMaxOpenConns(1)
	for _, install := range []func(context.Context, *sql.DB) error{syncstore.Migrate, readerstore.Install, func(c context.Context, d *sql.DB) error { return (&assets.SQLStore{DB: d}).Migrate(c) }} {
		if err = install(ctx, p.db); err != nil {
			return nil, err
		}
	}
	// Each row is bounded. Never collect a whole library's payloads or book bytes.
	r, err = rows.Open()
	if err != nil {
		return nil, err
	}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 64<<10), readerstore.MaxRowBytes)
	for scan.Scan() {
		if err = ctx.Err(); err != nil {
			break
		}
		if err = p.row(ctx, scan.Bytes()); err != nil {
			break
		}
	}
	if err == nil {
		err = scan.Err()
	}
	r.Close()
	if err != nil {
		return nil, err
	}
	// Materialize dependency-ordered reader rows using the normal domain gate.
	s := readerstore.New(p.db)
	for sweep := 0; sweep < 16; sweep++ {
		progress := 0
		var after int64
		for {
			page, e := s.Drain(ctx, after, 32)
			if e != nil {
				return nil, e
			}
			for _, v := range page.Records {
				if v.State != "pending" {
					progress++
				}
			}
			after = page.Next
			if after == 0 {
				break
			}
		}
		if progress == 0 {
			break
		}
	}
	var pending int
	if err = p.db.QueryRowContext(ctx, "SELECT count(*) FROM reader_store_incoming WHERE state!='applied'").Scan(&pending); err != nil {
		return nil, err
	}
	if pending != 0 {
		return nil, ErrSnapshot
	}
	dest := &assets.SQLStore{DB: p.db}
	for name, entry := range files {
		if !strings.HasPrefix(name, "assets/") {
			continue
		}
		assetID := strings.TrimPrefix(name, "assets/")
		var length int64
		if err = p.db.QueryRowContext(ctx, "SELECT byte_length FROM fn_reader_book WHERE asset_id=? LIMIT 1", assetID).Scan(&length); err != nil || length != int64(entry.UncompressedSize64) {
			return nil, ErrSnapshot
		}
		d := assets.Descriptor{ID: assetID, ByteLength: length, ChunkBytes: assets.ChunkBytes}
		if _, _, err = dest.Stage(ctx, d); err != nil {
			return nil, err
		}
		r, err = entry.Open()
		if err != nil {
			return nil, err
		}
		for n := int64(0); n < d.ChunkCount(); n++ {
			size, _ := d.ChunkLength(n)
			b := make([]byte, size)
			if _, err = io.ReadFull(r, b); err != nil {
				break
			}
			if err = dest.WriteChunk(ctx, assetID, n, b, assets.Digest(b)); err != nil {
				break
			}
		}
		if err == nil {
			var b [1]byte
			var n int
			n, err = r.Read(b[:])
			if err == io.EOF && n == 0 {
				err = nil
			} else {
				err = ErrSnapshot
			}
		}
		r.Close()
		if err != nil {
			return nil, err
		}
		if _, err = dest.Complete(ctx, assetID); err != nil {
			return nil, err
		}
	}
	if err = p.db.QueryRowContext(ctx, "SELECT last_seq FROM sync_seq WHERE id=1").Scan(&p.seq); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *prepared) row(ctx context.Context, raw []byte) error {
	if !utf8.Valid(raw) {
		return ErrSnapshot
	}
	var op readercontract.WireOp
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&op); err != nil {
		return ErrSnapshot
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return ErrSnapshot
	}
	def, ok := readercontract.CandidateCombined().ByName()[op.Table]
	if !ok || !syncstore.IsULID(op.SiteID) || op.SiteID[0] > '7' || op.OpTS < 0 || op.OpSeq <= 0 || len(op.Cols) != len(def.Columns) || op.PK == "" || len(op.PK) > 2048 {
		return ErrSnapshot
	}
	if op.SiteID == p.manifest.Publisher && op.OpSeq > p.manifest.HighWater {
		return ErrSnapshot
	}
	_, reader := readercontract.Registry().ByName()[op.Table]
	if !reader && !syncstore.IsULID(op.PK) {
		return ErrSnapshot
	}
	var rp *readerstore.Prepared
	var err error
	if reader {
		if _, _, err = readercontract.DecodeJSON(raw); err != nil {
			return ErrSnapshot
		}
		rp, err = readerstore.Prepare(op.SiteID, [][]byte{raw})
		if err != nil {
			return err
		}
	}
	cols := []string{"id"}
	args := []any{op.PK}
	for _, c := range def.Columns {
		v, exists := op.Cols[c.Name]
		if !exists {
			return ErrSnapshot
		}
		v = bytes.TrimSpace(v)
		var value any
		if bytes.Equal(v, []byte("null")) {
			if !c.Nullable {
				return ErrSnapshot
			}
		} else {
			switch c.Type {
			case registry.Text, registry.Blob:
				var text string
				if json.Unmarshal(v, &text) != nil {
					return ErrSnapshot
				}
				value = text
				if c.Type == registry.Blob {
					value, err = base64.StdEncoding.Strict().DecodeString(text)
					if err != nil {
						return ErrSnapshot
					}
				}
			case registry.Real:
				var n float64
				if json.Unmarshal(v, &n) != nil {
					return ErrSnapshot
				}
				value = n
			default:
				var n int64
				if json.Unmarshal(v, &n) != nil {
					return ErrSnapshot
				}
				value = n
			}
		}
		cols = append(cols, c.Name)
		args = append(args, value)
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Duplicate row winners or operation identities mean the snapshot is ambiguous.
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS restore_rows(tbl TEXT,pk TEXT,PRIMARY KEY(tbl,pk))`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO restore_rows VALUES(?,?)`, op.Table, op.PK); err != nil {
		return ErrSnapshot
	}
	if reader {
		err = rp.CommitTx(ctx, tx)
	} else {
		cols = append(cols, "lww_wall_ts", "lww_op_seq", "lww_site_id")
		args = append(args, op.OpTS, op.OpSeq, op.SiteID)
		_, err = tx.ExecContext(ctx, "INSERT INTO fn_"+op.Table+"("+strings.Join(cols, ",")+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+")", args...)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sync_seq SET last_seq=last_seq+1 WHERE id=1"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_ops SELECT last_seq,?,?,?,?,?,?,0 FROM sync_seq WHERE id=1`, op.SiteID, op.OpSeq, op.Table, op.PK, op.OpTS, string(raw)); err != nil {
		return fmt.Errorf("%w: duplicate operation", ErrSnapshot)
	}
	return tx.Commit()
}
