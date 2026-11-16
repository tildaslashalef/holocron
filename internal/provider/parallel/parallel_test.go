package parallel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/guard"
	"github.com/tildaslashalef/holocron/internal/provider"
)

const testKey = "par-secret-key-123"

type answer struct {
	status  int
	headers map[string]string
	body    string
}

// fakeParallel answers each request with the next answer, recording the
// requests and their bodies.
type fakeParallel struct {
	mu      sync.Mutex
	answers []answer
	paths   []string
	bodies  []map[string]any
	headers []http.Header
}

func (f *fakeParallel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	a := answer{status: 500, body: `{"type":"error","error":{"message":"no answer left"}}`}
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

type harness struct {
	p     *Provider
	fake  *fakeParallel
	clock time.Time
	slept []time.Duration
}

// publicResolver answers every name with a public address, and
// "private.example" with a private one.
func publicResolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("no DNS in tests")
	}}
}

func newHarness(t *testing.T, answers ...answer) *harness {
	t.Helper()
	f := &fakeParallel{answers: answers}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	h := &harness{fake: f, clock: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	p, err := newProvider(srv.URL, testKey, Options{UserAgent: "holocron/test"})
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return h.clock }
	p.sleep = func(_ context.Context, d time.Duration) error { h.slept = append(h.slept, d); return nil }
	h.p = p
	return h
}

const searchAnswer = `{"search_id":"s1","results":[
 {"url":"https://ziglang.org/a","title":"Zig http","publish_date":"2026-09-01","excerpts":["# Client\n\nThe HTTP client supports proxies through http_proxy and https_proxy fields.\n\nshort\n\nConnections are pooled and reused between requests to the same host."]},
 {"url":"https://example.org/b","title":null,"publish_date":"2026-11-10","excerpts":["Another page with a long enough passage to be kept as a snippet."]},
 {"url":"https://example.org/old","title":"Old","publish_date":null,"excerpts":[]}
],"usage":[{"name":"sku_search","count":1}]}`

