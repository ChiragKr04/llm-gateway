package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// Options configures one leg of a load run.
type Options struct {
	// RPS is the offered load. The driver holds this rate open-loop: it does
	// not wait for a response before scheduling the next request.
	RPS float64
	// Duration is the measurement window.
	Duration time.Duration
	// Warmup runs load without recording it, so connection setup and the Go
	// runtime's first-touch costs stay out of the histogram.
	Warmup time.Duration
	// Concurrency is the number of in-flight requests the driver allows.
	Concurrency int
	// Timeout bounds a single request.
	Timeout time.Duration
	// Stream sends an SSE request and reads it to "data: [DONE]".
	Stream bool
	// Body is the request payload, sent verbatim on every request.
	Body []byte
	// APIKey, when set, is sent as a bearer token. Unused until M8.
	APIKey string
}

// Result is one leg's measurements. Latencies are nanoseconds.
type Result struct {
	Name string
	URL  string

	// Attempted counts every request the driver issued, warmup included;
	// Measured counts only those scheduled inside the measurement window.
	Attempted int64
	Measured  int64
	Succeeded int64
	// Failures counts failed requests by category. Failed requests are
	// excluded from the latency histograms: a connection refused in 30µs
	// would otherwise flatter the percentiles it has no business being in.
	Failures map[string]int64

	// Latency is the full service time: request write through last byte read.
	Latency *hdr.Histogram
	// TTFB is time to the first body byte. It is only meaningful on a
	// streaming leg: for a non-streaming one it is Latency by another name,
	// so Stream records which leg this was and the report omits it.
	TTFB   *hdr.Histogram
	Stream bool
	// Schedule is how late the driver dispatched each request against its
	// scheduled time. It is the saturation check: while this stays near zero
	// the driver is holding the offered rate and the latency numbers describe
	// the server. Once it grows, they describe the driver's own backlog.
	Schedule *hdr.Histogram

	Window      time.Duration
	Bytes       int64
	AllocsPerOp float64
	BytesPerOp  float64
}

// Saturated reports whether the driver fell far enough behind its own
// schedule that the latency numbers should not be trusted.
func (r *Result) Saturated() bool {
	return r.Schedule.ValueAtQuantile(99) > int64(2*time.Millisecond)
}

// AchievedRPS is the rate actually offered during the measurement window.
func (r *Result) AchievedRPS() float64 {
	if r.Window <= 0 {
		return 0
	}
	return float64(r.Measured) / r.Window.Seconds()
}

// SuccessRate is the fraction of measured requests that completed cleanly.
func (r *Result) SuccessRate() float64 {
	if r.Measured == 0 {
		return 0
	}
	return float64(r.Succeeded) / float64(r.Measured)
}

// newHistogram covers 1ns to 120s at three significant figures. The ceiling is
// above any plausible request under -timeout, and three figures resolve a
// microsecond difference at the millisecond scale — which is exactly the
// resolution the gateway overhead metric needs.
func newHistogram() *hdr.Histogram {
	return hdr.New(1, int64(120*time.Second), 3)
}

// newClient builds a transport sized for the run. Compression and HTTP/2 are
// off deliberately: the driver measures a gateway hop on loopback, and both
// would add work to the measurement that no production caller of a local
// gateway pays.
func newClient(o Options) *http.Client {
	conns := o.Concurrency * 2
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        conns,
			MaxIdleConnsPerHost: conns,
			MaxConnsPerHost:     conns,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
			ForceAttemptHTTP2:   false,
		},
	}
}

