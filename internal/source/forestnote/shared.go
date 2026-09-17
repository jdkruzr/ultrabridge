package forestnote

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/libraryhost"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/synchttp"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

func (s *Source) checkCutover(ctx context.Context) error {
	var activated int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='alexandria_source_activation'`).Scan(&activated); err != nil {
		return err
	}
	if !s.cfg.SharedLibrary {
		if activated != 0 {
			return fmt.Errorf("shared library already activated; refusing legacy sync downgrade")
		}
		return nil
	}
	if s.fnDeps.Account == nil {
		return fmt.Errorf("shared library requires account authentication")
	}
	if s.cfg.Compaction {
		return fmt.Errorf("disable writer-only compaction before shared library activation")
	}
	if s.fnDeps.EmbedStore != nil && s.fnDeps.ForgetEmbeddings == nil {
		return fmt.Errorf("shared library requires embedding cache invalidation")
	}
	// Sticky even after a failed startup: an explicit cutover must fail closed,
	// never quietly re-enable the legacy credential/generation bypass.
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS alexandria_source_activation (id INTEGER PRIMARY KEY CHECK(id=1)); INSERT OR IGNORE INTO alexandria_source_activation VALUES(1)`)
	return err
}

func (s *Source) startShared(ctx context.Context) error {
	h, err := libraryhost.New(ctx, s.db, libraryhost.Options{
		Account: s.fnDeps.Account, Worker: readerstore.DefaultWorkerOptions(), Restore: true,
		Auxiliary: func(run context.Context) func() {
			w := &Source{name: s.name, db: s.db, store: s.store, deps: s.deps, fnDeps: s.fnDeps, cfg: Config{BatchLimit: s.cfg.BatchLimit}}
			w.startWriter(run, true)
			s.writer.Store(w)
			return w.Stop
		},
		WriterChanged: func(ctx context.Context, changed []syncstore.TablePK) {
			if w := s.writer.Load(); w != nil {
				w.bridge.PagesChanged(ctx, changed)
			}
		},
		ReplaceDerived: func(ctx context.Context, tx *sql.Tx) error {
			for _, q := range []string{`DELETE FROM note_content WHERE note_path LIKE 'forestnote://%'`, `DELETE FROM note_embeddings WHERE note_path LIKE 'forestnote://%'`} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			return nil
		},
		AfterReplace: func() {
			if s.fnDeps.ForgetEmbeddings != nil {
				s.fnDeps.ForgetEmbeddings("forestnote://")
			}
		},
	})
	if err != nil {
		return err
	}
	s.host = h
	return nil
}

func (s *Source) withWriter(ctx context.Context, work func(context.Context, *Source) error) error {
	if s == nil {
		return errSourceStopping
	}
	if s.host != nil {
		return s.host.Do(ctx, func(run context.Context) error {
			w := s.writer.Load()
			return w.background.run(run, func(run context.Context) error { return work(run, w) })
		})
	}
	return s.background.run(ctx, func(run context.Context) error { return work(run, s) })
}

// Admit includes whole service mutations (including their cache cleanup) and
// global embedding backfill in the same generation boundary as device requests.
func (s *Source) Admit(ctx context.Context, work func(context.Context) error) error {
	return s.withWriter(ctx, func(run context.Context, _ *Source) error { return work(run) })
}
func (s *Source) AdmitEmbedding(ctx context.Context, path string, work func(context.Context) error) error {
	if strings.HasPrefix(path, "forestnote://") {
		return s.Admit(ctx, work)
	}
	return work(ctx)
}

// All protocol routes are mounted together. There is never a parallel legacy
// /sync/v1 handler once enrolled mixed-library mode is active.
func (s *Source) RegisterRoutes(mux *http.ServeMux, account *auth.Middleware) {
	if s.host != nil {
		for _, path := range []string{"/sync/v1", "/sync/capabilities", "/sync/devices/v1/", "/sync/assets/v1/", "/sync/restore/v1/", "/reader/search"} {
			mux.Handle(path, s.host)
		}
	} else {
		mux.Handle("/sync/v1", account.Wrap(synchttp.New(s.syncSvc, synchttp.DefaultMaxBytes, s.deps.Logger)))
	}
}

// Rebuild derived text from saved transcriptions after restart/replacement.
// Keyset batches keep memory bounded; no OCR upload or new synced OCR authorship.
func (s *Source) rebuildSavedText(ctx context.Context) error {
	after := ""
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT p.id, COALESCE(t.text,'') FROM fn_page p JOIN fn_notebook n ON n.id=p.notebook_id LEFT JOIN fn_page_text_from_server t ON t.id=p.id AND t.deleted_at IS NULL WHERE p.deleted_at IS NULL AND n.deleted_at IS NULL AND p.id>? ORDER BY p.id LIMIT 128`, after)
		if err != nil {
			return err
		}
		type page struct{ id, text string }
		batch := []page{}
		for rows.Next() {
			var p page
			if err = rows.Scan(&p.id, &p.text); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, p := range batch {
			if err = s.bridge.IndexSavedText(ctx, p.id, p.text); err != nil {
				return err
			}
			after = p.id
		}
	}
}
