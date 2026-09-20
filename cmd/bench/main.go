// Command bench is the load driver for the gateway. It drives a fixed offered
// rate against one or two legs — direct to the provider and through the
// gateway — and reports the difference between them.
//
// Both legs run inside the same invocation, alternating run by run, so the
// machine's state at the moment of measurement is as close to identical as it
// can be made. Comparing two separately-launched runs would measure the
// laptop's mood as much as the gateway's cost.
//
// Run the mocker with -tps=0 for any overhead measurement: streaming pace
// injects one timer wait per token into both legs and buries the microseconds
// being measured under scheduling noise.
//
// Example:
//
//	go run ./cmd/bench -target http://127.0.0.1:8081 -rps 1000 -duration 30s -runs 3
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"
)

// Leg names. They are also the keys in the summary output, so tooling that
// scrapes the output can rely on them.
const (
	legDirect  = "direct"
	legGateway = "gateway"
)

const chatCompletionsPath = "/v1/chat/completions"

func main() {
	var (
		target      = flag.String("target", "", "base URL of the direct leg, e.g. http://127.0.0.1:8081 (required)")
		gateway     = flag.String("gateway", "", "base URL of the gateway leg; when set, both legs run and the overhead delta is printed")
		rps         = flag.Float64("rps", 1000, "offered load in requests per second, per leg")
		duration    = flag.Duration("duration", 30*time.Second, "measurement window per leg per run")
		warmup      = flag.Duration("warmup", 3*time.Second, "unrecorded load before the measurement window")
		cooldown    = flag.Duration("cooldown", 2*time.Second, "pause between legs so sockets and GC settle")
		runs        = flag.Int("runs", 1, "repeat the whole comparison this many times and report the median")
		concurrency = flag.Int("concurrency", 256, "maximum in-flight requests per leg")
		stream      = flag.Bool("stream", false, "send stream=true and read the SSE response to [DONE]")
		model       = flag.String("model", "mock-small", "model ID sent in the request body")
		promptWords = flag.Int("prompt-words", 64, "length of the generated user prompt, in words")
		maxTokens   = flag.Int("max-tokens", 0, "max_tokens sent with the request (0 omits the field)")
		timeout     = flag.Duration("timeout", 30*time.Second, "per-request timeout")
		apiKey      = flag.String("api-key", "", "bearer token sent with each request (unused until M8)")
		asJSON      = flag.Bool("json", false, "emit machine-readable JSON instead of the human report")
		cpuProfile  = flag.String("cpuprofile", "", "write a CPU profile of the driver to this path")
		memProfile  = flag.String("memprofile", "", "write an allocation profile of the driver to this path")
	)
	flag.Parse()

	if *target == "" {
		fmt.Fprintln(os.Stderr, "bench: -target is required")
		flag.Usage()
		os.Exit(2)
	}
	if *runs < 1 {
		fmt.Fprintln(os.Stderr, "bench: -runs must be at least 1")
		os.Exit(2)
	}

	directURL, err := resolveURL(*target)
	if err != nil {
		fprintf(os.Stderr, "bench: -target: %v\n", err)
		os.Exit(2)
	}
	gatewayURL := ""
	if *gateway != "" {
		if gatewayURL, err = resolveURL(*gateway); err != nil {
			fprintf(os.Stderr, "bench: -gateway: %v\n", err)
			os.Exit(2)
		}
	}

	body, err := buildBody(*model, *promptWords, *maxTokens, *stream)
	if err != nil {
		fprintf(os.Stderr, "bench: build request body: %v\n", err)
		os.Exit(1)
	}

	opts := Options{
		RPS:         *rps,
		Duration:    *duration,
		Warmup:      *warmup,
		Concurrency: *concurrency,
		Timeout:     *timeout,
		Stream:      *stream,
		Body:        body,
		APIKey:      *apiKey,
	}

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			fprintf(os.Stderr, "bench: cpuprofile: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = f.Close() }()
		if err := pprof.StartCPUProfile(f); err != nil {
			fprintf(os.Stderr, "bench: cpuprofile: %v\n", err)
			os.Exit(1)
		}
		defer pprof.StopCPUProfile()
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	legs := []struct{ name, url string }{{legDirect, directURL}}
	if gatewayURL != "" {
		legs = append(legs, struct{ name, url string }{legGateway, gatewayURL})
	}

	out := os.Stdout
	if !*asJSON {
		fprintf(out, "bench: %s, %.0f RPS, %s window, %s warmup, %d concurrency, %d run(s)\n",
			modeName(*stream), *rps, *duration, *warmup, *concurrency, *runs)
		fprintf(out, "bench: %s\n", envLine())
	}

	summaries := make([]*summary, 0, len(legs))
	byName := make(map[string]*summary, len(legs))
	for _, l := range legs {
		s := newSummary(l.name)
		summaries = append(summaries, s)
		byName[l.name] = s
	}

	var all [][]*Result
	for run := 1; run <= *runs; run++ {
		if !*asJSON {
			fprintf(out, "\n--- run %d/%d ---\n", run, *runs)
		}
		results := make([]*Result, 0, len(legs))
		for i, l := range legs {
			if i > 0 || run > 1 {
				sleepCtx(ctx, *cooldown)
			}
			if ctx.Err() != nil {
				break
			}
			r, err := Run(ctx, l.name, l.url, opts)
			if err != nil && r == nil {
				fprintf(os.Stderr, "bench: %s leg: %v\n", l.name, err)
				os.Exit(1)
			}
			results = append(results, r)
			byName[l.name].add(r)
			if !*asJSON {
				printResult(out, r)
			}
		}
		all = append(all, results)
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "bench: interrupted")
			break
		}
	}

	if *asJSON {
		if err := writeJSONReport(out, opts, all, summaries); err != nil {
			fprintf(os.Stderr, "bench: encode report: %v\n", err)
			os.Exit(1)
		}
	} else {
		printSummary(out, len(all), summaries)
	}

	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			fprintf(os.Stderr, "bench: memprofile: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = f.Close() }()
		runtime.GC()
		if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
			fprintf(os.Stderr, "bench: memprofile: %v\n", err)
			os.Exit(1)
		}
	}
}

