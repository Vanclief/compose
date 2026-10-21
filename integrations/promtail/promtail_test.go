package promtail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeLoki struct {
	mu       sync.Mutex
	requests []pushRequest
	statuses []int
}

func (f *fakeLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.statuses) > 0 {
		status := f.statuses[0]
		f.statuses = f.statuses[1:]
		if status != http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
	}

	var req pushRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.requests = append(f.requests, req)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeLoki) pushed() []pushRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.requests
}

func (f *fakeLoki) lineCounts() []int {
	var counts []int
	for _, req := range f.pushed() {
		count := 0
		for _, s := range req.Streams {
			count += len(s.Values)
		}
		counts = append(counts, count)
	}

	return counts
}

func testParams(host string) *WithPromtailParams {
	return &WithPromtailParams{
		App:               "api",
		Environment:       "test",
		PromtailHost:      host,
		PromtailUsername:  "user",
		PromtailLabels:    "method,status",
		PromtailEnabled:   true,
		PromtailTimeoutMS: 5000,
	}
}

func newTestClient(t *testing.T, host string) *Client {
	t.Helper()

	client, err := NewClient(testParams(host))
	require.NoError(t, err)

	return client
}

// newTestClientFlushingEvery starts a client whose timer flushes at interval.
func newTestClientFlushingEvery(t *testing.T, host string, interval time.Duration) *Client {
	t.Helper()

	client, err := newClient(testParams(host))
	require.NoError(t, err)

	client.flushInterval = interval
	go client.run()

	return client
}

func TestNewClientUsesEffectiveTimeout(t *testing.T) {
	params := testParams("http://loki")
	params.PromtailTimeoutMS = 250

	client, err := newClient(params)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(params.EffectiveTimeoutMS())*time.Millisecond, client.httpClient.Timeout)
}

func TestClientFlushesFullBatchBeforeClose(t *testing.T) {
	loki := &fakeLoki{}
	server := httptest.NewServer(loki)
	defer server.Close()

	// Keep the timer out of the way so only the size limit triggers a push
	client := newTestClientFlushingEvery(t, server.URL, time.Hour)

	logLine := []byte(strings.Repeat("x", 1024) + "\n")
	batchLines := maxBatchBytes/len(logLine) + 1
	written := batchLines + 10
	for range written {
		_, err := client.Write(logLine)
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return len(loki.pushed()) == 1
	}, time.Second, 5*time.Millisecond)

	err := client.Close(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []int{batchLines, written - batchLines}, loki.lineCounts())
}

func TestClientFlushesOnTimer(t *testing.T) {
	loki := &fakeLoki{}
	server := httptest.NewServer(loki)
	defer server.Close()

	client := newTestClientFlushingEvery(t, server.URL, 20*time.Millisecond)

	for range 3 {
		_, err := client.Write([]byte("INF tick\n"))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return len(loki.pushed()) > 0
	}, time.Second, 5*time.Millisecond)

	err := client.Close(context.Background())
	require.NoError(t, err)

	total := 0
	for _, count := range loki.lineCounts() {
		total += count
	}
	assert.Equal(t, 3, total)
}

func TestClientBatchesLinesIntoStreamsByLabels(t *testing.T) {
	loki := &fakeLoki{}
	server := httptest.NewServer(loki)
	defer server.Close()

	client := newTestClient(t, server.URL)

	writes := []string{
		"\x1b[32mINF\x1b[0m request \x1b[36mmethod=\x1b[0mGET \x1b[36mstatus=\x1b[0m200\n",
		"INF request method=GET status=200 uri=/a\n",
		"ERR request method=POST status=500\n",
		"INF no labels here\n",
	}
	for _, w := range writes {
		n, err := client.Write([]byte(w))
		require.NoError(t, err)
		assert.Equal(t, len(w), n)
	}

	err := client.Close(context.Background())
	require.NoError(t, err)

	requests := loki.pushed()
	require.Len(t, requests, 1)

	streams := requests[0].Streams
	require.Len(t, streams, 3)

	assert.Equal(t, map[string]string{"app": "api", "env": "test", "method": "GET", "status": "200"}, streams[0].Stream)
	require.Len(t, streams[0].Values, 2)
	assert.Equal(t, "INF request method=GET status=200", streams[0].Values[0][1])
	assert.Equal(t, "INF request method=GET status=200 uri=/a", streams[0].Values[1][1])

	assert.Equal(t, map[string]string{"app": "api", "env": "test", "method": "POST", "status": "500"}, streams[1].Stream)
	assert.Equal(t, map[string]string{"app": "api", "env": "test"}, streams[2].Stream)
}

func TestClientRetriesUnavailableLoki(t *testing.T) {
	loki := &fakeLoki{statuses: []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}}
	server := httptest.NewServer(loki)
	defer server.Close()

	client := newTestClient(t, server.URL)

	_, err := client.Write([]byte("INF hello\n"))
	require.NoError(t, err)

	err = client.Close(context.Background())
	require.NoError(t, err)

	requests := loki.pushed()
	require.Len(t, requests, 1)
	assert.Equal(t, "INF hello", requests[0].Streams[0].Values[0][1])
}

