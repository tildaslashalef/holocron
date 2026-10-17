package brave

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/provider"
)

const testKey = "BSA-secret-key-123"

// fakeBrave answers each request with the next of its answers, recording
// the requests.
type fakeBrave struct {
	mu       sync.Mutex
	answers  []answer
	requests []*http.Request
}

type answer struct {
	status  int
	headers map[string]string
	body    string
}

func (f *fakeBrave) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	a := answer{status: 500, body: `{"error":{"detail":"no answer left"}}`}
	if len(f.answers) > 0 {
		a, f.answers = f.answers[0], f.answers[1:]
	}
	f.mu.Unlock()
	for k, v := range a.headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(a.status)
	fmt.Fprint(w, a.body)
}

func (f *fakeBrave) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type harness struct {
	p     *Provider
	fake  *fakeBrave
	clock time.Time
	slept []time.Duration
}

func newHarness(t *testing.T, answers ...answer) *harness {
	t.Helper()
	f := &fakeBrave{answers: answers}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	h := &harness{fake: f, clock: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	h.p = newProvider(srv.URL+"/res", testKey, "holocron/test")
	h.p.now = func() time.Time { return h.clock }
	h.p.sleep = func(_ context.Context, d time.Duration) error { h.slept = append(h.slept, d); return nil }
	return h
}

var quotaHeaders = map[string]string{
	"X-RateLimit-Limit":     "50, 1000",
	"X-RateLimit-Remaining": "49, 948",
	"X-RateLimit-Reset":     "1, 86400",
}

func TestSearch(t *testing.T) {
	h := newHarness(t, answer{200, quotaHeaders, `{"type":"search","query":{"original":"zig std.htpp","altered":"zig std.http"},"web":{"type":"search","results":[
		{"title":"Zig&#x27;s std.http","url":"https://ziglang.org/a","description":"A &amp; B","page_age":"2026-09-01T10:00:00","extra_snippets":["more &amp; more","again"]},
		{"title":"No date","url":"https://example.org/b","description":"d","page_age":"yesterday"}]}}`})
	trace := &provider.Trace{}
	res, err := h.p.Search(provider.WithTrace(provider.WithNoCache(context.Background()), trace), provider.SearchRequest{Query: "zig std.htpp", N: 10, Freshness: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if q := trace.Quotas()["brave"]; q.Remaining != 948 || q.ResetsIn != 24*time.Hour {
		t.Errorf("trace quota = %+v", trace.Quotas())
	}
	want := []provider.Result{
		{Title: "Zig's std.http", URL: "https://ziglang.org/a", Snippet: "A & B", ExtraSnippets: []string{"more & more", "again"}, Engine: "brave", Published: "2026-09-01"},
		{Title: "No date", URL: "https://example.org/b", Snippet: "d", ExtraSnippets: []string{}, Engine: "brave"},
	}
	if !reflect.DeepEqual(res.Items, want) || res.EnginesFailed == nil || res.AlteredQuery != "zig std.http" {
		t.Errorf("results = %+v", res)
	}
	r := h.fake.requests[0]
	q := r.URL.Query()
	if r.URL.Path != "/res/v1/web/search" || q.Get("q") != "zig std.htpp" || q.Get("count") != "10" ||
		q.Get("safesearch") != "off" || q.Get("text_decorations") != "false" || q.Get("result_filter") != "web" ||
		q.Get("extra_snippets") != "true" || q.Get("freshness") != "pw" {
		t.Errorf("request %s", r.URL)
	}
	if r.Header.Get("X-Subscription-Token") != testKey || r.Header.Get("User-Agent") != "holocron/test" ||
		r.Header.Get("Cache-Control") != "no-cache" || strings.Contains(r.URL.String(), testKey) {
		t.Errorf("headers %v", r.Header)
	}
	if q, ok := h.p.Quota(); !ok || q.Remaining != 948 || q.ResetsIn != 24*time.Hour {
		t.Errorf("quota = %+v %v", q, ok)
	}
	h.clock = h.clock.Add(time.Hour)
	if q, _ := h.p.Quota(); q.ResetsIn != 23*time.Hour {
		t.Errorf("quota an hour later = %+v", q)
	}
}

func TestSearchCountClamped(t *testing.T) {
	h := newHarness(t, answer{200, nil, `{"web":{"results":[]}}`})
	res, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 50})
	if err != nil || len(res.Items) != 0 {
		t.Fatal(res, err)
	}
	if q := h.fake.requests[0].URL.Query(); q.Get("count") != "20" || q.Has("freshness") {
		t.Errorf("request %s", h.fake.requests[0].URL)
	}
	if res.AlteredQuery != "" {
		t.Errorf("altered %q without a correction", res.AlteredQuery)
	}
	if h.fake.requests[0].Header.Get("Cache-Control") != "" {
		t.Error("no-cache sent unasked")
	}
	if _, ok := h.p.Quota(); ok {
		t.Error("a quota without the headers")
	}
}

func TestContext(t *testing.T) {
	h := newHarness(t, answer{200, quotaHeaders, `{"grounding":{"generic":[
		{"url":"https://ziglang.org/a","title":"Zig &amp; HTTP","snippets":["one","{\"table\":[1,2]}"]},
		{"url":"https://example.org/b","title":"B","snippets":["two"]}],"map":[]},
		"sources":{"https://ziglang.org/a":{"title":"Zig","hostname":"ziglang.org","age":["Monday, September 1, 2026","2026-09-01","45 days ago","2026-09-01T10:00:00Z"]},
		"https://example.org/b":{"age":[]}}}`})
	res, err := h.p.Context(context.Background(), provider.ContextRequest{Query: "zig http", Tokens: 4096, URLs: 8, Freshness: "2026-01-01to2026-10-01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sources) != 2 || res.Sources[0].Title != "Zig & HTTP" || res.Sources[0].Published != "2026-09-01" ||
		len(res.Sources[0].Snippets) != 2 || res.Sources[0].Snippets[1] != `{"table":[1,2]}` || res.Sources[1].Published != "" {
		t.Errorf("context = %+v", res)
	}
	q := h.fake.requests[0].URL.Query()
	if h.fake.requests[0].URL.Path != "/res/v1/llm/context" || q.Get("maximum_number_of_tokens") != "4096" ||
		q.Get("maximum_number_of_urls") != "8" || q.Get("count") != "20" || q.Get("enable_local") != "false" ||
		q.Get("freshness") != "2026-01-01to2026-10-01" {
		t.Errorf("request %s", h.fake.requests[0].URL)
	}
}

func TestRateLimitRetry(t *testing.T) {
	limited := answer{429, map[string]string{"X-RateLimit-Limit": "50, 1000", "X-RateLimit-Remaining": "0, 500", "X-RateLimit-Reset": "1, 1000"}, `{"error":{"detail":"slow down"}}`}
	h := newHarness(t, limited, answer{200, nil, `{"web":{"results":[{"title":"t","url":"https://a.example/"}]}}`})
	res, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10})
	if err != nil || len(res.Items) != 1 || len(h.slept) != 1 || h.slept[0] != time.Second {
		t.Errorf("retry: %+v %v, slept %v", res, err, h.slept)
	}
	// Twice limited: no_engines, without a hold.
	h = newHarness(t, limited, limited, answer{200, nil, `{"web":{"results":[]}}`})
	_, err = h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10})
	if u, _ := errors.AsType[*provider.UnavailableError](err); !unavailable(err, "rate-limited: slow down") || u.Reason != provider.ReasonRateLimited {
		t.Errorf("twice: %v", err)
	}
	if err := h.p.Health(context.Background()); err != nil {
		t.Errorf("health after a rate limit: %v", err)
	}
	if _, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10}); err != nil {
		t.Errorf("next search: %v", err)
	}
}

