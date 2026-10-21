package ctrl

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/integrations/aws/s3"
	"github.com/vanclief/compose/integrations/aws/ses"
	"github.com/vanclief/compose/integrations/promtail"
	"github.com/vanclief/ez"
)

// fakeLoki records the log lines pushed to it.
type fakeLoki struct {
	mu    sync.Mutex
	lines []string
}

func (f *fakeLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Streams []struct {
			Values [][2]string `json:"values"`
		} `json:"streams"`
	}

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, s := range req.Streams {
		for _, v := range s.Values {
			f.lines = append(f.lines, v[1])
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeLoki) pushed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.lines...)
}

func testPromtailParams(host string) *promtail.WithPromtailParams {
	return &promtail.WithPromtailParams{
		App:               "api",
		Environment:       "test",
		PromtailHost:      host,
		PromtailUsername:  "user",
		PromtailEnabled:   true,
		PromtailTimeoutMS: 5000,
	}
}

func newTestWriter(t *testing.T, stdout io.Writer) (*promtailWriter, *fakeLoki) {
	t.Helper()

	loki := &fakeLoki{}
	server := httptest.NewServer(loki)
	t.Cleanup(server.Close)

	client, err := promtail.NewClient(testPromtailParams(server.URL))
	require.NoError(t, err)

	return &promtailWriter{stdout: stdout, client: client}, loki
}

func containsLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}

	return false
}

func TestPromtailWriterCloseDrainsBufferedLines(t *testing.T) {
	var stdout bytes.Buffer
	w, loki := newTestWriter(t, &stdout)

	n, err := w.Write([]byte("buffered line\n"))
	require.NoError(t, err)
	assert.Equal(t, len("buffered line\n"), n)
	assert.Equal(t, "buffered line\n", stdout.String())

	err = w.Close()
	require.NoError(t, err)

	assert.Equal(t, []string{"buffered line"}, loki.pushed())
}

// TestPromtailWriterClosedByZerologChain mirrors what zerolog's Fatal does
// before os.Exit: it closes the logger's writer, which WithZerolog sets to a
// ConsoleWriter wrapped in a LevelWriterAdapter.
func TestPromtailWriterClosedByZerologChain(t *testing.T) {
	w, loki := newTestWriter(t, io.Discard)

	console := zerolog.ConsoleWriter{Out: w, NoColor: true}
	logger := zerolog.New(console)
	logger.Info().Msg("about to exit")

	var closer io.Closer = zerolog.LevelWriterAdapter{Writer: console}
	err := closer.Close()
	require.NoError(t, err)

	assert.True(t, containsLine(loki.pushed(), "about to exit"))
}

func TestPromtailWriterCloseTwice(t *testing.T) {
	w, loki := newTestWriter(t, io.Discard)

	_, err := w.Write([]byte("once\n"))
	require.NoError(t, err)

	err = w.Close()
	require.NoError(t, err)

	err = w.Close()
	require.NoError(t, err)

	assert.Equal(t, []string{"once"}, loki.pushed())
}

func TestWithPromtailLogWriterIsCloser(t *testing.T) {
	previous := log.Logger
	t.Cleanup(func() {
		log.Logger = previous
	})

	loki := &fakeLoki{}
	server := httptest.NewServer(loki)
	t.Cleanup(server.Close)

	c := &BaseController{}
	err := c.WithPromtail(testPromtailParams(server.URL))
	require.NoError(t, err)

	closer, ok := c.logWriter.(io.Closer)
	require.True(t, ok, "log writer must be an io.Closer so zerolog's Fatal flushes it")

	err = closer.Close()
	require.NoError(t, err)

	assert.True(t, containsLine(loki.pushed(), "Promtail Config"))

	// CloseLogs closes the same client again and must still succeed
	err = c.CloseLogs(t.Context())
	require.NoError(t, err)
}

// TestWithSESReturnsClientError checks that a client setup failure is returned
// to the caller instead of exiting the process.
func TestWithSESReturnsClientError(t *testing.T) {
	c := &BaseController{}

	client, err := c.WithSES(context.Background(), &ses.Config{}, "secret")
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

// TestWithS3ReturnsClientError checks that a client setup failure is returned
// to the caller instead of exiting the process.
func TestWithS3ReturnsClientError(t *testing.T) {
	c := &BaseController{}

	client, err := c.WithS3(context.Background(), &s3.Config{}, "secret")
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}
