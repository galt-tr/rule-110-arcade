// Package metrics is the small amount of measurement this application needs to
// answer "where did the second go?".
//
// It is hand-rolled rather than pulled from a client library, for the same
// reason the /metrics handler is: the whole surface is a few counters and a few
// latency histograms rendered as Prometheus text, and a dependency that brings
// its own registry, collector interfaces and HTTP handler would be more code to
// understand than the thing it replaces.
//
// What it is NOT is a general-purpose library. There are no labels, because
// nothing here needs to slice by one and labels are where a metrics package
// stops being small. A per-cell breakdown belongs in a profile, not in a
// cardinality-128 gauge scraped every fifteen seconds.
//
// Every instrument is safe to use from any goroutine, which is not optional:
// observations come from 128 cell workers, the monitor's applier goroutines and
// the clock, all at once.
package metrics

import (
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// buckets are the histogram's upper bounds, in seconds.
//
// They span 100µs to 30s because that is the range the questions live in: a
// cached script build should land near the bottom, an arcade round trip in the
// middle, and anything in the top two buckets is a stall worth a name. Roughly
// 2.5x apart, which puts the quantile estimate's error well inside what a
// decision here would turn on.
var buckets = []float64{
	0.0001, 0.00025, 0.0005,
	0.001, 0.0025, 0.005,
	0.01, 0.025, 0.05,
	0.1, 0.25, 0.5,
	1, 2.5, 5,
	10, 30,
}

// Histogram is a latency distribution.
//
// The sum is kept in nanoseconds as an integer rather than as a float, because
// there is no atomic float and a mutex around every observation would put a
// contended lock on the hot path this exists to measure.
type Histogram struct {
	name, help string
	counts     []atomic.Uint64 // one per bucket, plus one for +Inf
	nanos      atomic.Uint64
	total      atomic.Uint64
}

// Observe records one duration.
func (h *Histogram) Observe(d time.Duration) {
	if d < 0 {
		// A negative duration means a clock that went backwards, and adding it
		// would corrupt the sum for the life of the process.
		d = 0
	}
	h.nanos.Add(uint64(d.Nanoseconds()))
	h.total.Add(1)

	secs := d.Seconds()
	// Linear scan: seventeen comparisons against a slice that is in cache
	// beats a binary search's branch misprediction at this size.
	for i, b := range buckets {
		if secs <= b {
			h.counts[i].Add(1)
			return
		}
	}
	h.counts[len(buckets)].Add(1)
}

// Time observes the duration since start. Its shape suits a defer.
func (h *Histogram) Time(start time.Time) { h.Observe(time.Since(start)) }

// Count is how many observations have been recorded.
func (h *Histogram) Count() uint64 { return h.total.Load() }

// Sum is the total observed time, in seconds.
func (h *Histogram) Sum() float64 { return float64(h.nanos.Load()) / 1e9 }

// Mean is the average observation in seconds, or 0 if there are none.
func (h *Histogram) Mean() float64 {
	n := h.total.Load()
	if n == 0 {
		return 0
	}
	return h.Sum() / float64(n)
}

// Quantile estimates the qth quantile in seconds, reported as the upper bound
// of the bucket it falls in.
//
// Deliberately an over-estimate rather than an interpolation. The number is
// read to answer "is this phase costing us milliseconds or hundreds of
// milliseconds", and a bucket bound is an honest answer to that; interpolating
// inside a bucket would invent precision the buckets do not carry. An
// observation past the last bound reports that bound, so a reported 30 means
// "at least 30".
func (h *Histogram) Quantile(q float64) float64 {
	n := h.total.Load()
	if n == 0 {
		return 0
	}
	// The rank is the SMALLEST count covering q of the observations, so it
	// rounds up: truncating makes Quantile(0.999) over 100 samples report the
	// 99th, which is the one figure a tail quantile exists to exclude.
	//
	// The epsilon is for binary floating point, not for the statistics. 0.99 x
	// 100 evaluates to 99.00000000000001, and a bare Ceil of that asks for 100
	// observations rather than 99 — turning every round percentile into the
	// next one up.
	want := uint64(math.Ceil(q*float64(n) - 1e-9))
	if want == 0 {
		want = 1
	}

	var seen uint64
	for i, b := range buckets {
		seen += h.counts[i].Load()
		if seen >= want {
			return b
		}
	}
	return buckets[len(buckets)-1]
}

func (h *Histogram) writeTo(b *strings.Builder) {
	b.WriteString("# HELP " + h.name + " " + h.help + "\n")
	b.WriteString("# TYPE " + h.name + " histogram\n")

	// Prometheus histogram buckets are CUMULATIVE, so each line carries every
	// observation at or below its bound.
	var seen uint64
	for i, bound := range buckets {
		seen += h.counts[i].Load()
		b.WriteString(h.name + `_bucket{le="` +
			strconv.FormatFloat(bound, 'g', -1, 64) + `"} ` +
			strconv.FormatUint(seen, 10) + "\n")
	}
	b.WriteString(h.name + `_bucket{le="+Inf"} ` +
		strconv.FormatUint(h.total.Load(), 10) + "\n")
	b.WriteString(h.name + "_sum " + strconv.FormatFloat(h.Sum(), 'g', -1, 64) + "\n")
	b.WriteString(h.name + "_count " + strconv.FormatUint(h.total.Load(), 10) + "\n")
}

// Counter is a monotonically increasing total.
type Counter struct {
	name, help string
	v          atomic.Uint64
}

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n.
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Value is the current total.
func (c *Counter) Value() uint64 { return c.v.Load() }

func (c *Counter) writeTo(b *strings.Builder) {
	b.WriteString("# HELP " + c.name + " " + c.help + "\n")
	b.WriteString("# TYPE " + c.name + " counter\n")
	b.WriteString(c.name + " " + strconv.FormatUint(c.v.Load(), 10) + "\n")
}

type collector interface{ writeTo(*strings.Builder) }

// Registry holds the instruments and renders them.
//
// Instruments are returned by name, so a caller that asks twice gets the same
// one — the engine wires several of these from different files, and two call
// sites holding different counters for one name would silently halve every
// figure.
type Registry struct {
	mu     sync.Mutex
	order  []collector
	byName map[string]collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]collector{}}
}

// Histogram returns the named histogram, creating it on first use.
func (r *Registry) Histogram(name, help string) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.byName[name]; ok {
		if h, ok := c.(*Histogram); ok {
			return h
		}
		panic("metrics: " + name + " is already registered as a different type")
	}
	h := &Histogram{name: name, help: help, counts: make([]atomic.Uint64, len(buckets)+1)}
	r.byName[name] = h
	r.order = append(r.order, h)
	return h
}

// Counter returns the named counter, creating it on first use.
func (r *Registry) Counter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.byName[name]; ok {
		if ctr, ok := c.(*Counter); ok {
			return ctr
		}
		panic("metrics: " + name + " is already registered as a different type")
	}
	ctr := &Counter{name: name, help: help}
	r.byName[name] = ctr
	r.order = append(r.order, ctr)
	return ctr
}

// Render writes every instrument as Prometheus text, in registration order.
//
// Ordered so that a scrape diffs cleanly against the last one, and so the
// grouping an operator reads is the one the code declares rather than whatever
// the map handed back.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	order := make([]collector, len(r.order))
	copy(order, r.order)
	r.mu.Unlock()

	var b strings.Builder
	for _, c := range order {
		c.writeTo(&b)
	}
	_, _ = io.WriteString(w, b.String())
}
