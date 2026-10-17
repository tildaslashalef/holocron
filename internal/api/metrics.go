package api

import (
	"bytes"
	"context"
	"net/http"
	"runtime"
	rtmetrics "runtime/metrics"
	"strconv"
	"time"

	"github.com/tildaslashalef/holocron/internal/metrics"
	"github.com/tildaslashalef/holocron/internal/provider"
)

// endpoints are §8.2's endpoint label values; any other path is "other",
// so a scanner's paths cannot grow the series.
var endpoints = map[string]bool{"/v1/search": true, "/v1/context": true, "/v1/fetch": true, "/v1/health": true}

// cacheKinds are §8.2's kind label values by endpoint.
var cacheKinds = map[string]string{"/v1/search": "search", "/v1/context": "context", "/v1/fetch": "fetch"}

// serverMetrics are §8.2's families counted per request.
type serverMetrics struct {
	requests  *metrics.Counter
	duration  *metrics.Histogram
	billed    *metrics.Counter
	fallbacks *metrics.Counter
	engines   *metrics.Counter
	lookups   *metrics.Counter
	busy      *metrics.Counter
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{
		requests: metrics.NewCounter("holocron_requests_total",
			"Requests by endpoint, error code, cache result and answering provider, as the log line has them.",
			"endpoint", "code", "cache", "answered_by"),
		duration: metrics.NewHistogram("holocron_request_duration_seconds", "Request durations.",
			[]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}, "endpoint"),
		billed: metrics.NewCounter("holocron_brave_billed_total",
			"Brave's successful answers, which it bills; once per call."),
		fallbacks: metrics.NewCounter("holocron_fallbacks_total",
			"Requests answered after the chain moved past a provider, by provider and reason.", "provider", "reason"),
		engines: metrics.NewCounter("holocron_engines_failed_total",
			"Names in answers' engines_failed, once per upstream call.", "engine"),
		lookups: metrics.NewCounter("holocron_cache_lookups_total",
			"Requests that reached the cache, by kind and result.", "kind", "result"),
		busy: metrics.NewCounter("holocron_busy_total", "busy answers: every slot stayed taken."),
	}
}

// record counts one request with the values of its log line.
func (m *serverMetrics) record(r *http.Request, e *entry, d time.Duration) {
	endpoint := r.URL.Path
	if !endpoints[endpoint] {
		endpoint = "other"
	}
	m.requests.Inc(endpoint, e.code, e.cache, e.answeredBy)
	m.duration.Observe(d.Seconds(), endpoint)
	if n := e.billed["brave"]; n > 0 {
		m.billed.Add(float64(n))
	}
	for _, f := range e.trace.Fallbacks() {
		m.fallbacks.Inc(f.Provider, f.Reason)
	}
	// Failed engines once per upstream call: not on a hit or a stale
	// answer, which repeat an older call's.
	if e.firstOfCall && e.cache == cacheMiss {
		for _, name := range e.enginesFailed {
			m.engines.Inc(name)
		}
	}
	if kind, ok := cacheKinds[endpoint]; ok && e.cache != "" {
		m.lookups.Inc(kind, e.cache)
	}
	if e.code == "busy" {
		m.busy.Inc()
	}
}

// serveMetrics writes §8.2's exposition.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	var b bytes.Buffer
	m := s.metrics
	m.requests.Write(&b)
	m.duration.Write(&b)
	m.billed.Write(&b)
	m.fallbacks.Write(&b)
	m.engines.Write(&b)

	ctx, cancel := context.WithTimeout(r.Context(), HealthTimeout)
	defer cancel()
	var up []metrics.LabeledValue
	for _, p := range provider.Members(s.provider) {
		v := 1.0
		if p.Health(ctx) != nil {
			v = 0
		}
		up = append(up, metrics.LabeledValue{Labels: [][2]string{{"provider", p.Name()}}, Value: v})
	}
	metrics.Gauge(&b, "holocron_provider_up", "1 when the provider is ok, as /v1/health says; Brave's check makes no call.", up...)

	var entries, sizes []metrics.LabeledValue
	for _, k := range []struct {
		kind string
		len  func() (int, int64)
	}{{"search", s.cache.searches.Len}, {"context", s.cache.contexts.Len}, {"fetch", s.cache.pages.Len}} {
		n, bytes := k.len()
		label := [][2]string{{"kind", k.kind}}
		entries = append(entries, metrics.LabeledValue{Labels: label, Value: float64(n)})
		sizes = append(sizes, metrics.LabeledValue{Labels: label, Value: float64(bytes)})
	}
	metrics.Gauge(&b, "holocron_cache_entries", "Entries in each cache.", entries...)
	metrics.Gauge(&b, "holocron_cache_bytes", "Bytes in each cache, as its bound counts them.", sizes...)
	m.lookups.Write(&b)

	metrics.Gauge(&b, "holocron_slots_in_use", "Request slots taken.", metrics.LabeledValue{Value: float64(len(s.slots))})
	metrics.Gauge(&b, "holocron_slots_max", "Request slots.", metrics.LabeledValue{Value: float64(cap(s.slots))})
	m.busy.Write(&b)
	metrics.Gauge(&b, "holocron_build_info", "Always 1; the labels name the build.", metrics.LabeledValue{
		Labels: [][2]string{{"version", s.version}, {"goversion", runtime.Version()}, {"searxng_image", s.searxngImage}},
		Value:  1,
	})
	writeRuntime(&b)

	h := w.Header()
	h.Set("Content-Type", metrics.ContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(http.StatusOK)
	w.Write(b.Bytes())
}

// writeRuntime writes the Go runtime basics from runtime/metrics.
func writeRuntime(b *bytes.Buffer) {
	samples := []rtmetrics.Sample{
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	rtmetrics.Read(samples)
	value := func(s rtmetrics.Sample) float64 {
		if s.Value.Kind() == rtmetrics.KindUint64 {
			return float64(s.Value.Uint64())
		}
		return 0
	}
	metrics.Gauge(b, "go_goroutines", "Goroutines.", metrics.LabeledValue{Value: value(samples[0])})
	metrics.Gauge(b, "go_heap_objects_bytes", "Bytes in live and not yet swept heap objects.", metrics.LabeledValue{Value: value(samples[1])})
	metrics.Gauge(b, "go_memory_total_bytes", "Bytes the Go runtime has mapped.", metrics.LabeledValue{Value: value(samples[2])})
	metrics.CounterFunc(b, "go_gc_cycles_total", "Completed GC cycles.", value(samples[3]))
}