// A used-up quota, a 402 and a refused key hold calls back, and health
// says so; the hold ends.
func TestHolds(t *testing.T) {
	for _, tt := range []struct {
		name   string
		a      answer
		why    string
		hold   time.Duration
		reason string
	}{
		{"quota", answer{429, map[string]string{"X-RateLimit-Limit": "50, 1000", "X-RateLimit-Remaining": "50, 0", "X-RateLimit-Reset": "1, 7200"}, `{}`}, "quota or credit used up", 2 * time.Hour, "quota"},
		{"quota without a reset", answer{429, map[string]string{"X-RateLimit-Limit": "50, 1000", "X-RateLimit-Remaining": "50, 0"}, `{}`}, "quota or credit used up", time.Hour, "quota"},
		{"payment", answer{402, nil, `{"error":{"detail":"no credit"}}`}, "quota or credit used up: no credit", time.Hour, "quota"},
		{"401", answer{401, nil, `{"error":{"code":"SUBSCRIPTION_TOKEN_INVALID","detail":"The provided subscription token is invalid."}}`}, "refused the key (401): The provided subscription token is invalid.", 10 * time.Minute, "key_refused"},
		{"403", answer{403, nil, ``}, "refused the key (403)", 10 * time.Minute, "key_refused"},
		// What Brave answered a wrong key, 2026-10-16.
		{"422 invalid token", answer{422, nil, `{"error":{"code":"SUBSCRIPTION_TOKEN_INVALID","detail":"The provided subscription token is invalid.","meta":{"component":"authentication"},"status":422},"type":"ErrorResponse"}`},
			"refused the key (422): The provided subscription token is invalid.", 10 * time.Minute, "key_refused"},
	} {
		h := newHarness(t, tt.a, answer{200, nil, `{"web":{"results":[]}}`})
		_, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10})
		if !unavailable(err, tt.why) {
			t.Errorf("%s: %v", tt.name, err)
		}
		if u, _ := errors.AsType[*provider.UnavailableError](err); u.Reason != tt.reason {
			t.Errorf("%s: reason %q, want %q", tt.name, u.Reason, tt.reason)
		}
		if strings.Contains(fmt.Sprint(err), testKey) {
			t.Errorf("%s: the error names the key", tt.name)
		}
		if err := h.p.Health(context.Background()); !unavailable(err, tt.why) {
			t.Errorf("%s: health %v", tt.name, err)
		}
		h.clock = h.clock.Add(tt.hold - time.Second)
		if _, err := h.p.Context(context.Background(), provider.ContextRequest{Query: "x", Tokens: 1024, URLs: 1}); !unavailable(err, tt.why) || h.fake.count() != 1 {
			t.Errorf("%s: during the hold: %v, %d requests", tt.name, err, h.fake.count())
		}
		h.clock = h.clock.Add(time.Second)
		if err := h.p.Health(context.Background()); err != nil {
			t.Errorf("%s: health after the hold: %v", tt.name, err)
		}
		if _, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10}); err != nil || h.fake.count() != 2 {
			t.Errorf("%s: after the hold: %v, %d requests", tt.name, err, h.fake.count())
		}
	}
}

