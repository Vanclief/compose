package promtail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vanclief/ez"
)

const (
	pushPath        = "/loki/api/v1/push"
	flushInterval   = time.Second
	maxBatchBytes   = 1 << 20
	bufferedLines   = 10000
	maxPushAttempts = 3
	retryBackoff    = 250 * time.Millisecond
	maxLabelValues  = 100
)

// Taken from https://github.com/acarl005/stripansi
var ansiRegex = regexp.MustCompile("[\u001B\u009B][[\\]()#;?]*(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))")

// labelValueRegex is the charset a value parsed from a log line needs to become
// a stream label, so free text cannot reach Loki's index.
var labelValueRegex = regexp.MustCompile(`^[A-Za-z0-9_.:@/-]{1,64}$`)

// Client is an io.Writer that ships log lines to Loki in batches. Write never
// waits on the network: lines are buffered and pushed by a background
// goroutine, and dropped when the buffer is full or the client is closed.
// Call Close before the process exits to push what is still buffered.
type Client struct {
	url           string
	username      string
	password      string
	labelNames    []string
	staticLabels  map[string]string
	httpClient    *http.Client
	flushInterval time.Duration

	lines   chan line
	dropped atomic.Int64
	done    chan struct{}

	// seen holds the distinct values accepted per label name, capped at
	// maxLabelValues to bound stream cardinality, and capped records the names
	// already reported as over the cap. Both are only touched from run.
	seen   map[string]map[string]struct{}
	capped map[string]bool

	// mu makes closing exact: Close waits for in-flight Writes, so once stop is
	// closed nothing else is queued and drain only pushes what was buffered.
	mu     sync.RWMutex
	closed bool
	stop   chan struct{}
}

type line struct {
	at   time.Time
	text []byte
}

type batch struct {
	lines []line
	size  int
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type pushRequest struct {
	Streams []*stream `json:"streams"`
}

func NewClient(params *WithPromtailParams) (*Client, error) {
	c, err := newClient(params)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	go c.run()

	return c, nil
}

// newClient builds the client without starting the background goroutine, so
// tests can adjust it first.
func newClient(params *WithPromtailParams) (*Client, error) {
	err := params.Validate()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	// A configured timeout below the threshold was tuned for the old per-line
	// client, so EffectiveTimeoutMS swaps in the batch default
	if params.PromtailTimeoutMS != 0 && params.PromtailTimeoutMS < legacyTimeoutThresholdMS {
		fmt.Fprintf(os.Stderr, "promtail: timeout of %dms looks like a per-line value from the old client, using the %dms batch default\n", params.PromtailTimeoutMS, DEFAULT_TIMEOUT_MS)
	}

	var labelNames []string
	for _, name := range strings.Split(params.PromtailLabels, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			labelNames = append(labelNames, name)
		}
	}

	c := &Client{
		url:        strings.TrimRight(params.PromtailHost, "/") + pushPath,
		username:   params.PromtailUsername,
		password:   params.PromtailPassword,
		labelNames: labelNames,
		staticLabels: map[string]string{
			"env": params.Environment,
			"app": params.App,
		},
		httpClient:    &http.Client{Timeout: time.Duration(params.EffectiveTimeoutMS()) * time.Millisecond},
		flushInterval: flushInterval,
		lines:         make(chan line, bufferedLines),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		seen:          map[string]map[string]struct{}{},
		capped:        map[string]bool{},
	}

	return c, nil
}

// Write queues p without blocking and always reports it as written. Lines
// written once Close has begun are silently dropped.
func (c *Client) Write(p []byte) (int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed {
		return len(p), nil
	}

	// The caller may reuse p after Write returns
	text := bytes.Clone(p)

	select {
	case c.lines <- line{at: time.Now(), text: text}:
	default:
		c.dropped.Add(1)
	}

	return len(p), nil
}

// Close stops accepting lines, pushes the ones already buffered and waits until
// they are sent or ctx expires. Lines written after Close begins are dropped.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.stop)
	}
	c.mu.Unlock()

	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ez.New(ez.ETIMEOUT, "Timed out pushing buffered logs to Loki", ctx.Err())
	}
}

func (c *Client) run() {
	defer close(c.done)

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	var pending batch
	for {
		select {
		case l := <-c.lines:
			c.add(&pending, l)
		case <-ticker.C:
			c.flush(&pending)
		case <-c.stop:
			c.drain(&pending)
			return
		}
	}
}

