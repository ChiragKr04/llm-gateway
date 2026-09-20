package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// fprintf writes one line of the report. The error is discarded on purpose:
// the destination is stdout or a strings.Builder, and threading a write
// failure through every line of a benchmark report buys nothing.
func fprintf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// quantiles reported for every leg. p999 is included because the gateway's
// own tail — lock contention, GC assists — hides there rather than at p99.
var quantiles = []float64{50, 90, 99, 99.9}

// printResult writes one leg's measurements in the shape BENCHMARKS.md wants.
func printResult(w io.Writer, r *Result) {
	fprintf(w, "  leg %-8s %s\n", r.Name, r.URL)
	fprintf(w, "    requests    %d measured (%d issued incl. warmup), %.1f RPS achieved\n",
		r.Measured, r.Attempted, r.AchievedRPS())
	fprintf(w, "    success     %.2f%% (%d ok", 100*r.SuccessRate(), r.Succeeded)
	if len(r.Failures) > 0 {
		fprintf(w, ", %s", formatFailures(r.Failures))
	}
	fprintf(w, ")\n")
	fprintf(w, "    latency     %s\n", formatPercentiles(r.Latency))
	if r.Stream && r.TTFB.TotalCount() > 0 {
		fprintf(w, "    ttfb        %s\n", formatPercentiles(r.TTFB))
	}
	fprintf(w, "    sched delay %s\n", formatPercentiles(r.Schedule))
	fprintf(w, "    driver cost %.1f allocs/op, %s/op (the driver's own, not the server's)\n",
		r.AllocsPerOp, formatBytes(r.BytesPerOp))
	if r.Saturated() {
		fprintf(w, "    WARNING     driver is saturated (sched delay p99 %s): raise -concurrency or lower -rps.\n",
			dur(r.Schedule.ValueAtQuantile(99)))
		fprintf(w, "                Latency below describes the driver's backlog, not the server.\n")
	}
}

func formatPercentiles(h interface {
	ValueAtQuantile(float64) int64
	Max() int64
	TotalCount() int64
},
) string {
	if h.TotalCount() == 0 {
		return "no samples"
	}
	var b strings.Builder
	for _, q := range quantiles {
		fprintf(&b, "p%-5s %-10s", trimQuantile(q), dur(h.ValueAtQuantile(q)))
	}
	fprintf(&b, "max %s", dur(h.Max()))
	return b.String()
}

func trimQuantile(q float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", q), "0"), ".")
}

func formatFailures(f map[string]int64) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", f[k], k))
	}
	return strings.Join(parts, ", ")
}

// dur renders a nanosecond count at a fixed three significant figures, which
// is the histogram's own resolution — printing more would imply precision the
// measurement does not have.
func dur(ns int64) string {
	switch d := time.Duration(ns); {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%dns", ns)
	}
}

func formatBytes(b float64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fkB", b/(1<<10))
	default:
		return fmt.Sprintf("%.0fB", b)
	}
}

// ---------------------------------------------------------------------------
// Across-run summary
// ---------------------------------------------------------------------------

// summary aggregates the same leg across repeated runs. The benchmark protocol
// reports the median of three, not a single run, because a single run on a
// laptop is a measurement of what else the laptop was doing.
type summary struct {
	name    string
	p50     []int64
	p99     []int64
	success []float64
}

func newSummary(name string) *summary { return &summary{name: name} }

func (s *summary) add(r *Result) {
	s.p50 = append(s.p50, r.Latency.ValueAtQuantile(50))
	s.p99 = append(s.p99, r.Latency.ValueAtQuantile(99))
	s.success = append(s.success, r.SuccessRate())
}

// medianP99 is the headline number for the leg.
func (s *summary) medianP99() int64 { return median(s.p99) }
func (s *summary) medianP50() int64 { return median(s.p50) }

// spreadPct is (max-min)/median over the runs' p99s. The M2 acceptance bar is
// under 10%: above that, the baseline is not stable enough to detect the
// microsecond-scale overhead later milestones add.
func (s *summary) spreadPct() float64 {
	if len(s.p99) < 2 {
		return 0
	}
	sorted := append([]int64(nil), s.p99...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	med := median(s.p99)
	if med == 0 {
		return 0
	}
	return 100 * float64(sorted[len(sorted)-1]-sorted[0]) / float64(med)
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]int64(nil), v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

const stabilityBarPct = 10.0

// printSummary reports the medians, the run-to-run spread, and — when both
// legs ran — the gateway overhead, which is the whole point of the harness.
func printSummary(w io.Writer, runs int, legs []*summary) {
	fprintf(w, "\n=== summary over %d run(s) ===\n", runs)
	for _, s := range legs {
		fprintf(w, "  %-8s median p50 %-10s median p99 %-10s", s.name, dur(s.medianP50()), dur(s.medianP99()))
		if runs > 1 {
			verdict := "PASS"
			if s.spreadPct() > stabilityBarPct {
				verdict = "FAIL"
			}
			fprintf(w, "  p99 spread %.1f%% (<%.0f%% %s)", s.spreadPct(), stabilityBarPct, verdict)
		}
		fprintf(w, "\n")
		if runs > 1 {
			fprintf(w, "           per-run p99: %s\n", joinDurations(s.p99))
		}
	}

	direct, gateway := findLeg(legs, legDirect), findLeg(legs, legGateway)
	if direct == nil || gateway == nil {
		fprintf(w, "\n  gateway leg not run: baseline only. Pass -gateway to get the overhead delta.\n")
		return
	}
	// The primary metric. Taken between medians of p99 rather than between two
	// single runs, so one noisy run cannot invent or erase microseconds.
	fprintf(w, "\n  gateway_overhead_p50 = %s\n", dur(gateway.medianP50()-direct.medianP50()))
	fprintf(w, "  gateway_overhead_p99 = %s   <-- primary metric (%s - %s)\n",
		dur(gateway.medianP99()-direct.medianP99()), dur(gateway.medianP99()), dur(direct.medianP99()))

	// An overhead smaller than the run-to-run noise is not a measurement.
	noise := int64(float64(direct.medianP99()) * max(direct.spreadPct(), gateway.spreadPct()) / 100)
	if overhead := gateway.medianP99() - direct.medianP99(); overhead < noise {
		fprintf(w, "  NOTE: overhead (%s) is below run-to-run noise (%s). Treat it as \"under the noise floor\",\n",
			dur(overhead), dur(noise))
		fprintf(w, "        not as a measured value. Lower the mocker's -latency to shrink the noise.\n")
	}
}

func joinDurations(v []int64) string {
	parts := make([]string, len(v))
	for i, ns := range v {
		parts[i] = dur(ns)
	}
	return strings.Join(parts, ", ")
}

func findLeg(legs []*summary, name string) *summary {
	for _, s := range legs {
		if s.name == name {
			return s
		}
	}
	return nil
}