// Run drives one leg and returns its measurements. It returns early only if
// ctx is cancelled; a server that fails every request still produces a Result.
func Run(ctx context.Context, name, url string, o Options) (*Result, error) {
	if o.RPS <= 0 {
		return nil, errors.New("bench: -rps must be positive")
	}
	if o.Concurrency <= 0 {
		return nil, errors.New("bench: -concurrency must be positive")
	}
	if o.Duration <= 0 {
		return nil, errors.New("bench: -duration must be positive")
	}

	client := newClient(o)
	defer client.CloseIdleConnections()

	// Allocation accounting spans the whole run rather than just the
	// measurement window: ReadMemStats stops the world, and a stop-the-world
	// pause dropped into the middle of a latency measurement would land in the
	// tail percentiles being reported.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	measureFrom := start.Add(o.Warmup)
	end := measureFrom.Add(o.Duration)
	perRequest := float64(time.Second) / o.RPS

	workers := make([]*worker, o.Concurrency)
	for i := range workers {
		workers[i] = &worker{
			client:      client,
			url:         url,
			opts:        o,
			br:          bufio.NewReaderSize(nil, 8<<10),
			measureFrom: measureFrom,
			latency:     newHistogram(),
			ttfb:        newHistogram(),
			sched:       newHistogram(),
			fail:        make(map[string]int64, 8),
		}
	}

	// jobs carries each request's scheduled dispatch time. The buffer is one
	// slot per worker: any deeper and a backlog would hide inside the channel
	// instead of showing up in the schedule histogram.
	jobs := make(chan time.Time, o.Concurrency)

	var wg sync.WaitGroup
	wg.Add(len(workers))
	for _, w := range workers {
		go func(w *worker) {
			defer wg.Done()
			for scheduled := range jobs {
				w.do(ctx, scheduled)
			}
		}(w)
	}

	// One reusable timer for the whole schedule. time.After would allocate a
	// timer per request that survives until it fires, which at four-figure RPS
	// is allocation pressure the driver would then attribute to the server.
	pace := time.NewTimer(time.Hour)
	if !pace.Stop() {
		<-pace.C
	}
	defer pace.Stop()

dispatch:
	for i := 0; ; i++ {
		// Absolute scheduling from start, not repeated addition: accumulating
		// a rounded interval drifts the offered rate over a 30s run.
		next := start.Add(time.Duration(float64(i) * perRequest))
		if !next.Before(end) {
			break
		}
		if wait := time.Until(next); wait > 0 {
			pace.Reset(wait)
			select {
			case <-ctx.Done():
				if !pace.Stop() {
					<-pace.C
				}
				break dispatch
			case <-pace.C:
			}
		}
		select {
		case jobs <- next:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(measureFrom)

	runtime.ReadMemStats(&after)

	res := &Result{
		Name:     name,
		URL:      url,
		Failures: make(map[string]int64, 8),
		Latency:  newHistogram(),
		TTFB:     newHistogram(),
		Schedule: newHistogram(),
		Stream:   o.Stream,
		Window:   min(elapsed, o.Duration),
	}
	for _, w := range workers {
		res.Attempted += w.attempted
		res.Measured += w.measured
		res.Succeeded += w.succeeded
		res.Bytes += w.bytes
		res.Latency.Merge(w.latency)
		res.TTFB.Merge(w.ttfb)
		res.Schedule.Merge(w.sched)
		for k, v := range w.fail {
			res.Failures[k] += v
		}
	}
	if res.Attempted > 0 {
		res.AllocsPerOp = float64(after.Mallocs-before.Mallocs) / float64(res.Attempted)
		res.BytesPerOp = float64(after.TotalAlloc-before.TotalAlloc) / float64(res.Attempted)
	}
	return res, ctx.Err()
}

// worker issues requests serially. Each owns its histograms and counters so
// the hot path takes no locks; they are merged once the run ends.
type worker struct {
	client *http.Client
	url    string
	opts   Options
	br     *bufio.Reader

	measureFrom time.Time

	attempted int64
	measured  int64
	succeeded int64
	bytes     int64
	fail      map[string]int64

	latency *hdr.Histogram
	ttfb    *hdr.Histogram
	sched   *hdr.Histogram
}

var (
	dataPrefix = []byte("data: ")
	doneMarker = []byte("[DONE]")
)

func (w *worker) do(ctx context.Context, scheduled time.Time) {
	dispatched := time.Now()
	// A request scheduled before the window still runs — load must be
	// continuous across the warmup boundary — but is not recorded.
	measure := !dispatched.Before(w.measureFrom)

	w.attempted++
	if measure {
		w.measured++
		_ = w.sched.RecordValue(int64(dispatched.Sub(scheduled)))
	}

	reqCtx, cancel := context.WithTimeout(ctx, w.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.url, bytes.NewReader(w.opts.Body))
	if err != nil {
		w.record(measure, "build_request", 0, 0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if w.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.opts.APIKey)
	}
	if w.opts.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.record(measure, classifyErr(err), 0, 0)
		return
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		w.record(measure, statusClass(resp.StatusCode), 0, 0)
		return
	}

	var (
		firstByte time.Time
		n         int64
		readErr   error
	)
	if w.opts.Stream {
		n, firstByte, readErr = w.readStream(resp.Body)
	} else {
		n, readErr = io.Copy(io.Discard, resp.Body)
		firstByte = time.Now()
	}
	_ = resp.Body.Close()
	done := time.Now()

	if readErr != nil {
		w.record(measure, classifyErr(readErr), 0, 0)
		return
	}
	w.bytes += n
	w.record(measure, "", done.Sub(dispatched), firstByte.Sub(dispatched))
}

// readStream consumes an SSE body to its [DONE] marker, returning the bytes
// read and the instant the first body byte arrived.
func (w *worker) readStream(body io.Reader) (n int64, firstByte time.Time, err error) {
	w.br.Reset(body)

	var sawDone bool
	for {
		line, lerr := w.br.ReadSlice('\n')
		if firstByte.IsZero() && len(line) > 0 {
			firstByte = time.Now()
		}
		n += int64(len(line))

		// A line longer than the reader's buffer arrives in pieces. Only the
		// first piece carries the "data: " prefix, so the rest is skipped.
		for errors.Is(lerr, bufio.ErrBufferFull) {
			var more []byte
			more, lerr = w.br.ReadSlice('\n')
			n += int64(len(more))
		}

		if payload, ok := bytes.CutPrefix(line, dataPrefix); ok {
			if bytes.HasPrefix(payload, doneMarker) {
				sawDone = true
			}
		}

		if lerr != nil {
			if errors.Is(lerr, io.EOF) {
				break
			}
			return n, firstByte, lerr
		}
		if sawDone {
			break
		}
	}

	// Drain whatever follows [DONE] so the connection goes back to the idle
	// pool instead of being torn down and redialled on the next request.
	if _, derr := io.Copy(io.Discard, w.br); derr != nil {
		return n, firstByte, derr
	}
	if !sawDone {
		return n, firstByte, errStreamTruncated
	}
	return n, firstByte, nil
}

var errStreamTruncated = errors.New("stream ended without [DONE]")

// record files one completed request. kind is empty for a success.
func (w *worker) record(measure bool, kind string, latency, ttfb time.Duration) {
	if kind != "" {
		if measure {
			w.fail[kind]++
		}
		return
	}
	if !measure {
		return
	}
	w.succeeded++
	// RecordValue only errors above the histogram ceiling, which -timeout
	// keeps out of reach; a request that somehow exceeds 120s is not a
	// percentile worth preserving.
	_ = w.latency.RecordValue(int64(latency))
	_ = w.ttfb.RecordValue(int64(ttfb))
}

func statusClass(code int) string {
	switch {
	case code == http.StatusTooManyRequests:
		return "status_429"
	case code >= 500:
		return "status_5xx"
	case code >= 400:
		return "status_4xx"
	default:
		return fmt.Sprintf("status_%d", code)
	}
}

func classifyErr(err error) string {
	switch {
	case errors.Is(err, errStreamTruncated):
		return "stream_truncated"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "dial_" + opErr.Op
	}
	if strings.Contains(err.Error(), "EOF") {
		return "eof"
	}
	return "other"
}
