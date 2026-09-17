package libraryhost

import (
	"context"
	"database/sql"
	"net/http"
	"sync/atomic"

	"github.com/sysop/ultrabridge/internal/libraryrestore"
	"github.com/sysop/ultrabridge/internal/readersearch"
	"github.com/sysop/ultrabridge/internal/readerstore"
)

// ReaderWorkers creates fresh projection AND search instances after replacement.
// Auxiliary may join other source workers into this same cancel/join lifetime.
// Its factory must not fail; validate dependencies before constructing this owner.
type ReaderWorkers struct {
	lifetime *libraryrestore.Workers
	current  atomic.Pointer[readerstore.Worker]
	search   atomic.Pointer[readersearch.Store]
}

func NewReaderWorkers(ctx context.Context, db *sql.DB, options readerstore.WorkerOptions, auxiliary func(context.Context) func()) (*ReaderWorkers, error) {
	// Validate options before installing tables or launching anything.
	if _, err := readerstore.NewWorker(readerstore.New(db), nil, options); err != nil {
		return nil, err
	}
	if err := readerstore.Install(ctx, db); err != nil {
		return nil, err
	}
	if err := readersearch.Install(ctx, db); err != nil {
		return nil, err
	}
	w := &ReaderWorkers{}
	w.lifetime = libraryrestore.NewWorkers(ctx, func(run context.Context) func() {
		search := readersearch.New(db)
		worker, _ := readerstore.NewWorker(readerstore.New(db), search.Schedule, options)
		w.current.Store(worker)
		w.search.Store(search)
		var joinAuxiliary func()
		if auxiliary != nil {
			joinAuxiliary = auxiliary(run)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			searchDone := make(chan struct{})
			go func() { defer close(searchDone); _ = search.Run(run, options.OnError) }()
			_ = worker.Run(run)
			<-searchDone
		}()
		return func() {
			<-done
			if joinAuxiliary != nil {
				joinAuxiliary()
			}
		}
	})
	return w, nil
}
func (w *ReaderWorkers) Wake()  { w.current.Load().Wake() }
func (w *ReaderWorkers) Close() { w.lifetime.Close() }
func (w *ReaderWorkers) Exclusive(ctx context.Context, replace func() error) error {
	return w.lifetime.Exclusive(ctx, replace)
}
func (w *ReaderWorkers) SearchHandler() http.Handler {
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) { w.search.Load().Handler().ServeHTTP(out, r) })
}