func modeName(stream bool) string {
	if stream {
		return "streaming"
	}
	return "non-streaming"
}

func envLine() string {
	return fmt.Sprintf("%s %s/%s, %d CPU", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// resolveURL accepts either a base URL or a full endpoint URL, so
// "-target http://127.0.0.1:8081" and "-target http://127.0.0.1:8081/v1/chat/completions"
// both work.
func resolveURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q has no host", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = chatCompletionsPath
	}
	return u.String(), nil
}

// ---------------------------------------------------------------------------
// Request body
// ---------------------------------------------------------------------------

type benchRequest struct {
	Model         string          `json:"model"`
	Messages      []benchMessage  `json:"messages"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *benchStreamOpt `json:"stream_options,omitempty"`
}

type benchMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type benchStreamOpt struct {
	IncludeUsage bool `json:"include_usage"`
}

// buildBody produces one payload reused for every request in the run. Every
// request carrying identical bytes is deliberate: it keeps the body out of the
// variables, so a difference between legs is the gateway and not the prompt.
func buildBody(model string, promptWords, maxTokens int, stream bool) ([]byte, error) {
	req := benchRequest{
		Model:    model,
		Messages: []benchMessage{{Role: "user", Content: prompt(promptWords)}},
		Stream:   stream,
	}
	if maxTokens > 0 {
		req.MaxTokens = &maxTokens
	}
	if stream {
		// Asking for usage explicitly means the streaming leg pays the cost of
		// the terminal usage chunk, which is what the gateway will accumulate
		// from M4 onward.
		req.StreamOptions = &benchStreamOpt{IncludeUsage: true}
	}
	return json.Marshal(req)
}

var promptVocabulary = strings.Fields(
	"explain the tradeoffs between reserving capacity ahead of a request and " +
		"settling it afterwards when the client may disconnect at any point")

func prompt(words int) string {
	if words <= 0 {
		words = 1
	}
	var b strings.Builder
	for i := range words {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(promptVocabulary[i%len(promptVocabulary)])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// JSON report
// ---------------------------------------------------------------------------

type jsonReport struct {
	Config  jsonConfig      `json:"config"`
	Runs    [][]jsonLeg     `json:"runs"`
	Summary []jsonLegMedian `json:"summary"`
	// OverheadP99Ns is present only when both legs ran.
	OverheadP50Ns *int64 `json:"overhead_p50_ns,omitempty"`
	OverheadP99Ns *int64 `json:"overhead_p99_ns,omitempty"`
}

type jsonConfig struct {
	RPS         float64 `json:"rps"`
	DurationMs  int64   `json:"duration_ms"`
	WarmupMs    int64   `json:"warmup_ms"`
	Concurrency int     `json:"concurrency"`
	Stream      bool    `json:"stream"`
	Go          string  `json:"go"`
	CPUs        int     `json:"cpus"`
}

type jsonLeg struct {
	Name        string           `json:"name"`
	URL         string           `json:"url"`
	Measured    int64            `json:"measured"`
	Succeeded   int64            `json:"succeeded"`
	Failures    map[string]int64 `json:"failures,omitempty"`
	AchievedRPS float64          `json:"achieved_rps"`
	Latency     map[string]int64 `json:"latency_ns"`
	TTFB        map[string]int64 `json:"ttfb_ns,omitempty"`
	ScheduleP99 int64            `json:"schedule_delay_p99_ns"`
	Saturated   bool             `json:"saturated"`
	AllocsPerOp float64          `json:"driver_allocs_per_op"`
}

type jsonLegMedian struct {
	Name      string  `json:"name"`
	P50Ns     int64   `json:"p50_ns"`
	P99Ns     int64   `json:"p99_ns"`
	SpreadPct float64 `json:"p99_spread_pct"`
}

func writeJSONReport(w *os.File, o Options, all [][]*Result, legs []*summary) error {
	rep := jsonReport{
		Config: jsonConfig{
			RPS:         o.RPS,
			DurationMs:  o.Duration.Milliseconds(),
			WarmupMs:    o.Warmup.Milliseconds(),
			Concurrency: o.Concurrency,
			Stream:      o.Stream,
			Go:          runtime.Version(),
			CPUs:        runtime.NumCPU(),
		},
	}
	for _, run := range all {
		out := make([]jsonLeg, 0, len(run))
		for _, r := range run {
			leg := jsonLeg{
				Name:        r.Name,
				URL:         r.URL,
				Measured:    r.Measured,
				Succeeded:   r.Succeeded,
				Failures:    r.Failures,
				AchievedRPS: r.AchievedRPS(),
				Latency:     percentileMap(r.Latency),
				ScheduleP99: r.Schedule.ValueAtQuantile(99),
				Saturated:   r.Saturated(),
				AllocsPerOp: r.AllocsPerOp,
			}
			if o.Stream {
				leg.TTFB = percentileMap(r.TTFB)
			}
			out = append(out, leg)
		}
		rep.Runs = append(rep.Runs, out)
	}
	for _, s := range legs {
		rep.Summary = append(rep.Summary, jsonLegMedian{
			Name:      s.name,
			P50Ns:     s.medianP50(),
			P99Ns:     s.medianP99(),
			SpreadPct: s.spreadPct(),
		})
	}
	if d, g := findLeg(legs, legDirect), findLeg(legs, legGateway); d != nil && g != nil {
		p50, p99 := g.medianP50()-d.medianP50(), g.medianP99()-d.medianP99()
		rep.OverheadP50Ns, rep.OverheadP99Ns = &p50, &p99
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func percentileMap(h interface {
	ValueAtQuantile(float64) int64
	Max() int64
},
) map[string]int64 {
	m := make(map[string]int64, len(quantiles)+1)
	for _, q := range quantiles {
		m["p"+trimQuantile(q)] = h.ValueAtQuantile(q)
	}
	m["max"] = h.Max()
	return m
}
