package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Disposable fixture ONLY. A returned response proves the handler's transaction
// finished; blocking before delivery exposes the committed-but-unacknowledged gap.
// The parent kills this process after reading the checkpoint line. No timed race.
func gateResponse(next http.Handler, target string) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+":"+r.URL.Path != target {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		if rec.Code >= 200 && rec.Code < 300 {
			once.Do(func() { fmt.Println(`{"checkpoint":"server_response"}`); <-r.Context().Done() })
		}
		for k, values := range rec.Header() {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}
