// Package telemetrytest is a recording metrics sink for cortex's own tests.
//
// The go-utils collector keys series by name alone, so every labelled child
// of one family would collapse into the first. A Forge app keys them by name
// and labels, and this sink does the same, which is what lets a root-module
// test assert label values without importing forge.
package telemetrytest

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	gometrics "github.com/xraph/go-utils/metrics"
)

// Sink records every series the code under test declares.
type Sink struct {
	*gometrics.MockMetrics

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	name   string
	help   string
	labels map[string]string

	counter   gometrics.Counter
	gauge     gometrics.Gauge
	histogram gometrics.Histogram
}

// NewSink returns an empty sink.
func NewSink() *Sink {
	s := &Sink{MockMetrics: &gometrics.MockMetrics{}, series: map[string]*series{}}
	s.CounterFunc = func(name string, opts ...gometrics.MetricOption) gometrics.Counter {
		return s.get(name, opts, func(se *series) { se.counter = gometrics.NewCounter(name) }).counter
	}
	s.GaugeFunc = func(name string, opts ...gometrics.MetricOption) gometrics.Gauge {
		return s.get(name, opts, func(se *series) { se.gauge = gometrics.NewGauge(name) }).gauge
	}
	s.HistogramFunc = func(name string, opts ...gometrics.MetricOption) gometrics.Histogram {
		return s.get(name, opts, func(se *series) { se.histogram = gometrics.NewHistogram(name) }).histogram
	}
	return s
}

func (s *Sink) get(name string, opts []gometrics.MetricOption, build func(*series)) *series {
	var mo gometrics.MetricOptions
	for _, o := range opts {
		o(&mo)
	}
	key := seriesKey(name, mo.Labels)
	s.mu.Lock()
	defer s.mu.Unlock()
	if se, ok := s.series[key]; ok {
		return se
	}
	se := &series{name: name, help: mo.Description, labels: maps.Clone(mo.Labels)}
	build(se)
	s.series[key] = se
	return se
}

func seriesKey(name string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range slices.Sorted(maps.Keys(labels)) {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", k, labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

// Value returns a counter's or gauge's value, or a histogram's count, for
// the series with exactly these labels. A series never declared reads 0.
func (s *Sink) Value(name string, labels map[string]string) float64 {
	s.mu.Lock()
	se, ok := s.series[seriesKey(name, labels)]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	switch {
	case se.counter != nil:
		return se.counter.Value()
	case se.gauge != nil:
		return se.gauge.Value()
	case se.histogram != nil:
		return float64(se.histogram.Count())
	}
	return 0
}

// Names returns every declared metric name, sorted and without repeats.
func (s *Sink) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, se := range s.series {
		seen[se.name] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// LabelKeys returns the label keys each series of name carries, one sorted
// key list per series.
func (s *Sink) LabelKeys(name string) [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]string
	for _, se := range s.series {
		if se.name == name {
			out = append(out, slices.Sorted(maps.Keys(se.labels)))
		}
	}
	return out
}

// Scrape renders every series as one line of name{labels} value, the way
// an exposition format would, so a test can search it for text that must
// never become a label.
func (s *Sink) Scrape() string {
	s.mu.Lock()
	keys := slices.Sorted(maps.Keys(s.series))
	s.mu.Unlock()
	var b strings.Builder
	for _, k := range keys {
		s.mu.Lock()
		se := s.series[k]
		s.mu.Unlock()
		fmt.Fprintf(&b, "# HELP %s %s\n%s %v\n", se.name, se.help, k, s.Value(se.name, se.labels))
	}
	return b.String()
}
