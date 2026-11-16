package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/metrics"
	"github.com/tildaslashalef/holocron/internal/provider"
)

// scrape reads /v1/metrics and parses it strictly.
func scrape(t *testing.T, h harness) *metrics.Parsed {
	t.Helper()
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("scrape: %d %v", rec.Code, rec.Header())
	}
	body, _ := io.ReadAll(rec.Body)
	p, err := metrics.Parse(string(body))
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	return p
}

func wantSamples(t *testing.T, p *metrics.Parsed, want map[string]float64) {
	t.Helper()
	for key, v := range want {
		if got, ok := p.Samples[key]; !ok || got != v {
			t.Errorf("%s = %v (present %v), want %v", key, got, ok, v)
		}
	}
}

// §8.2: every family has one HELP and TYPE, the exposition parses, and a
// scrape is neither logged nor counted.
func TestMetricsExposition(t *testing.T) {
	h := newHarness(&fakeProvider{}, nil, true)
	scrape(t, h)
	p := scrape(t, h)
	for family, typ := range map[string]string{
		"holocron_requests_total": "counter", "holocron_request_duration_seconds": "histogram",
		"holocron_billed_total": "counter", "holocron_cost_usd_total": "counter",
		"holocron_cache_saved_usd_total": "counter", "holocron_fallbacks_total": "counter",
		"holocron_providers_failed_total": "counter", "holocron_provider_up": "gauge",
		"holocron_cache_entries": "gauge", "holocron_cache_bytes": "gauge", "holocron_cache_lookups_total": "counter",
		"holocron_slots_in_use": "gauge", "holocron_slots_max": "gauge", "holocron_busy_total": "counter",
		"holocron_build_info": "gauge", "go_goroutines": "gauge", "go_heap_objects_bytes": "gauge",
		"go_memory_total_bytes": "gauge", "go_gc_cycles_total": "counter",
	} {
		if p.Types[family] != typ {
			t.Errorf("%s: type %q, want %s", family, p.Types[family], typ)
		}
	}
	wantSamples(t, p, map[string]float64{
		`holocron_provider_up{provider="fake"}`:                                     1,
		`holocron_provider_up{provider="local"}`:                                    1,
		`holocron_cache_entries{kind="fetch"}`:                                      0,
		`holocron_slots_max{pool="upstream"}`:                                       Slots,
		`holocron_slots_max{pool="extract"}`:                                        ExtractSlots,
		`holocron_slots_in_use{pool="upstream"}`:                                    0,
		`holocron_slots_in_use{pool="extract"}`:                                     0,
		`holocron_cache_saved_usd_total`:                                            0,
		`holocron_busy_total`:                                                       0,
		`holocron_build_info{version="1.2.3",goversion="` + runtimeVersion() + `"}`: 1,
	})
	if p.Samples["go_goroutines"] < 1 || p.Samples["go_memory_total_bytes"] <= 0 {
		t.Errorf("runtime: %v %v", p.Samples["go_goroutines"], p.Samples["go_memory_total_bytes"])
	}
	for key := range p.Samples {
		if strings.Contains(key, "metrics") {
			t.Errorf("the scrape was counted: %s", key)
		}
	}
	if h.logged() != "" {
		t.Errorf("the scrape was logged:\n%s", h.logged())
	}
	// Not GET: an ordinary 405, logged and counted.
	var eb ErrorBody
	if resp := h.do(t, http.MethodPost, "/v1/metrics", &eb); resp.StatusCode != 405 || eb.Error.Code != "method_not_allowed" {
		t.Errorf("POST: %d %+v", resp.StatusCode, eb)
	}
	wantSamples(t, scrape(t, h), map[string]float64{`holocron_requests_total{endpoint="other",code="method_not_allowed",cache="",answered_by=""}`: 1})
}

func runtimeVersion() string { return runtime.Version() }

