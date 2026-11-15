package mcp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// bulkSpec declares an operation whose body takes one large string or a
// large array of small objects with numbers.
const bulkSpec = `{"openapi": "3.1.0", "info": {"title": "Bulk", "version": "1"}, "paths": {
	"/bulk": {"post": {"operationId": "bulk.send", "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {
		"payload": {"type": "string"},
		"items": {"type": "array", "items": {"type": "object", "properties": {
			"id": {"type": "integer"}, "price": {"type": "number"}, "qty": {"type": "integer"}, "sku": {"type": "string"}
		}}}
	}}}}}, "responses": {"200": {"description": "OK"}}}}
}}`

// discardApp reads and drops the request body and answers {}.
func discardApp() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
}

// bulkArgs is the arguments object of bulk.send with about size bytes of
// JSON: one string, or an array of small objects.
func bulkArgs(shape string, size int) string {
	if shape == "string" {
		return `{"payload":"` + strings.Repeat("x", size) + `"}`
	}
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"price":%d.25,"qty":%d,"sku":"s%d"}`, i, i%1000, i%7, i%100)
	}
	b.WriteString(`]}`)
	return b.String()
}

// heapPeak samples the bytes of heap objects until stop is called and
// returns the most it saw.
func heapPeak() func() uint64 {
	var peak atomic.Uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		for {
			metrics.Read(sample)
			v := sample[0].Value.Uint64()
			if v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-done:
				return
			default:
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	return func() uint64 {
		close(done)
		wg.Wait()
		return peak.Load()
	}
}

// BenchmarkCallFunctionLargeArguments posts one call_function request of
// about 4 MB of arguments, just under the 4 MiB the transport takes, to
// Server.Handler and reports, besides the allocations, the heap peak of a
// call above the heap after a collection. The peak counts garbage not yet
// collected; run with GOGC=10 for one close to the most the call holds live.
func BenchmarkCallFunctionLargeArguments(b *testing.B) {
	for _, shape := range []string{"string", "objects"} {
		b.Run(shape, func(b *testing.B) {
			srv, err := New(Config{Spec: []byte(bulkSpec), Handler: discardApp()})
			require.NoError(b, err)
			handler := srv.Handler()
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"call_function","arguments":{"name":"bulk.send","args":` +
				bulkArgs(shape, 4_000_000) + `}}}`

			var peaks uint64
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				rec := httptest.NewRecorder()
				runtime.GC()
				sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
				metrics.Read(sample)
				base := sample[0].Value.Uint64()
				stop := heapPeak()
				b.StartTimer()

				handler.ServeHTTP(rec, req)

				b.StopTimer()
				peaks += stop() - base
				if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"text":"{}"`)) {
					b.Fatalf("status %d: %.300s", rec.Code, rec.Body.String())
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(peaks)/float64(b.N)/(1<<20), "peak-MiB/op")
		})
	}
}
