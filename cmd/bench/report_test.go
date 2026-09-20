package main

import (
	"strings"
	"testing"
	"time"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{7}, 7},
		{[]int64{3, 1, 2}, 2},
		{[]int64{4, 1, 3, 2}, 2}, // mean of the middle pair
	}
	for _, tc := range tests {
		if got := median(tc.in); got != tc.want {
			t.Errorf("median(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSpreadPct(t *testing.T) {
	s := &summary{name: legDirect, p99: []int64{100, 105, 110}}
	// (110-100)/105 = 9.52%: just inside the stability bar.
	if got := s.spreadPct(); got < 9.4 || got > 9.6 {
		t.Errorf("spreadPct = %.2f, want ~9.52", got)
	}
	if got := (&summary{p99: []int64{100}}).spreadPct(); got != 0 {
		t.Errorf("single run spreadPct = %.2f, want 0", got)
	}
}

func TestPrintSummaryReportsOverhead(t *testing.T) {
	direct := &summary{
		name: legDirect,
		p50:  []int64{int64(50 * time.Millisecond)},
		p99:  []int64{int64(52 * time.Millisecond)},
	}
	gateway := &summary{
		name: legGateway,
		p50:  []int64{int64(50*time.Millisecond + 20*time.Microsecond)},
		p99:  []int64{int64(52*time.Millisecond + 60*time.Microsecond)},
	}

	var b strings.Builder
	printSummary(&b, 1, []*summary{direct, gateway})
	out := b.String()

	if !strings.Contains(out, "gateway_overhead_p99 = 60.0µs") {
		t.Errorf("overhead line missing or wrong:\n%s", out)
	}
	if !strings.Contains(out, "gateway_overhead_p50 = 20.0µs") {
		t.Errorf("p50 overhead line missing or wrong:\n%s", out)
	}
}

func TestPrintSummaryFlagsABaselineOnlyRun(t *testing.T) {
	var b strings.Builder
	printSummary(&b, 3, []*summary{{name: legDirect, p50: []int64{1, 2, 3}, p99: []int64{10, 11, 12}}})
	if out := b.String(); !strings.Contains(out, "gateway leg not run") {
		t.Errorf("baseline-only run should say so:\n%s", out)
	}
}

func TestDurFormatsAtHistogramResolution(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{900 * time.Nanosecond, "900ns"},
		{1500 * time.Nanosecond, "1.5µs"},
		{52400 * time.Microsecond, "52.400ms"},
		{1500 * time.Millisecond, "1.50s"},
	}
	for _, tc := range tests {
		if got := dur(int64(tc.in)); got != tc.want {
			t.Errorf("dur(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
