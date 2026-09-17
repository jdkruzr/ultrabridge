package libraryhost

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/auth"
	"github.com/sysop/ultrabridge/internal/libraryrestore"
	"github.com/sysop/ultrabridge/internal/readerstore"
	"github.com/sysop/ultrabridge/internal/syncassets"
	"github.com/sysop/ultrabridge/internal/syncstore"
)

type Options struct {
	Account   *auth.Middleware
	Worker    readerstore.WorkerOptions
	Restore   bool
	StageRoot string
	// Every additional library author/materializer must join this lifetime before
	// Restore is enabled. There are no production-specific workers in this package.
	Auxiliary      func(context.Context) func()
	WriterChanged  func(context.Context, []syncstore.TablePK)
	ReplaceDerived func(context.Context, *sql.Tx) error
	// In-memory invalidation only, after successful commit and before restart.
	AfterReplace func()
}

// Host owns admitted requests and workers, but never closes the shared DB.
// Mount all protocol paths through this instance; do not retain a legacy bypass.
type Host struct {
	ctx             context.Context
	cancel          context.CancelFunc
	state           sync.Mutex
	closed          bool
	requests        sync.WaitGroup
	admission       sync.RWMutex
	once            sync.Once
	workers         *ReaderWorkers
	afterReplace    func()
	normal, restore http.Handler
}

func New(ctx context.Context, db *sql.DB, options Options) (*Host, error) {
	if options.Account == nil {
		return nil, fmt.Errorf("shared library account authentication required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := readerstore.NewWorker(readerstore.New(db), nil, options.Worker); err != nil {
		return nil, err
	}
	if err := syncassets.Migrate(ctx, db); err != nil {
		return nil, err
	}
	if options.Restore {
		if err := libraryrestore.Install(ctx, db); err != nil {
			return nil, err
		}
	}
	run, cancel := context.WithCancel(ctx)
	h := &Host{ctx: run, cancel: cancel, afterReplace: options.AfterReplace}
	// Build routes before starting jobs, so a failed installation has no worker leak.
	normal, err := EnrolledHandler(ctx, db, RouteOptions{Assets: true,
		Search:     http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.workers.SearchHandler().ServeHTTP(w, r) }),
		WakeReader: func() { h.workers.Wake() }, WriterChanged: options.WriterChanged}, options.Account)
	if err != nil {
		cancel()
		return nil, err
	}
	h.normal = normal
	h.workers, err = NewReaderWorkers(run, db, options.Worker, options.Auxiliary)
	if err != nil {
		cancel()
		return nil, err
	}
	if options.Restore {
		service := &libraryrestore.Service{DB: db, StageRoot: options.StageRoot, Exclusive: h.Exclusive, ReplaceDerived: options.ReplaceDerived}
		h.restore = service.Handler(options.Account)
	}
	return h, nil
}

// Do includes manual source mutations/reprocessing in replacement admission.
// Callers must not cache a worker pointer outside the admitted function.
func (h *Host) Do(ctx context.Context, work func(context.Context) error) error {
	h.state.Lock()
	if h.closed {
		h.state.Unlock()
		return libraryrestore.ErrWorkersClosed
	}
	h.requests.Add(1)
	h.state.Unlock()
	defer h.requests.Done()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	h.admission.RLock()
	defer h.admission.RUnlock()
	if err := h.ctx.Err(); err != nil {
		return err
	}
	if err := run.Err(); err != nil {
		return err
	}
	return work(run)
}

// Exclusive joins requests already admitted to the old state before stopping
// workers. Waiting/new requests authenticate against the replacement afterward.
func (h *Host) Exclusive(ctx context.Context, replace func() error) error {
	h.admission.Lock()
	defer h.admission.Unlock()
	if err := h.ctx.Err(); err != nil {
		return err
	}
	return h.workers.Exclusive(ctx, func() error {
		if err := replace(); err != nil {
			return err
		}
		if h.afterReplace != nil {
			h.afterReplace()
		}
		return nil
	})
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Context cancellation alone does not interrupt a blocked request-body read.
	// Bound authenticated slow clients and wake socket I/O when this owner closes.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	ioStopped := make(chan struct{})
	stopIO := context.AfterFunc(h.ctx, func() {
		defer close(ioStopped)
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stopIO() {
			<-ioStopped
		}
	}()
	if strings.HasPrefix(r.URL.Path, "/sync/restore/v1/") {
		if h.restore == nil {
			http.NotFound(w, r)
			return
		}
		// Publication must not hold a read lock while acquiring Exclusive. Staging is
		// cancellable and tracked, so shutdown still joins it before database closure.
		h.state.Lock()
		if h.closed {
			h.state.Unlock()
			http.Error(w, "library_unavailable", 503)
			return
		}
		h.requests.Add(1)
		h.state.Unlock()
		defer h.requests.Done()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(h.ctx, cancel)
		defer stop()
		if h.ctx.Err() != nil {
			http.Error(w, "library_unavailable", 503)
			return
		}
		h.restore.ServeHTTP(w, r.WithContext(ctx))
		return
	}
	err := h.Do(r.Context(), func(ctx context.Context) error { h.normal.ServeHTTP(w, r.WithContext(ctx)); return nil })
	if err != nil {
		http.Error(w, "library_unavailable", 503)
	}
}
func (h *Host) Close() {
	h.once.Do(func() {
		h.state.Lock()
		h.closed = true
		h.cancel()
		h.state.Unlock()
		h.requests.Wait()
		h.workers.Close()
	})
}
