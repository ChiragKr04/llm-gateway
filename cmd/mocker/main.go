// Command mocker impersonates an OpenAI-compatible LLM provider with
// controllable latency, throughput and failure behaviour. It exists so the
// gateway's overhead can be measured against a provider whose own timing is a
// known constant rather than a network variable.
//
// Example:
//
//	go run ./cmd/mocker -port 8081 -latency 50ms -jitter 10ms -tokens 200 -tps 50
//	curl -N http://localhost:8081/v1/chat/completions \
//	  -H 'Content-Type: application/json' \
//	  -d '{"model":"mock-small","stream":true,"messages":[{"role":"user","content":"hi"}]}'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	var (
		port     = flag.Int("port", 8081, "port to listen on")
		latency  = flag.Duration("latency", 50*time.Millisecond, "delay before the first byte of a response")
		jitter   = flag.Duration("jitter", 0, "uniform +/- jitter applied to -latency")
		tokens   = flag.Int("tokens", 200, "completion tokens generated per request")
		tps      = flag.Float64("tps", 50, "streaming pace in tokens per second (0 = unpaced)")
		failRate = flag.Float64("fail-rate", 0, "probability in [0,1] of rejecting a request with 429 or 500")
		models   = flag.String("models", "mock-small,mock-large", "comma-separated model IDs served by /v1/models")
	)
	flag.Parse()

	if *failRate < 0 || *failRate > 1 {
		fmt.Fprintln(os.Stderr, "mocker: -fail-rate must be between 0 and 1")
		os.Exit(2)
	}
	if *tokens < 0 {
		fmt.Fprintln(os.Stderr, "mocker: -tokens must not be negative")
		os.Exit(2)
	}

	srv := NewServer(Config{
		Latency:  *latency,
		Jitter:   *jitter,
		Tokens:   *tokens,
		TPS:      *tps,
		FailRate: *failRate,
		Models:   splitModels(*models),
	})

	addr := fmt.Sprintf(":%d", *port)
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// No WriteTimeout: a paced stream is legitimately long-lived.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("mocker: listen: %v", err)
	}

	log.Printf("mocker: listening on %s (latency=%s jitter=%s tokens=%d tps=%g fail-rate=%g)",
		ln.Addr(), *latency, *jitter, *tokens, *tps, *failRate)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("mocker: serve: %v", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("mocker: shutdown: %v", err)
	}
	log.Print("mocker: stopped")
}

func splitModels(s string) []string {
	var out []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}