func TestSearch(t *testing.T) {
	h := newHarness(t, answer{200, nil, searchAnswer})
	trace := &provider.Trace{}
	res, err := h.p.Search(provider.WithTrace(context.Background(), trace), provider.SearchRequest{Query: "zig std.http.Client proxy", N: 10, Freshness: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("items %+v", res.Items)
	}
	a := res.Items[0]
	if a.Title != "Zig http" || a.Published != "2026-09-01" || !strings.HasPrefix(a.Snippet, "The HTTP client supports proxies") ||
		len(a.Snippets) != 1 || !strings.HasPrefix(a.Snippets[0], "Connections are pooled") {
		t.Errorf("first = %+v", a)
	}
	if res.Items[1].Title != "" || res.Items[2].Snippet != "" {
		t.Errorf("null title or no excerpts: %+v", res.Items[1:])
	}
	body := h.fake.bodies[0]
	settings := body["advanced_settings"].(map[string]any)
	if h.fake.paths[0] != "POST /v1/search" || body["mode"] != "fast" || body["objective"] != "zig std.http.Client proxy" ||
		body["search_queries"].([]any)[0] != "zig std.http.Client proxy" || settings["max_results"] != 10.0 ||
		settings["source_policy"].(map[string]any)["after_date"] != "2026-11-09" || body["max_chars_total"] != 30000.0 {
		t.Errorf("request %v", body)
	}
	hd := h.fake.headers[0]
	if hd.Get("X-Api-Key") != testKey || hd.Get("Content-Type") != "application/json" || hd.Get("User-Agent") != "holocron/test" {
		t.Errorf("headers %v", hd)
	}
	if b := h.p.Billed(); b != (provider.Bill{Units: 1, Micros: priceFast}) || trace.Cost() != priceFast {
		t.Errorf("billed %+v, trace %d", b, trace.Cost())
	}
}

func TestSearchDateRangeAndLongQuery(t *testing.T) {
	h := newHarness(t, answer{200, nil, searchAnswer})
	long := strings.Repeat("word ", 60)
	res, err := h.p.Search(context.Background(), provider.SearchRequest{Query: long, N: 3, Freshness: "2026-08-01to2026-10-01"})
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-10 is after the range's end and dropped; no date is kept.
	if len(res.Items) != 2 || res.Items[0].URL != "https://ziglang.org/a" || res.Items[1].URL != "https://example.org/old" {
		t.Errorf("items %+v", res.Items)
	}
	body := h.fake.bodies[0]
	q := body["search_queries"].([]any)[0].(string)
	if len(q) > maxQuery || strings.HasSuffix(q, " ") || body["advanced_settings"].(map[string]any)["source_policy"].(map[string]any)["after_date"] != "2026-08-01" {
		t.Errorf("query %q, body %v", q, body)
	}
}

func TestContext(t *testing.T) {
	h := newHarness(t, answer{200, nil, searchAnswer})
	res, err := h.p.Context(context.Background(), provider.ContextRequest{Query: "zig proxies", Tokens: 2048, URLs: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sources) != 3 || len(res.Sources[0].Snippets) != 1 || !strings.Contains(res.Sources[0].Snippets[0], "\n\n") {
		t.Errorf("sources %+v (excerpts are kept whole)", res.Sources)
	}
	body := h.fake.bodies[0]
	if body["mode"] != "advanced" || body["max_chars_total"] != 8192.0 || body["advanced_settings"].(map[string]any)["max_results"] != 5.0 {
		t.Errorf("request %v", body)
	}
	if _, ok := body["advanced_settings"].(map[string]any)["source_policy"]; ok {
		t.Error("source_policy without freshness")
	}
	if b := h.p.Billed(); b.Micros != priceAdvanced {
		t.Errorf("billed %+v", b)
	}
}

func TestExtract(t *testing.T) {
	h := newHarness(t,
		answer{200, nil, `{"extract_id":"e1","results":[{"url":"https://93.184.215.14/a","title":"A page","publish_date":"2024-04-10","excerpts":["x"],"full_content":"# A page\n\nThe text."}],"errors":[],"usage":[{"name":"sku_extract_excerpts","count":1}]}`},
		answer{200, nil, `{"results":[{"url":"https://93.184.215.14/a","title":"A page","excerpts":["first part","second part"],"full_content":null}],"errors":[],"usage":[{"name":"sku_extract_excerpts","count":1}]}`},
	)
	page, err := h.p.Extract(context.Background(), provider.ExtractRequest{URL: "https://93.184.215.14/a"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Text != "# A page\n\nThe text." || page.Title != "A page" || page.Published != "2024-04-10" || page.Focused || page.Status != 0 || page.ContentType != "" {
		t.Errorf("page %+v", page)
	}
	settings := h.fake.bodies[0]["advanced_settings"].(map[string]any)
	if _, ok := h.fake.bodies[0]["objective"]; ok || settings["full_content"].(map[string]any)["max_chars_per_result"] != float64(MaxFullContent) {
		t.Errorf("request %v", h.fake.bodies[0])
	}
	page, err = h.p.Extract(context.Background(), provider.ExtractRequest{URL: "https://93.184.215.14/a", Focus: "the second part"})
	if err != nil || page.Text != "first part\n\nsecond part" || !page.Focused {
		t.Fatalf("focused %+v, %v", page, err)
	}
	if b := h.fake.bodies[1]; b["objective"] != "the second part" || b["advanced_settings"].(map[string]any)["full_content"] != false {
		t.Errorf("focused request %v", b)
	}
	if b := h.p.Billed(); b != (provider.Bill{Units: 2, Micros: 2 * priceExtract}) {
		t.Errorf("billed %+v", b)
	}
}

func TestExtractFailures(t *testing.T) {
	h := newHarness(t,
		answer{200, nil, `{"results":[],"errors":[{"url":"u","error_type":"http_error","http_status_code":404,"content":null}]}`},
		answer{200, nil, `{"results":[],"errors":[{"url":"u","error_type":"http_error","http_status_code":403,"content":null}]}`},
		answer{200, nil, `{"results":[{"url":"u","title":"t","excerpts":[],"full_content":"   "}],"errors":[]}`},
	)
	ctx := context.Background()
	_, err := h.p.Extract(ctx, provider.ExtractRequest{URL: "https://93.184.215.14/gone"})
	if u, ok := errors.AsType[*fetch.UpstreamError](err); !ok || !strings.Contains(u.Msg, "404") {
		t.Errorf("404: %v", err)
	}
	for _, name := range []string{"403", "empty"} {
		_, err = h.p.Extract(ctx, provider.ExtractRequest{URL: "https://93.184.215.14/x"})
		if u, ok := errors.AsType[*provider.UnavailableError](err); !ok || u.Reason != provider.ReasonUnreadable {
			t.Errorf("%s: %v (local should be asked next)", name, err)
		}
	}
	// A private literal, or a name the resolver cannot answer, is never sent.
	n := len(h.fake.paths)
	_, err = h.p.Extract(ctx, provider.ExtractRequest{URL: "http://192.168.1.20/"})
	h.p.resolver = publicResolver()
	_, err2 := h.p.Extract(ctx, provider.ExtractRequest{URL: "https://nowhere.example/"})
	if len(h.fake.paths) != n {
		t.Errorf("sent %v", h.fake.paths[n:])
	}
	if _, ok := errors.AsType[*guard.BlockedError](err); !ok {
		t.Errorf("a private literal: %v", err)
	}
	if _, ok := errors.AsType[*fetch.UpstreamError](err2); !ok {
		t.Errorf("unresolvable: %v", err2)
	}
}

func TestFailures(t *testing.T) {
	ctx := context.Background()
	req := provider.SearchRequest{Query: "x", N: 5}
	for _, tt := range []struct {
		name   string
		answer answer
		reason string
		msg    string
		held   bool
	}{
		{"bad key", answer{401, nil, `{"code":16,"message":"Invalid API key (C.1)"}`}, provider.ReasonKeyRefused, "parallel refused the key (401): Invalid API key (C.1)", true},
		{"no credit", answer{402, nil, `{"type":"error","error":{"message":"Insufficient credit"}}`}, provider.ReasonQuota, "parallel: credit used up: Insufficient credit", true},
		{"rate limited", answer{429, map[string]string{"Retry-After": "30"}, `{}`}, provider.ReasonRateLimited, "parallel: rate-limited", false},
		{"validation", answer{422, nil, `{"type":"error","error":{"message":"Request validation error","detail":{"loc":"mode"}}}`}, "", `parallel answered 422: Request validation error {"loc":"mode"}`, false},
		{"server", answer{503, nil, `oops`}, "", "parallel answered 503", false},
		{"redirect", answer{302, map[string]string{"Location": "https://elsewhere.example/"}, ``}, "", "parallel answered 302", false},
	} {
		h := newHarness(t, tt.answer, answer{200, nil, searchAnswer})
		_, err := h.p.Search(ctx, req)
		u, ok := errors.AsType[*provider.UnavailableError](err)
		if !ok || u.Reason != tt.reason || u.Msg != tt.msg {
			t.Errorf("%s: %#v", tt.name, err)
		}
		if strings.Contains(fmt.Sprint(err), testKey) {
			t.Errorf("%s: the key in the error", tt.name)
		}
		if held := h.p.Health(ctx) != nil; held != tt.held {
			t.Errorf("%s: held %v", tt.name, held)
		}
		if tt.held {
			// Held: no call until the hold ends.
			if _, err := h.p.Search(ctx, req); err == nil || len(h.fake.paths) != 1 {
				t.Errorf("%s: called while held", tt.name)
			}
			h.clock = h.clock.Add(2 * time.Hour)
			if _, err := h.p.Search(ctx, req); err != nil {
				t.Errorf("%s: after the hold: %v", tt.name, err)
			}
		}
		if h.p.Billed().Units > 1 {
			t.Errorf("%s: billed a failure", tt.name)
		}
	}
}

func TestRetryOnce(t *testing.T) {
	h := newHarness(t, answer{429, map[string]string{"Retry-After": "1"}, `{}`}, answer{200, nil, searchAnswer})
	if _, err := h.p.Search(context.Background(), provider.SearchRequest{Query: "x", N: 5}); err != nil {
		t.Fatal(err)
	}
	if len(h.slept) != 1 || h.slept[0] != time.Second || len(h.fake.paths) != 2 {
		t.Errorf("slept %v, %d calls", h.slept, len(h.fake.paths))
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer srv.Close()
	p, _ := newProvider(srv.URL, testKey, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Search(ctx, provider.SearchRequest{Query: "x"})
	if _, ok := errors.AsType[*provider.TimeoutError](err); !ok {
		t.Errorf("err %v", err)
	}
}

func TestNew(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	os.WriteFile(key, []byte(testKey+"\n"), 0o600)
	if p, err := New(key, Options{}); err != nil || p.key != testKey {
		t.Errorf("New: %v", err)
	}
	if _, err := New("", Options{}); err == nil || !strings.Contains(err.Error(), "parallel-key") {
		t.Errorf("no key: %v", err)
	}
	if _, err := New(key, Options{SearchMode: "warp"}); err == nil || !strings.Contains(err.Error(), "warp") {
		t.Errorf("mode: %v", err)
	}
	p, _ := New(key, Options{SearchMode: ModeTurbo})
	if s, c := p.Modes(); s != "turbo" || c != "advanced" {
		t.Errorf("modes %s %s", s, c)
	}
}

func TestWindowAndPassages(t *testing.T) {
	now := time.Date(2026, 3, 31, 23, 0, 0, 0, time.UTC)
	for f, want := range map[string][2]string{
		"": {"", ""}, "pd": {"2026-03-30", ""}, "pw": {"2026-03-24", ""}, "pm": {"2026-03-03", ""}, "py": {"2025-03-31", ""},
		"2026-01-01to2026-02-01": {"2026-01-01", "2026-02-01"},
	} {
		if a, b := window(f, now); a != want[0] || b != want[1] {
			t.Errorf("window(%q) = %q %q", f, a, b)
		}
	}
	got := passages([]string{"## Title\n\n...\n\nok", "[...]"})
	if len(got) != 2 || got[0] != "Title" || got[1] != "ok" {
		t.Errorf("passages of short parts: %q", got)
	}
	if clip("hello world", 8) != "hello" || clip("héllo", 10) != "héllo" {
		t.Error("clip")
	}
}
