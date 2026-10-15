// Package metrics is the little of Prometheus that loam-api exports: counters,
// histograms and gauges read at scrape time, written in the text exposition
// format. It is here rather than prometheus/client_golang because that is a
// tree of dependencies for a page of code (CLAUDE.md, "Standard library first").
package metrics

import (
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds the metric families in the order they were made.
type Registry struct {
	mu       sync.Mutex
	families []family
}

type family interface {
	write(w io.Writer)
}

func (r *Registry) add(f family) {
	r.mu.Lock()
	r.families = append(r.families, f)
	r.mu.Unlock()
}

// Write writes every family in the text format (version 0.0.4).
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	fs := slices.Clone(r.families)
	r.mu.Unlock()
	for _, f := range fs {
		f.write(w)
	}
}

// ContentType is the text format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

type desc struct {
	name, help string
	labels     []string
}

func (d desc) header(w io.Writer, kind string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", d.name, strings.ReplaceAll(d.help, "\n", " "), d.name, kind)
}

// series is one label set's values, in the family's label order.
type series struct {
	values []string
}

func (d desc) key(values []string) string {
	if len(values) != len(d.labels) {
		// A call with the wrong number of values is a bug in the caller.
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", d.name, len(d.labels), len(values)))
	}
	return strings.Join(values, "\xff")
}

// labelText is {a="x",b="y"}, with extra pairs (a histogram's le) last.
func (d desc) labelText(values []string, extra ...string) string {
	if len(d.labels) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	pair := func(k, v string) {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escape(v))
		b.WriteByte('"')
	}
	for i, l := range d.labels {
		pair(l, values[i])
	}
	for i := 0; i+1 < len(extra); i += 2 {
		pair(extra[i], extra[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func number(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// matches says whether a series has every label in match.
func (d desc) matches(s series, match map[string]string) bool {
	for k, v := range match {
		i := slices.Index(d.labels, k)
		if i < 0 || s.values[i] != v {
			return false
		}
	}
	return true
}

// CounterVec is a counter per label set.
type CounterVec struct {
	desc
	mu   sync.Mutex
	vals map[string]*counter
}

type counter struct {
	series
	v float64
}

func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{desc: desc{name, help, labels}, vals: map[string]*counter{}}
	r.add(c)
	return c
}

// Add adds v (not negative) to the series of the label values.
func (c *CounterVec) Add(v float64, values ...string) {
	k := c.key(values)
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.vals[k]
	if s == nil {
		s = &counter{series: series{slices.Clone(values)}}
		c.vals[k] = s
	}
	s.v += v
}

func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Delete drops every series whose labels include match: a sandbox's, when
// the sandbox is deleted, so a churn of sandboxes doesn't grow the page for ever.
func (c *CounterVec) Delete(match map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.vals, func(_ string, s *counter) bool { return c.matches(s.series, match) })
}

func (c *CounterVec) write(w io.Writer) {
	c.header(w, "counter")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range sortedKeys(c.vals) {
		s := c.vals[k]
		fmt.Fprintf(w, "%s%s %s\n", c.name, c.labelText(s.values), number(s.v))
	}
}

// GaugeVec is a gauge per label set, set and moved by the caller.
type GaugeVec struct {
	desc
	mu   sync.Mutex
	vals map[string]*counter
}

func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	g := &GaugeVec{desc: desc{name, help, labels}, vals: map[string]*counter{}}
	r.add(g)
	return g
}

// Add moves the series of the label values by v, which may be negative.
func (g *GaugeVec) Add(v float64, values ...string) {
	k := g.key(values)
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.vals[k]
	if s == nil {
		s = &counter{series: series{slices.Clone(values)}}
		g.vals[k] = s
	}
	s.v += v
}

// AddExisting is Add to a series that exists, and nothing otherwise: the
// close of a connection whose series was deleted while it was open.
func (g *GaugeVec) AddExisting(v float64, values ...string) {
	k := g.key(values)
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.vals[k]; s != nil {
		s.v += v
	}
}

func (g *GaugeVec) Delete(match map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	maps.DeleteFunc(g.vals, func(_ string, s *counter) bool { return g.matches(s.series, match) })
}

func (g *GaugeVec) write(w io.Writer) {
	g.header(w, "gauge")
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range sortedKeys(g.vals) {
		s := g.vals[k]
		fmt.Fprintf(w, "%s%s %s\n", g.name, g.labelText(s.values), number(s.v))
	}
}

// Sample is one series of a GaugeFunc: its label values and its value.
type Sample struct {
	Values []string
	Value  float64
}

// GaugeFunc is a gauge read at scrape time, for numbers that live elsewhere
// (the sandboxes by state, from the store).
type GaugeFunc struct {
	desc
	collect func() []Sample
}

func (r *Registry) GaugeFunc(name, help string, collect func() []Sample, labels ...string) {
	r.add(&GaugeFunc{desc: desc{name, help, labels}, collect: collect})
}

func (g *GaugeFunc) write(w io.Writer) {
	g.header(w, "gauge")
	ss := g.collect()
	sort.Slice(ss, func(i, j int) bool { return g.key(ss[i].Values) < g.key(ss[j].Values) })
	for _, s := range ss {
		fmt.Fprintf(w, "%s%s %s\n", g.name, g.labelText(s.Values), number(s.Value))
	}
}

// HistogramVec is a histogram per label set.
type HistogramVec struct {
	desc
	buckets []float64
	mu      sync.Mutex
	vals    map[string]*histogram
}

type histogram struct {
	series
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

// Histogram makes a histogram family with the given upper bounds, ascending.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	h := &HistogramVec{desc: desc{name, help, labels}, buckets: buckets, vals: map[string]*histogram{}}
	r.add(h)
	return h
}

func (h *HistogramVec) Observe(v float64, values ...string) {
	k := h.key(values)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.vals[k]
	if s == nil {
		s = &histogram{series: series{slices.Clone(values)}, counts: make([]uint64, len(h.buckets))}
		h.vals[k] = s
	}
	if i := sort.SearchFloat64s(h.buckets, v); i < len(h.buckets) {
		s.counts[i]++
	}
	s.sum += v
	s.count++
}

func (h *HistogramVec) Delete(match map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	maps.DeleteFunc(h.vals, func(_ string, s *histogram) bool { return h.matches(s.series, match) })
}

func (h *HistogramVec) write(w io.Writer) {
	h.header(w, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range sortedKeys(h.vals) {
		s := h.vals[k]
		var cum uint64
		for i, le := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labelText(s.values, "le", number(le)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labelText(s.values, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, h.labelText(s.values), number(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, h.labelText(s.values), s.count)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := slices.Collect(maps.Keys(m))
	sort.Strings(ks)
	return ks
}