// Requests are counted with their log line's values; paths outside the
// contract are "other"; no query or URL becomes a label.
func TestMetricsCountRequests(t *testing.T) {
	f := &fakeFetcher{err: &fetch.UpstreamError{Msg: "example.org answered 404 Not Found"}}
	h := newHarness(&fakeProvider{res: results(3)}, f, true)
	h.clock()
	h.do(t, http.MethodGet, "/v1/search?q=private+words", nil)
	h.do(t, http.MethodGet, "/v1/search?q=private+words", nil)
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/secret", nil)
	h.do(t, http.MethodGet, "/v1/health", nil)
	h.do(t, http.MethodGet, "/v1/scanner/probe/x", nil)
	h.do(t, http.MethodGet, "/v1/search?q=", nil)
	p := scrape(t, h)
	wantSamples(t, p, map[string]float64{
		`holocron_requests_total{endpoint="/v1/search",code="",cache="miss",answered_by="fake"}`:           1,
		`holocron_requests_total{endpoint="/v1/search",code="",cache="hit",answered_by="fake"}`:            1,
		`holocron_requests_total{endpoint="/v1/search",code="bad_request",cache="",answered_by=""}`:        1,
		`holocron_requests_total{endpoint="/v1/fetch",code="upstream_failed",cache="miss",answered_by=""}`: 1,
		`holocron_requests_total{endpoint="/v1/health",code="",cache="",answered_by=""}`:                   1,
		`holocron_requests_total{endpoint="other",code="not_found",cache="",answered_by=""}`:               1,
		`holocron_request_duration_seconds_count{endpoint="/v1/search"}`:                                   3,
		`holocron_cache_lookups_total{kind="search",result="miss"}`:                                        1,
		`holocron_cache_lookups_total{kind="search",result="hit"}`:                                         1,
		`holocron_cache_lookups_total{kind="fetch",result="miss"}`:                                         1,
		`holocron_cache_entries{kind="search"}`:                                                            1,
		`holocron_cache_entries{kind="fetch"}`:                                                             1,
	})
	for key := range p.Samples {
		for _, leak := range []string{"private", "secret", "scanner", "example.org"} {
			if strings.Contains(key, leak) {
				t.Errorf("label holds %q: %s", leak, key)
			}
		}
	}
}

// A metered provider's bill counts once per call, with its cost; a hit
// counts what it saved; fallbacks count once per request, and failed
// providers once per call.
func TestMetricsBillsAndFallbacks(t *testing.T) {
	par := &traceProvider{name: "parallel", res: results(3), block: make(chan struct{})}
	brave := &traceProvider{name: "brave", res: results(2)}
	h := newHarness(provider.NewChain(par, brave), nil, true)
	h.clock()
	key := searchKey("parallel,brave", "shared", "")
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			h.srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/search?q=shared", nil))
		})
	}
	waitFor(t, func() bool { return h.srv.cache.searchCalls.Waiting(key) == 3 })
	close(par.block)
	wg.Wait()
	h.do(t, http.MethodGet, "/v1/search?q=shared", nil) // a hit: saved, not billed

	par.err = &provider.UnavailableError{Msg: "parallel: credit used up", Reason: provider.ReasonQuota}
	h.do(t, http.MethodGet, "/v1/search?q=fallen", nil)
	h.do(t, http.MethodGet, "/v1/search?q=fallen", nil) // a hit: no new failure
	h.do(t, http.MethodGet, "/v1/search?q=again", nil)
	p := scrape(t, h)
	wantSamples(t, p, map[string]float64{
		`holocron_billed_total{provider="parallel"}`:                                                 1,
		`holocron_billed_total{provider="brave"}`:                                                    2,
		`holocron_cost_usd_total{provider="parallel"}`:                                               0.005,
		`holocron_cost_usd_total{provider="brave"}`:                                                  0.01,
		`holocron_requests_total{endpoint="/v1/search",code="",cache="miss",answered_by="parallel"}`: 3,
		`holocron_requests_total{endpoint="/v1/search",code="",cache="miss",answered_by="brave"}`:    2,
		`holocron_fallbacks_total{provider="parallel",reason="quota"}`:                               2,
		`holocron_providers_failed_total{provider="parallel"}`:                                       2,
		`holocron_provider_up{provider="parallel"}`:                                                  1,
		`holocron_provider_up{provider="brave"}`:                                                     1,
	})
	if saved := p.Samples[`holocron_cache_saved_usd_total`]; saved < 0.0099 || saved > 0.0101 {
		t.Errorf("saved %v, want 0.01: two hits", saved)
	}
}
