// Package metrics is a minimal, dependency-free implementation of the
// Prometheus text exposition format
// (https://prometheus.io/docs/instrumenting/exposition_formats/): counters,
// gauges, and histograms with label support, rendered by a Registry. It has
// no knowledge of gotochanger's domain model - that wiring lives in
// internal/api.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
)

// Registry collects Counters, Gauges, and Histograms and renders them in
// registration order. Safe for concurrent use.
type Registry struct {
	mu         sync.Mutex
	collectors []collector
	names      map[string]bool
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{names: map[string]bool{}}
}

type collector interface {
	name() string
	help() string
	typeName() string
	writeSamples(w io.Writer) error
}

func (r *Registry) register(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[c.name()] {
		panic("metrics: duplicate metric name " + c.name())
	}
	r.names[c.name()] = true
	r.collectors = append(r.collectors, c)
}

// WriteExposition renders every registered metric's HELP/TYPE header and
// current samples, in registration order.
func (r *Registry) WriteExposition(w io.Writer) error {
	r.mu.Lock()
	cs := make([]collector, len(r.collectors))
	copy(cs, r.collectors)
	r.mu.Unlock()

	for _, c := range cs {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", c.name(), c.help(), c.name(), c.typeName()); err != nil {
			return err
		}
		if err := c.writeSamples(w); err != nil {
			return err
		}
	}
	return nil
}

// NewCounter registers and returns a new monotonically-increasing Counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	c := &Counter{metricName: name, metricHelp: help, values: map[string]float64{}, labels: map[string]map[string]string{}}
	r.register(c)
	return c
}

// NewGauge registers and returns a new Gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	g := &Gauge{metricName: name, metricHelp: help, values: map[string]float64{}, labels: map[string]map[string]string{}}
	r.register(g)
	return g
}

// NewHistogram registers and returns a new Histogram with the given bucket
// upper bounds (need not be pre-sorted; a +Inf bucket is added implicitly).
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	b := make([]float64, len(buckets))
	copy(b, buckets)
	sort.Float64s(b)
	h := &Histogram{
		metricName:   name,
		metricHelp:   help,
		buckets:      b,
		bucketCounts: map[string][]uint64{},
		sums:         map[string]float64{},
		counts:       map[string]uint64{},
		labels:       map[string]map[string]string{},
	}
	r.register(h)
	return h
}

// Counter is a monotonically-increasing value, optionally split by label
// set (one independent value per distinct label combination).
type Counter struct {
	metricName string
	metricHelp string
	mu         sync.Mutex
	values     map[string]float64
	labels     map[string]map[string]string
}

func (c *Counter) name() string     { return c.metricName }
func (c *Counter) help() string     { return c.metricHelp }
func (c *Counter) typeName() string { return "counter" }

// Inc increments the counter for the given label combination by 1.
func (c *Counter) Inc(labels map[string]string) { c.Add(1, labels) }

// Add increases the counter for the given label combination by delta,
// which must not be negative.
func (c *Counter) Add(delta float64, labels map[string]string) {
	if delta < 0 {
		panic("metrics: counter cannot decrease")
	}
	key := labelKey(labels)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] += delta
	if _, ok := c.labels[key]; !ok {
		c.labels[key] = cloneLabels(labels)
	}
}

func (c *Counter) writeSamples(w io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range mapKeys(c.values) {
		if _, err := fmt.Fprintf(w, "%s%s %s\n", c.metricName, formatLabels(c.labels[key]), formatFloat(c.values[key])); err != nil {
			return err
		}
	}
	return nil
}

// Gauge is a point-in-time value that can go up or down, optionally split
// by label set.
type Gauge struct {
	metricName string
	metricHelp string
	mu         sync.Mutex
	values     map[string]float64
	labels     map[string]map[string]string
}

func (g *Gauge) name() string     { return g.metricName }
func (g *Gauge) help() string     { return g.metricHelp }
func (g *Gauge) typeName() string { return "gauge" }

// Set replaces the value for the given label combination.
func (g *Gauge) Set(value float64, labels map[string]string) {
	key := labelKey(labels)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values[key] = value
	g.labels[key] = cloneLabels(labels)
}

// Reset clears every sample. Callers that recompute a labeled gauge from
// scratch on every scrape (e.g. one sample per volume-location bucket) must
// call this first, otherwise a label combination that no longer applies
// (a bucket that had volumes on a previous scrape but is now empty) would
// keep exposing its last, now-stale value forever instead of disappearing.
func (g *Gauge) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values = map[string]float64{}
	g.labels = map[string]map[string]string{}
}

// Delete removes the sample for one label combination entirely, so it is
// omitted from the exposition rather than reported as a misleading 0
// (e.g. last_backup_timestamp before any backup has ever been taken).
func (g *Gauge) Delete(labels map[string]string) {
	key := labelKey(labels)
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.values, key)
	delete(g.labels, key)
}

func (g *Gauge) writeSamples(w io.Writer) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, key := range mapKeys(g.values) {
		if _, err := fmt.Fprintf(w, "%s%s %s\n", g.metricName, formatLabels(g.labels[key]), formatFloat(g.values[key])); err != nil {
			return err
		}
	}
	return nil
}

// Histogram observes a distribution of values into fixed cumulative
// buckets, plus a running sum and count, optionally split by label set.
type Histogram struct {
	metricName   string
	metricHelp   string
	buckets      []float64
	mu           sync.Mutex
	bucketCounts map[string][]uint64
	sums         map[string]float64
	counts       map[string]uint64
	labels       map[string]map[string]string
}

func (h *Histogram) name() string     { return h.metricName }
func (h *Histogram) help() string     { return h.metricHelp }
func (h *Histogram) typeName() string { return "histogram" }

// Observe records one value for the given label combination.
func (h *Histogram) Observe(value float64, labels map[string]string) {
	key := labelKey(labels)
	h.mu.Lock()
	defer h.mu.Unlock()
	counts, ok := h.bucketCounts[key]
	if !ok {
		counts = make([]uint64, len(h.buckets))
		h.bucketCounts[key] = counts
		h.labels[key] = cloneLabels(labels)
	}
	for i, bound := range h.buckets {
		if value <= bound {
			counts[i]++
		}
	}
	h.sums[key] += value
	h.counts[key]++
}

func (h *Histogram) writeSamples(w io.Writer) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range mapKeys(h.bucketCounts) {
		labels := h.labels[key]
		counts := h.bucketCounts[key]
		for i, bound := range h.buckets {
			bl := withLabel(labels, "le", formatFloat(bound))
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, formatLabels(bl), counts[i]); err != nil {
				return err
			}
		}
		blInf := withLabel(labels, "le", "+Inf")
		if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", h.metricName, formatLabels(blInf), h.counts[key]); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_sum%s %s\n", h.metricName, formatLabels(labels), formatFloat(h.sums[key])); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_count%s %d\n", h.metricName, formatLabels(labels), h.counts[key]); err != nil {
			return err
		}
	}
	return nil
}

func withLabel(labels map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out[key] = value
	return out
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// labelKey canonicalizes a label set into a stable map key so repeated
// observations with the same labels accumulate into the same sample.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, labels[k]...)
		b = append(b, ',')
	}
	return string(b)
}

// formatLabels renders a label set as "{k=\"v\",k2=\"v2\"}", or "" if empty.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := "{"
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		out += k + `="` + escapeLabelValue(labels[k]) + `"`
	}
	return out + "}"
}

func escapeLabelValue(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, v[i])
		}
	}
	return string(out)
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

func mapKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