func (c *Client) drain(pending *batch) {
	for {
		select {
		case l := <-c.lines:
			c.add(pending, l)
		default:
			c.flush(pending)
			return
		}
	}
}

func (c *Client) add(pending *batch, l line) {
	pending.lines = append(pending.lines, l)
	pending.size += len(l.text)
	if pending.size >= maxBatchBytes {
		c.flush(pending)
	}
}

// flush reports failures on stderr because writing them to the logger would
// feed them back into this client.
func (c *Client) flush(pending *batch) {
	dropped := c.dropped.Swap(0)
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "promtail: dropped %d log lines because the buffer was full\n", dropped)
	}

	if len(pending.lines) == 0 {
		return
	}

	err := c.push(pending.lines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promtail: dropped %d log lines: %s\n", len(pending.lines), err)
	}

	pending.lines = pending.lines[:0]
	pending.size = 0
}

func (c *Client) push(lines []line) error {
	body, err := json.Marshal(c.request(lines))
	if err != nil {
		return ez.New(ez.EINTERNAL, "Could not encode the Loki push request", err)
	}

	for attempt := 1; ; attempt++ {
		err = c.post(body)
		if err == nil {
			return nil
		}

		if attempt == maxPushAttempts || ez.ErrorCode(err) != ez.EUNAVAILABLE {
			return ez.Wrap(err)
		}

		time.Sleep(time.Duration(attempt) * retryBackoff)
	}
}

func (c *Client) post(body []byte) error {
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return ez.New(ez.EINTERNAL, "Could not build the Loki push request", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ez.New(ez.EUNAVAILABLE, "Could not reach Loki", err)
	}
	defer resp.Body.Close()

	// Best effort: the body only adds detail to the error message
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	if resp.StatusCode < 300 {
		return nil
	}

	code := ez.EINVALID
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		code = ez.EUNAVAILABLE
	}

	msg := fmt.Sprintf("Loki rejected the push with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	return ez.New(code, msg, nil)
}

// request groups the lines into one stream per label set.
func (c *Client) request(lines []line) pushRequest {
	var req pushRequest
	streams := map[string]*stream{}

	for _, l := range lines {
		text := ansiRegex.ReplaceAllString(strings.TrimRight(string(l.text), "\n"), "")
		labels := c.labels(text)

		var key strings.Builder
		for _, name := range c.labelNames {
			key.WriteString(labels[name])
			key.WriteByte(0)
		}

		s, ok := streams[key.String()]
		if !ok {
			s = &stream{Stream: labels}
			streams[key.String()] = s
			req.Streams = append(req.Streams, s)
		}

		s.Values = append(s.Values, [2]string{strconv.FormatInt(l.at.UnixNano(), 10), text})
	}

	return req
}

// labels extracts the configured labels from the key=value tokens that
// zerolog's ConsoleWriter prints, then applies the static labels on top.
//
// The last token for a name wins: ConsoleWriter prints the message before the
// fields, so a token injected through the message cannot override the real
// field. The label is then dropped when that final value is outside
// labelValueRegex, so an invalid real value never falls back to an injected
// one. A name keeps at most maxLabelValues distinct values, later ones are
// dropped.
func (c *Client) labels(text string) map[string]string {
	labels := map[string]string{}

	for _, token := range strings.Split(text, " ") {
		parts := strings.Split(token, "=")
		if len(parts) != 2 {
			continue
		}

		if slices.Contains(c.labelNames, parts[0]) {
			labels[parts[0]] = parts[1]
		}
	}

	for name, value := range labels {
		if !labelValueRegex.MatchString(value) || !c.allowLabelValue(name, value) {
			delete(labels, name)
		}
	}

	maps.Copy(labels, c.staticLabels)

	return labels
}

// allowLabelValue records value for name and reports whether it fits within
// maxLabelValues distinct values. The first rejection per name goes to stderr.
func (c *Client) allowLabelValue(name, value string) bool {
	values, ok := c.seen[name]
	if !ok {
		values = map[string]struct{}{}
		c.seen[name] = values
	}

	_, ok = values[value]
	if ok {
		return true
	}

	if len(values) < maxLabelValues {
		values[value] = struct{}{}
		return true
	}

	if !c.capped[name] {
		c.capped[name] = true
		fmt.Fprintf(os.Stderr, "promtail: label %q exceeded %d distinct values, further values are dropped\n", name, maxLabelValues)
	}

	return false
}
