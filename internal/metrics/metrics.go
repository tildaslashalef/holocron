// Package metrics is §8.2's machinery (SPEC.md): counters and a histogram
// with fixed label names, and the Prometheus text exposition format 0.0.4,
// written with the standard library.
package metrics

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ContentType is the exposition's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Counter is a counter family: one value per set of label values. It is
// safe for concurrent use.
type Counter struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]*sample
}

type sample struct {
	labels []string
	value  float64
}

// NewCounter returns a counter family with these label names.
func NewCounter(name, help string, labels ...string) *Counter {
	return &Counter{name: name, help: help, labels: labels, values: map[string]*sample{}}
}

// Add adds v to the counter for these label values, one per label name.
func (c *Counter) Add(v float64, values ...string) {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", c.name, len(c.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.values[key]
	if !ok {
		s = &sample{labels: slices.Clone(values)}
		c.values[key] = s
	}
	s.value += v
}

// Inc adds 1.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Value returns the counter for these label values.
func (c *Counter) Value(values ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.values[strings.Join(values, "\x00")]; ok {
		return s.value
	}
	return 0
}

// Write writes the family; one without labels is written as 0 before it
// is first counted.
func (c *Counter) Write(w io.Writer) {
	header(w, c.name, c.help, "counter")
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.labels) == 0 && len(c.values) == 0 {
		Sample(w, c.name, nil, 0)
		return
	}
	for _, key := range sortedKeys(c.values) {
		s := c.values[key]
		Sample(w, c.name, pairs(c.labels, s.labels), s.value)
	}
}

// Histogram is a histogram family with fixed buckets.
type Histogram struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	values     map[string]*histSample
}

type histSample struct {
	labels []string
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

// NewHistogram returns a histogram family with these upper bounds
// (ascending; +Inf is implied) and label names.
func NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	return &Histogram{name: name, help: help, labels: labels, buckets: buckets, values: map[string]*histSample{}}
}

// Observe records v for these label values.
func (h *Histogram) Observe(v float64, values ...string) {
	if len(values) != len(h.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", h.name, len(h.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.values[key]
	if !ok {
		s = &histSample{labels: slices.Clone(values), counts: make([]uint64, len(h.buckets))}
		h.values[key] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

// Write writes the family: cumulative buckets, +Inf, sum and count.
func (h *Histogram) Write(w io.Writer) {
	header(w, h.name, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range sortedKeys(h.values) {
		s := h.values[key]
		base := pairs(h.labels, s.labels)
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			Sample(w, h.name+"_bucket", append(base, [2]string{"le", formatFloat(b)}), float64(cum))
		}
		Sample(w, h.name+"_bucket", append(base, [2]string{"le", "+Inf"}), float64(s.count))
		Sample(w, h.name+"_sum", base, s.sum)
		Sample(w, h.name+"_count", base, float64(s.count))
	}
}

// Gauge writes a gauge family whose samples are read at scrape time.
func Gauge(w io.Writer, name, help string, samples ...LabeledValue) {
	header(w, name, help, "gauge")
	for _, s := range samples {
		Sample(w, name, s.Labels, s.Value)
	}
}

// CounterFunc writes a counter family read at scrape time (one kept
// elsewhere, such as the Go runtime's).
func CounterFunc(w io.Writer, name, help string, v float64) {
	header(w, name, help, "counter")
	Sample(w, name, nil, v)
}

// LabeledValue is one sample: label pairs and a value.
type LabeledValue struct {
	Labels [][2]string
	Value  float64
}

func header(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
}

// Sample writes one sample line.
func Sample(w io.Writer, name string, labels [][2]string, v float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l[0])
			b.WriteString(`="`)
			b.WriteString(escapeLabel(l[1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatFloat(v))
	b.WriteByte('\n')
	io.WriteString(w, b.String())
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func pairs(names, values []string) [][2]string {
	out := make([][2]string, len(names))
	for i := range names {
		out[i] = [2]string{names[i], values[i]}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