// A monthly limit of 0 is unlimited: a remaining 0 is not a used-up
// quota, and no quota is reported.
func TestUnlimited(t *testing.T) {
	headers := map[string]string{"X-RateLimit-Limit": "50, 0", "X-RateLimit-Remaining": "0, 0", "X-RateLimit-Reset": "1, 0"}
	h := newHarness(t, answer{429, headers, `{}`}, answer{200, headers, `{"web":{"results":[]}}`})
	if _, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10}); err != nil || len(h.slept) != 1 {
		t.Errorf("err %v, slept %v", err, h.slept)
	}
	if _, ok := h.p.Quota(); ok {
		t.Error("a quota for an unlimited plan")
	}
}

func TestFailures(t *testing.T) {
	for _, tt := range []struct {
		a     answer
		check func(error) bool
	}{
		{answer{422, nil, `{"error":{"code":"VALIDATION","detail":"Unable to validate request parameter(s)","meta":{"errors":[{"loc":["query","q"],"msg":"String should have at most 400 characters"}]},"status":422}}`}, func(err error) bool {
			b, ok := errors.AsType[*provider.BadQueryError](err)
			return ok && b.Msg == "brave refused the query: String should have at most 400 characters"
		}},
		// A complaint about another parameter is holocron's request, not
		// the client's query.
		{answer{422, nil, `{"error":{"code":"VALIDATION","detail":"Unable to validate request parameter(s)","meta":{"errors":[{"loc":["query","count"],"msg":"too large"}]},"status":422}}`}, func(err error) bool {
			return unavailable(err, "brave answered 422: Unable to validate request parameter(s)")
		}},
		{answer{400, nil, `{}`}, func(err error) bool { return unavailable(err, "brave answered 400") }},
		{answer{500, nil, `oops`}, func(err error) bool { return unavailable(err, "brave answered 500") }},
		{answer{200, nil, `not json`}, func(err error) bool { return unavailable(err, "unreadable answer") }},
	} {
		h := newHarness(t, tt.a)
		if _, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10}); !tt.check(err) {
			t.Errorf("%d %q: %v", tt.a.status, tt.a.body, err)
		}
	}

	// Unreachable, and too slow.
	h := newHarness(t)
	h.p.base.Host = "127.0.0.1:1"
	if _, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 10}); !unavailable(err, "unreachable") {
		t.Errorf("unreachable: %v", err)
	}
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	p := newProvider(slow.URL, testKey, "t")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Search(ctx, provider.SearchRequest{Query: "x", N: 10}); !errors.As(err, new(*provider.TimeoutError)) {
		t.Errorf("slow: %v", err)
	}
}