func TestClientDoesNotRetryRejectedPush(t *testing.T) {
	loki := &fakeLoki{statuses: []int{http.StatusBadRequest}}
	server := httptest.NewServer(loki)
	defer server.Close()

	client := newTestClient(t, server.URL)

	_, err := client.Write([]byte("INF hello\n"))
	require.NoError(t, err)

	err = client.Close(context.Background())
	require.NoError(t, err)

	assert.Empty(t, loki.pushed())
	assert.Empty(t, loki.statuses)
}

func TestClientWriteDoesNotBlockOnStuckLoki(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	client := newTestClient(t, server.URL)

	// Big enough lines to fill a batch, so the push blocks and the buffer fills up
	logLine := []byte(strings.Repeat("x", 1024) + "\n")

	start := time.Now()
	for range bufferedLines * 2 {
		_, err := client.Write(logLine)
		require.NoError(t, err)
	}
	assert.Less(t, time.Since(start), time.Second)
	assert.Positive(t, client.dropped.Load())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := client.Close(ctx)
	require.Error(t, err)
}

func TestClientDropsWritesAfterClose(t *testing.T) {
	loki := &fakeLoki{}
	server := httptest.NewServer(loki)
	defer server.Close()

	client := newTestClient(t, server.URL)

	_, err := client.Write([]byte("INF before close\n"))
	require.NoError(t, err)

	err = client.Close(context.Background())
	require.NoError(t, err)
	require.Len(t, loki.pushed(), 1)

	for range 3 {
		n, err := client.Write([]byte("INF after close\n"))
		require.NoError(t, err)
		assert.Equal(t, len("INF after close\n"), n)
	}

	err = client.Close(context.Background())
	require.NoError(t, err)

	assert.Empty(t, client.lines)
	assert.Zero(t, client.dropped.Load())
	assert.Len(t, loki.pushed(), 1)
}

func TestClientCloseFinishesWithConcurrentWrites(t *testing.T) {
	loki := &fakeLoki{}
	server := httptest.NewServer(loki)
	defer server.Close()

	client := newTestClient(t, server.URL)

	stop := make(chan struct{})
	var writers sync.WaitGroup
	for range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, err := client.Write([]byte("INF busy\n"))
					assert.NoError(t, err)
				}
			}
		}()
	}

	// Let the writers get going before closing
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := client.Close(ctx)
	elapsed := time.Since(start)

	close(stop)
	writers.Wait()

	require.NoError(t, err)
	assert.Less(t, elapsed, 2*time.Second)
	assert.Empty(t, client.lines)
}

func TestClientLabelsIgnoreValuesOutsideCharset(t *testing.T) {
	client, err := newClient(testParams("http://loki"))
	require.NoError(t, err)

	tests := []struct {
		name string
		text string
		want map[string]string
	}{
		{
			name: "quoted value with spaces",
			text: `INF request method="GET /a" status=200`,
			want: map[string]string{"app": "api", "env": "test", "status": "200"},
		},
		{
			name: "quotes and brackets",
			text: `INF request method=[GET] status="200"`,
			want: map[string]string{"app": "api", "env": "test"},
		},
		{
			name: "too long",
			text: "INF request method=" + strings.Repeat("a", 65) + " status=200",
			want: map[string]string{"app": "api", "env": "test", "status": "200"},
		},
		{
			name: "allowed punctuation",
			text: "INF request method=a-b_c.d:e@f/g status=200",
			want: map[string]string{"app": "api", "env": "test", "method": "a-b_c.d:e@f/g", "status": "200"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, client.labels(tt.text))
		})
	}
}

func TestClientLabelsCannotOverrideStaticLabels(t *testing.T) {
	params := testParams("http://loki")
	params.PromtailLabels = "method,env,app"

	client, err := newClient(params)
	require.NoError(t, err)

	labels := client.labels("INF request env=prod app=evil method=GET")
	assert.Equal(t, map[string]string{"app": "api", "env": "test", "method": "GET"}, labels)
}

func TestClientLabelsLastTokenWins(t *testing.T) {
	client, err := newClient(testParams("http://loki"))
	require.NoError(t, err)

	// ConsoleWriter prints the message before the fields, so the real field comes last
	labels := client.labels("INF user sent method=EVIL method=GET status=200")
	assert.Equal(t, map[string]string{"app": "api", "env": "test", "method": "GET", "status": "200"}, labels)

	// An invalid real value drops the label instead of falling back to an injected token
	labels = client.labels("INF user sent status=200 status=[bad]")
	assert.Equal(t, map[string]string{"app": "api", "env": "test"}, labels)
}

func TestClientLabelsDropValuesPastLimit(t *testing.T) {
	client, err := newClient(testParams("http://loki"))
	require.NoError(t, err)

	for i := range maxLabelValues {
		labels := client.labels("INF request method=m" + strconv.Itoa(i) + " status=200")
		assert.Equal(t, "m"+strconv.Itoa(i), labels["method"])
	}

	labels := client.labels("INF request method=overflow status=200")
	assert.Equal(t, map[string]string{"app": "api", "env": "test", "status": "200"}, labels)

	// Values already seen keep working, and other names have their own budget
	labels = client.labels("INF request method=m0 status=201")
	assert.Equal(t, map[string]string{"app": "api", "env": "test", "method": "m0", "status": "201"}, labels)
	assert.Len(t, client.seen["method"], maxLabelValues)
	assert.True(t, client.capped["method"])
	assert.False(t, client.capped["status"])
}
