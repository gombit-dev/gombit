package scenario

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// RunParallelBenchmark reports how a stack scales across CPU cores by driving
// the plaintext and valid-POST scenarios concurrently with b.RunParallel, as
// b.Run sub-benchmarks. It is the contention counterpart to RunBenchmark, whose
// single-goroutine numbers hide any per-request serialization: a stack that
// funnels every request through a shared lock barely improves here as
// GOMAXPROCS rises, while a lock-free stack approaches linear scaling. That gap
// is exactly what a single-goroutine microbench cannot surface (issue #243,
// motivated by the metrics-mutex finding in #239).
//
// Read a row by comparing its per-op time across -cpu values, e.g.:
//
//	go test ./benchmarks/micro/... -bench=BenchmarkFrameworkTaxParallel -benchmem -cpu=1,2,4,8,16
//
// A row whose ns/op falls roughly in proportion to the core count scales;
// a row whose ns/op flattens is contention-bound. The shared plaintext GET
// request is safe to reuse across goroutines: none of the four stacks mutate
// the inbound *http.Request in place (context changes go through
// Request.WithContext, which returns a copy). POST cannot share a request — a
// drained body can't be re-read — so each iteration builds its own.
func RunParallelBenchmark(b *testing.B, stack Stack) {
	b.Helper()

	b.Run("plaintext", func(b *testing.B) {
		request := httptest.NewRequest(http.MethodGet, "/plaintext", nil)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				response := httptest.NewRecorder()
				stack.Handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					// Errorf, not Fatalf: this runs in a RunParallel worker
					// goroutine, and testing.Fatal/FailNow are only safe from the
					// benchmark's own goroutine. Errorf marks the failure
					// concurrency-safely; return stops this worker.
					b.Errorf("plaintext status = %d, want %d; body: %s", response.Code, http.StatusOK, response.Body.String())
					return
				}
			}
		})
	})

	b.Run("valid-post", func(b *testing.B) {
		payload := []byte(ValidCreateUserBody)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				request := httptest.NewRequest(http.MethodPost, "/users", bytes.NewReader(payload))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				stack.Handler.ServeHTTP(response, request)
				if !statusOKOrCreated(response.Code) {
					// Errorf, not Fatalf: called from a RunParallel worker
					// goroutine (see the plaintext site above).
					b.Errorf("valid-post status = %d; body: %s", response.Code, response.Body.String())
					return
				}
			}
		})
	})
}
