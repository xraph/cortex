package telemetry

import (
	"strings"
	"sync"

	gometrics "github.com/xraph/go-utils/metrics"
)

// vec caches one child per label-value combination, so the metrics sink is
// asked for each series once and a hot path costs a map lookup. A Forge
// sink keys series by name plus labels, which is what lets every child
// share one family on the scrape.
type vec[T any] struct {
	keys  []string
	build func(labels map[string]string) T
	cache sync.Map // label values joined by \xff -> T
}

func (v *vec[T]) with(values []string) (T, bool) {
	var zero T
	if len(values) != len(v.keys) {
		return zero, false
	}
	key := strings.Join(values, "\xff")
	if c, ok := v.cache.Load(key); ok {
		child, isT := c.(T)
		return child, isT
	}
	labels := make(map[string]string, len(v.keys))
	for i, k := range v.keys {
		labels[k] = values[i]
	}
	c, _ := v.cache.LoadOrStore(key, v.build(labels))
	child, isT := c.(T)
	return child, isT
}

// CounterVec is a labelled counter. A nil *CounterVec records nothing, which
// is what a nil metrics sink gives you.
type CounterVec struct{ v vec[gometrics.Counter] }

// NewCounterVec declares a counter on m, or returns nil when m is nil.
func NewCounterVec(m gometrics.Metrics, name, help string, keys ...string) *CounterVec {
	if m == nil {
		return nil
	}
	return &CounterVec{v: vec[gometrics.Counter]{keys: keys, build: func(l map[string]string) gometrics.Counter {
		return m.Counter(name, gometrics.WithDescription(help), gometrics.WithLabels(l))
	}}}
}

// Inc adds one to the series named by values, given in key order.
func (c *CounterVec) Inc(values ...string) {
	if c == nil {
		return
	}
	if child, ok := c.v.with(values); ok {
		child.Inc()
	}
}

// HistogramVec is a labelled histogram. A nil *HistogramVec records nothing.
type HistogramVec struct{ v vec[gometrics.Histogram] }

// NewHistogramVec declares a histogram on m, or returns nil when m is nil.
func NewHistogramVec(m gometrics.Metrics, name, help, unit string, buckets []float64, keys ...string) *HistogramVec {
	if m == nil {
		return nil
	}
	return &HistogramVec{v: vec[gometrics.Histogram]{keys: keys, build: func(l map[string]string) gometrics.Histogram {
		return m.Histogram(name,
			gometrics.WithDescription(help),
			gometrics.WithUnit(unit),
			gometrics.WithBuckets(buckets...),
			gometrics.WithLabels(l),
		)
	}}}
}

// Observe records v in the series named by values, given in key order.
func (h *HistogramVec) Observe(v float64, values ...string) {
	if h == nil {
		return
	}
	if child, ok := h.v.with(values); ok {
		child.Observe(v)
	}
}

// GaugeVec is a labelled gauge. A nil *GaugeVec records nothing.
type GaugeVec struct{ v vec[gometrics.Gauge] }

// NewGaugeVec declares a gauge on m, or returns nil when m is nil.
func NewGaugeVec(m gometrics.Metrics, name, help string, keys ...string) *GaugeVec {
	if m == nil {
		return nil
	}
	return &GaugeVec{v: vec[gometrics.Gauge]{keys: keys, build: func(l map[string]string) gometrics.Gauge {
		return m.Gauge(name, gometrics.WithDescription(help), gometrics.WithLabels(l))
	}}}
}

// Set sets the series named by values, given in key order.
func (g *GaugeVec) Set(v float64, values ...string) {
	if g == nil {
		return
	}
	if child, ok := g.v.with(values); ok {
		child.Set(v)
	}
}
