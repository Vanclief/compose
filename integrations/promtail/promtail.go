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
	pushPath           = "/loki/api/v1/push"
	flushInterval      = time.Second
	maxBatchBytes      = 1 << 20
	bufferedLines      = 10000
	maxPushAttempts    = 3
	retryBackoff       = 250 * time.Millisecond
	maxLabelValues     = 100
	labelResetInterval = time.Hour
)

// ansiRegex matches ANSI escape sequences. request strips them from a copy of
// each line for label extraction only, the pushed line keeps its colors.
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
	// maxLabelValues per labelResetInterval to bound stream cardinality, and
	// capped records the names already reported as over the cap in the current
	// interval. Both are cleared every interval and only touched from run.
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

	labelReset := time.NewTicker(labelResetInterval)
	defer labelReset.Stop()

	var pending batch
	for {
		select {
		case l := <-c.lines:
			c.add(&pending, l)
		case <-ticker.C:
			c.flush(&pending)
		case <-labelReset.C:
			c.resetLabelValues()
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

// request groups the lines into one stream per label set. Labels are read from
// a copy of the line with the ANSI codes stripped, and the line is pushed with
// its colors, which Grafana renders.
func (c *Client) request(lines []line) pushRequest {
	var req pushRequest
	streams := map[string]*stream{}

	for _, l := range lines {
		raw := strings.TrimRight(string(l.text), "\n")
		labels := c.labels(ansiRegex.ReplaceAllString(raw, ""))

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

		s.Values = append(s.Values, [2]string{strconv.FormatInt(l.at.UnixNano(), 10), raw})
	}

	return req
}

// labels extracts the configured labels from the name=value fields that
// zerolog's ConsoleWriter prints, then applies the static labels on top.
//
// ConsoleWriter prints the fields last, after the message, each as name=value
// with one space between them. A string value is printed bare unless it holds
// a space, a quote, a backslash or a byte outside printable ASCII, in which
// case it is printed by strconv.Quote; any other value is a JSON number,
// true, false, null, object or array. labels reads the fields from the end of
// the line backwards with lastField and stops at the first text that is not a
// whole field, so it never reads a field out of another field's value, and
// the message, which is free text, can only be reached once every real field
// has been read. The first value read for a name wins, so a token in the
// message cannot override the real field. The label is then dropped when that
// value is outside labelValueRegex, so an invalid real value never falls back
// to an injected one. A name keeps at most maxLabelValues distinct values per
// labelResetInterval, later ones are dropped until the next reset.
//
// Text the writer prints after the fields, such as a FormatExtra, or a field
// name holding a space ends the scan there, so the fields before it give no
// labels.
func (c *Client) labels(text string) map[string]string {
	labels := map[string]string{}

	for text != "" {
		name, value, start, ok := lastField(text)
		if !ok {
			break
		}

		_, seen := labels[name]
		if !seen && slices.Contains(c.labelNames, name) {
			labels[name] = value
		}

		// start is 0 or the field follows a space
		text = text[:max(start-1, 0)]
	}

	for name, value := range labels {
		if !labelValueRegex.MatchString(value) || !c.allowLabelValue(name, value) {
			delete(labels, name)
		}
	}

	maps.Copy(labels, c.staticLabels)

	return labels
}

// lastField reads the field that ends text, as ConsoleWriter prints it, and
// returns its name, its value, the index the field starts at, which is 0 or
// follows a space, and false when text does not end with a whole field.
//
// A last token with no quote or backslash after its first "=", or anywhere
// when it has no "=", ends in a bare value, which holds no space, so the token
// is the whole field and the name ends at that "=". The end of a quoted
// string or of a JSON value holding one always has a quote after the token's
// first "=", since a string that holds an "=" closes after it. Otherwise a
// value ending in a quote is a quoted string and one ending in } or ] a JSON
// object or array, read back to its opening quote or bracket, and the name
// runs from the previous space to the "=" before it. Such a value is returned
// with its quotes or brackets, which labelValueRegex refuses.
func lastField(text string) (string, string, int, bool) {
	tokenStart := strings.LastIndexByte(text, ' ') + 1
	token := text[tokenStart:]

	name, value, found := strings.Cut(token, "=")
	end := token
	if found {
		end = value
	}

	if !strings.ContainsAny(end, `"\`) {
		if !found || name == "" {
			return "", "", 0, false
		}
		return name, value, tokenStart, true
	}

	var valueStart int
	switch text[len(text)-1] {
	case '"':
		valueStart = openingQuote(text, len(text)-1)
	case '}', ']':
		valueStart = openingBracket(text)
	default:
		return "", "", 0, false
	}

	if valueStart < 1 || text[valueStart-1] != '=' {
		return "", "", 0, false
	}

	start := strings.LastIndexByte(text[:valueStart-1], ' ') + 1
	name = text[start : valueStart-1]
	if name == "" {
		return "", "", 0, false
	}

	return name, text[valueStart:], start, true
}

// openingQuote returns the index of the quote that opens the string whose
// closing quote is text[closing], or -1. In strconv.Quote and JSON output a
// backslash always starts an escape, so a quote is escaped exactly when an odd
// number of backslashes precede it.
func openingQuote(text string, closing int) int {
	for i := closing - 1; i >= 0; i-- {
		if text[i] != '"' {
			continue
		}

		backslashes := 0
		for j := i - 1; j >= 0 && text[j] == '\\'; j-- {
			backslashes++
		}

		if backslashes%2 == 0 {
			return i
		}
	}

	return -1
}

// openingBracket returns the index of the { or [ that opens the JSON object or
// array ending text, or -1. Brackets inside JSON strings are skipped.
func openingBracket(text string) int {
	depth := 0
	for i := len(text) - 1; i >= 0; i-- {
		switch text[i] {
		case '}', ']':
			depth++
		case '{', '[':
			depth--
			if depth == 0 {
				return i
			}
		case '"':
			// Jump to the string's opening quote, the loop then steps past it
			i = openingQuote(text, i)
			if i < 0 {
				return -1
			}
		}
	}

	return -1
}

// allowLabelValue records value for name and reports whether it fits within
// maxLabelValues distinct values. The first rejection per name and interval
// goes to stderr.
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

// resetLabelValues starts a new cardinality window, so an early burst of values
// cannot remove a label for the rest of the process.
func (c *Client) resetLabelValues() {
	clear(c.seen)
	clear(c.capped)
}
