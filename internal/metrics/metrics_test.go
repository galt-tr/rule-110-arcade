package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistogramCountsAndSum(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("t_seconds", "test")

	h.Observe(5 * time.Millisecond)
	h.Observe(15 * time.Millisecond)
	h.Observe(2 * time.Second)

	if got := h.Count(); got != 3 {
		t.Errorf("Count() = %d, want 3", got)
	}
	// 0.005 + 0.015 + 2.0
	if got := h.Sum(); got < 2.019 || got > 2.021 {
		t.Errorf("Sum() = %v, want ~2.02", got)
	}
}

// A histogram that reported nothing until its first observation would make an
// idle phase indistinguishable from a phase that was never wired up.
func TestHistogramRendersBeforeAnyObservation(t *testing.T) {
	r := NewRegistry()
	r.Histogram("t_seconds", "test")

	out := render(t, r)
	if !strings.Contains(out, "t_seconds_count 0") {
		t.Errorf("an unobserved histogram must still render a zero count:\n%s", out)
	}
}

func TestHistogramRendersPrometheusText(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("t_seconds", "how long t took")
	h.Observe(3 * time.Millisecond)

	out := render(t, r)
	for _, want := range []string{
		"# HELP t_seconds how long t took",
		"# TYPE t_seconds histogram",
		`t_seconds_bucket{le="+Inf"} 1`,
		"t_seconds_count 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Buckets are cumulative: 3ms lands in every bucket at or above 0.005, and
	// in none below it.
	if !strings.Contains(out, `t_seconds_bucket{le="0.0025"} 0`) {
		t.Errorf("3ms must not be counted under the 2.5ms bound:\n%s", out)
	}
	if !strings.Contains(out, `t_seconds_bucket{le="0.005"} 1`) {
		t.Errorf("3ms must be counted under the 5ms bound:\n%s", out)
	}
}

// The quantile estimate is what the per-generation log line prints, so it has
// to be right about which bucket an observation fell in.
func TestQuantileEstimatesFromBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("t_seconds", "test")
	for range 99 {
		h.Observe(time.Millisecond)
	}
	h.Observe(5 * time.Second)

	// p50 sits in the 1ms bucket, whose upper bound is 0.001.
	if got := h.Quantile(0.5); got > 0.001 {
		t.Errorf("Quantile(0.5) = %v, want <= 0.001", got)
	}
	// p99.9 is dragged into the top bucket by the one slow observation.
	if got := h.Quantile(0.999); got < 1 {
		t.Errorf("Quantile(0.999) = %v, want >= 1", got)
	}
}

// The rank rounds UP, and binary floating point must not push a round
// percentile into the next bucket. 0.99*100 evaluates to 99.00000000000001, so
// a naive Ceil asks for 100 observations and reports the outlier as p99.
func TestQuantileRankRoundsUpWithoutFloatDrift(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("t_seconds", "test")
	for range 99 {
		h.Observe(time.Millisecond)
	}
	h.Observe(5 * time.Second)

	if got := h.Quantile(0.99); got != 0.001 {
		t.Errorf("Quantile(0.99) = %v, want 0.001 — 99 of 100 samples are 1ms", got)
	}
	if got := h.Quantile(1.0); got != 5 {
		t.Errorf("Quantile(1.0) = %v, want 5 — the max must be covered", got)
	}
}

func TestQuantileOfEmptyHistogramIsZero(t *testing.T) {
	r := NewRegistry()
	if got := r.Histogram("t_seconds", "test").Quantile(0.5); got != 0 {
		t.Errorf("Quantile of an empty histogram = %v, want 0", got)
	}
}

func TestCounter(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("t_total", "test")
	c.Add(2)
	c.Inc()

	if got := c.Value(); got != 3 {
		t.Errorf("Value() = %d, want 3", got)
	}
	out := render(t, r)
	for _, want := range []string{"# TYPE t_total counter", "t_total 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Re-requesting a name must return the same instrument. The engine wires these
// up from several call sites and two of them holding different counters would
// silently halve every number.
func TestSameNameReturnsSameInstrument(t *testing.T) {
	r := NewRegistry()
	if r.Histogram("t_seconds", "test") != r.Histogram("t_seconds", "test") {
		t.Error("Histogram returned a different instrument for the same name")
	}
	if r.Counter("t_total", "test") != r.Counter("t_total", "test") {
		t.Error("Counter returned a different instrument for the same name")
	}
}

// Every observation happens on a cell worker, and there are 128 of them.
func TestConcurrentObservation(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("t_seconds", "test")
	c := r.Counter("t_total", "test")

	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				h.Observe(time.Millisecond)
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if got := h.Count(); got != 12800 {
		t.Errorf("Count() = %d, want 12800", got)
	}
	if got := c.Value(); got != 12800 {
		t.Errorf("Value() = %d, want 12800", got)
	}
}

// Output order must not depend on map iteration, or every scrape diffs against
// the last one for no reason.
func TestRenderIsOrdered(t *testing.T) {
	r := NewRegistry()
	r.Counter("z_total", "z")
	r.Histogram("a_seconds", "a")
	r.Counter("m_total", "m")

	first := render(t, r)
	for range 5 {
		if got := render(t, r); got != first {
			t.Fatalf("render is not stable:\n%s\n---\n%s", first, got)
		}
	}
	// Registration order, not sorted order: the grouping an operator sees
	// should be the one the code declares.
	zi, ai := strings.Index(first, "z_total"), strings.Index(first, "a_seconds")
	if zi > ai {
		t.Errorf("expected registration order, got a_seconds before z_total:\n%s", first)
	}
}

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	r.Render(&b)
	return b.String()
}
