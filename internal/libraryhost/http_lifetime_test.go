package libraryhost

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdkruzr/rhizome/server-go/bounded"
	"github.com/sysop/ultrabridge/internal/logging"
)

type observedBody struct {
	io.ReadCloser
	reads   atomic.Int32
	waiting chan struct{}
}

func (b *observedBody) Read(out []byte) (int, error) {
	if b.reads.Add(1) == 2 {
		close(b.waiting)
	}
	return b.ReadCloser.Read(out)
}

func TestCloseInterruptsSlowAuthenticatedHTTPBody(t *testing.T) {
	h, _, token := fixture(t, nil)
	waiting := make(chan struct{})
	server := httptest.NewServer(logging.RequestID(slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &observedBody{ReadCloser: r.Body, waiting: waiting}
		h.ServeHTTP(w, r)
	})))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = fmt.Fprintf(conn, "POST /sync/v1 HTTP/1.1\r\nHost: fixture\r\nAuthorization: Bearer %s\r\n%s: 1\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{", token, bounded.Header); err != nil {
		t.Fatal(err)
	}
	await(t, waiting) // The decoder has consumed the first byte and awaits more.
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	await(t, closed)
}