func TestNew(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p, err := New(write("ok", "  "+testKey+"\n"), "ua")
	if err != nil || p.key != testKey || p.base.String() != API {
		t.Errorf("New = %v, %v", p, err)
	}
	for _, f := range []string{"", filepath.Join(dir, "missing"), write("empty", " \n"), write("two", "a\nb\n")} {
		if _, err := New(f, "ua"); err == nil {
			t.Errorf("New(%q) accepted", f)
		} else if strings.Contains(err.Error(), "a\nb") {
			t.Errorf("New(%q) error shows the file: %v", f, err)
		}
	}
}

func unavailable(err error, contains string) bool {
	u, ok := errors.AsType[*provider.UnavailableError](err)
	return ok && strings.Contains(u.Msg, contains)
}

// A prepaid plan, as measured 2026-10-16: no monthly limit, so no
// quota; each successful answer is billed and counted once, a 429 not.
func TestPrepaidPlan(t *testing.T) {
	measured := map[string]string{
		"X-RateLimit-Limit":     "50, 0",
		"X-RateLimit-Policy":    "50;w=1, 0;w=2678400",
		"X-RateLimit-Remaining": "49, 0",
		"X-RateLimit-Reset":     "1, 1304852",
	}
	limited := map[string]string{"X-RateLimit-Limit": "50, 0", "X-RateLimit-Remaining": "0, 0", "X-RateLimit-Reset": "1, 1304852"}
	h := newHarness(t, answer{200, measured, `{"web":{"results":[]}}`}, answer{429, limited, `{}`}, answer{200, measured, `{"grounding":{"generic":[]}}`})
	trace := &provider.Trace{}
	ctx := provider.WithTrace(context.Background(), trace)
	if _, err := h.p.Search(ctx, provider.SearchRequest{Query: "x", N: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.Context(ctx, provider.ContextRequest{Query: "x", Tokens: 1024, URLs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.p.Quota(); ok || len(trace.Quotas()) != 0 {
		t.Errorf("a quota on a plan without a monthly limit: %+v", trace.Quotas())
	}
	if h.p.Billed() != 2 || trace.Billed()["brave"] != 2 || !trace.Claim() || trace.Claim() {
		t.Errorf("billed %d", h.p.Billed())
	}
	if err := h.p.Health(context.Background()); err != nil {
		t.Errorf("health: %v", err)
	}
}
