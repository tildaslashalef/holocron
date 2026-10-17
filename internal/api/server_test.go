package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/guard"
	"github.com/tildaslashalef/holocron/internal/provider"
)

type fakeProvider struct {
	calls  atomic.Int32
	res    provider.Results
	err    error
	panics bool
	block  chan struct{} // when set, Search waits on it or its context
	health error
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Search(ctx context.Context, r provider.SearchRequest) (provider.Results, error) {
	p.calls.Add(1)
	if p.panics {
		panic("provider bug")
	}
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return provider.Results{}, &provider.TimeoutError{Msg: "slow"}
		}
	}
	return p.res, p.err
}

func (p *fakeProvider) Health(ctx context.Context) error { return p.health }

type fakeFetcher struct {
	calls atomic.Int32
	page  *fetch.Page
	err   error
	block chan struct{} // when set, the work ends (and done is called) only once it closes
}

func (f *fakeFetcher) Fetch(ctx context.Context, raw string, done func()) (*fetch.Page, error) {
	f.calls.Add(1)
	if f.block != nil {
		go func() { <-f.block; done() }()
		<-ctx.Done()
		return nil, &fetch.TimeoutError{Msg: "budget"}
	}
	defer done()
	if f.err != nil {
		return nil, f.err
	}
	p := *f.page
	p.URL = raw
	return &p, nil
}

type harness struct {
	srv *Server
	log *bytes.Buffer
	mu  *sync.Mutex
}

func newHarness(p provider.Provider, f Fetcher, logQueries bool) harness {
	var buf bytes.Buffer
	mu := &sync.Mutex{}
	log := slog.New(slog.NewTextHandler(lockedWriter{&buf, mu}, nil))
	return harness{NewServer("1.2.3", p, f, log, logQueries), &buf, mu}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (h harness) logged() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.log.String()
}

// do runs one request and decodes its JSON into v (nil: not decoded).
func (h harness) do(t *testing.T, method, target string, v any) *http.Response {
	t.Helper()
	return h.send(t, httptest.NewRequest(method, target, nil), v)
}

// send is do for a request built by the caller.
func (h harness) send(t *testing.T, req *http.Request, v any) *http.Response {
	t.Helper()
	target := req.URL.String()
	req.Header.Set("User-Agent", "nuclis/9")
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	resp := rec.Result()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("%s: Content-Type %q", target, ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("%s: Cache-Control %q", target, cc)
	}
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	return resp
}

// wantError checks a request's status and error code.
func (h harness) wantError(t *testing.T, target string, status int, code string) string {
	t.Helper()
	var eb ErrorBody
	resp := h.do(t, http.MethodGet, target, &eb)
	if resp.StatusCode != status || eb.Error.Code != code {
		t.Errorf("%s: %d %q (%s), want %d %q", target, resp.StatusCode, eb.Error.Code, eb.Error.Message, status, code)
	}
	return eb.Error.Message
}

func TestSearch(t *testing.T) {
	long := strings.Repeat("word ", 60)
	p := &fakeProvider{res: provider.Results{
		Items: []provider.Result{
			{Title: "  A \n title ", URL: "https://a.example/1", Snippet: "s\t1", Engine: "duckduckgo", Published: "2026-09-01"},
			{Title: "dup", URL: "https://a.example/1", Engine: "brave"},
			{Title: "ftp", URL: "ftp://a.example/", Engine: "brave"},
			{Title: "rel", URL: "/relative", Engine: "brave"},
			{Title: long, URL: "http://b.example/", Snippet: strings.Repeat("snip ", 200)},
		},
		EnginesFailed: []string{"mojeek"},
	}}
	h := newHarness(p, nil, true)
	var s Search
	h.do(t, http.MethodGet, "/v1/search?q=+zig+std.http.Client+&n=20", &s)
	if s.Query != "zig std.http.Client" || s.Provider != "fake" || len(s.Results) != 2 ||
		len(s.EnginesFailed) != 1 || s.EnginesFailed[0] != "mojeek" {
		t.Fatalf("search = %+v", s)
	}
	r0, r1 := s.Results[0], s.Results[1]
	if r0.Title != "A title" || r0.Snippet != "s 1" || r0.Engine != "duckduckgo" || r0.Published == nil || *r0.Published != "2026-09-01" {
		t.Errorf("result 0 = %+v", r0)
	}
	if len([]rune(r1.Title)) > MaxTitle || !strings.HasSuffix(r1.Title, "…") ||
		len([]rune(r1.Snippet)) > MaxSnippet || !strings.HasSuffix(r1.Snippet, "…") ||
		r1.Engine != "fake" || r1.Published != nil {
		t.Errorf("result 1 = %+v", r1)
	}

	var raw map[string]any
	h.do(t, http.MethodGet, "/v1/search?q=x&n=1", &raw)
	if res := raw["results"].([]any); len(res) != 1 || res[0].(map[string]any)["published"] != "2026-09-01" {
		t.Errorf("n=1: %v", raw)
	}
	if _, ok := raw["results"].([]any)[0].(map[string]any)["published"]; !ok {
		t.Error("published missing")
	}

	h.wantError(t, "/v1/search", 400, "bad_request")
	h.wantError(t, "/v1/search?q=++", 400, "bad_request")
	h.wantError(t, "/v1/search?q="+strings.Repeat("a", MaxQuery+1), 400, "bad_request")
	h.wantError(t, "/v1/search?q=x&n=0", 400, "bad_request")
	h.wantError(t, "/v1/search?q=x&n=many", 400, "bad_request")
	if !strings.Contains(h.logged(), `q="zig std.http.Client"`) || !strings.Contains(h.logged(), "ua=nuclis/9") {
		t.Errorf("log:\n%s", h.logged())
	}
}

func TestSearchEmptyAndFailures(t *testing.T) {
	var s Search
	h := newHarness(&fakeProvider{}, nil, true)
	if resp := h.do(t, http.MethodGet, "/v1/search?q=x", &s); resp.StatusCode != 200 || s.Results == nil || len(s.Results) != 0 || s.EnginesFailed == nil {
		t.Errorf("empty: %d %+v", resp.StatusCode, s)
	}
	h = newHarness(&fakeProvider{res: provider.Results{EnginesFailed: []string{"brave", "mojeek"}}}, nil, true)
	if msg := h.wantError(t, "/v1/search?q=x", 503, "no_engines"); !strings.Contains(msg, "brave, mojeek") {
		t.Errorf("message %q", msg)
	}
	h = newHarness(&fakeProvider{err: &provider.UnavailableError{Msg: "down"}}, nil, true)
	h.wantError(t, "/v1/search?q=x", 503, "no_engines")
	h = newHarness(&fakeProvider{err: &provider.TimeoutError{Msg: "slow"}}, nil, true)
	h.wantError(t, "/v1/search?q=x", 504, "upstream_timeout")
}

func TestFetch(t *testing.T) {
	text := "# Héading\n\n" + strings.Repeat("é", 50)
	f := &fakeFetcher{page: &fetch.Page{FinalURL: "https://example.org/a/", Status: 200, ContentType: "text/html", Title: "T", Text: text}}
	h := newHarness(&fakeProvider{}, f, true)
	chars := len([]rune(text))

	var got Fetch
	h.do(t, http.MethodGet, "/v1/fetch?url="+url.QueryEscape("https://example.org/a"), &got)
	if got.URL != "https://example.org/a" || got.FinalURL != "https://example.org/a/" || got.Status != 200 || got.ContentType != "text/html" ||
		got.Title != "T" || got.Text != text || got.Start != 0 || got.Chars != chars || got.Truncated {
		t.Errorf("whole = %+v", got)
	}
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/a&max_chars=5", &got)
	if got.Text != "# Héa" || got.Chars != chars || !got.Truncated {
		t.Errorf("first 5 = %+v", got)
	}
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/a&start=5&max_chars=6", &got)
	if got.Text != "ding\n\n" || got.Start != 5 || !got.Truncated {
		t.Errorf("start 5 = %q %+v", got.Text, got)
	}
	h.do(t, http.MethodGet, fmt.Sprintf("/v1/fetch?url=https://example.org/a&start=%d", chars-3), &got)
	if got.Text != "ééé" || got.Truncated {
		t.Errorf("tail = %+v", got)
	}
	h.do(t, http.MethodGet, fmt.Sprintf("/v1/fetch?url=https://example.org/a&start=%d", chars+10), &got)
	if got.Text != "" || got.Chars != chars || got.Truncated {
		t.Errorf("past the end = %+v", got)
	}
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/a&max_chars=999999999", &got)
	if got.Text != text {
		t.Errorf("clamped max_chars = %+v", got)
	}
	f.page.Truncated = true
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/cut", &got)
	if !got.Truncated {
		t.Error("a body cut at 5 MiB is not truncated")
	}

	h.wantError(t, "/v1/fetch", 400, "bad_request")
	h.wantError(t, "/v1/fetch?url=ftp://example.org/", 400, "bad_request")
	h.wantError(t, "/v1/fetch?url=https://example.org:8443/", 400, "bad_request")
	h.wantError(t, "/v1/fetch?url=https://example.org/&start=-1", 400, "bad_request")
	h.wantError(t, "/v1/fetch?url=https://example.org/&max_chars=0", 400, "bad_request")
	h.wantError(t, "/v1/fetch?url=http://192.168.1.20:8443/", 403, "blocked_address")
	h.wantError(t, "/v1/fetch?url=http://nas.home.arpa:8443/", 403, "blocked_address")
}

func TestFetchErrors(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{&guard.BlockedError{Msg: "x resolves to 10.0.0.1"}, 403, "blocked_address"},
		{&fetch.UnsupportedError{Msg: "pdf"}, 415, "unsupported_content"},
		{&fetch.UpstreamError{Msg: "404"}, 502, "upstream_failed"},
		{&fetch.TimeoutError{Msg: "slow"}, 504, "upstream_timeout"},
		{&fetch.PanicError{Value: "bug", Stack: []byte("stack")}, 500, "internal"},
		{fmt.Errorf("unexpected"), 500, "internal"},
	} {
		h := newHarness(&fakeProvider{}, &fakeFetcher{err: tt.err}, true)
		h.wantError(t, "/v1/fetch?url=https://example.org/", tt.status, tt.code)
		if tt.code == "internal" && !strings.Contains(h.logged(), "level=ERROR") {
			t.Errorf("%v: not logged as an error:\n%s", tt.err, h.logged())
		}
	}
}

func TestRouting(t *testing.T) {
	h := newHarness(&fakeProvider{}, nil, true)
	var eb ErrorBody
	resp := h.do(t, http.MethodPost, "/v1/search?q=x", &eb)
	if resp.StatusCode != 405 || eb.Error.Code != "method_not_allowed" || resp.Header.Get("Allow") != "GET" {
		t.Errorf("POST: %d %+v", resp.StatusCode, eb)
	}
	resp = h.do(t, http.MethodHead, "/v1/health", nil)
	if resp.StatusCode != 405 {
		t.Errorf("HEAD: %d", resp.StatusCode)
	}
	for _, p := range []string{"/", "/v1", "/v1/search/", "/v2/search", "/v1/nope"} {
		h.wantError(t, p, 404, "not_found")
	}
}

func TestHealth(t *testing.T) {
	p := &fakeProvider{}
	h := newHarness(p, nil, true)
	var got Health
	h.do(t, http.MethodGet, "/v1/health", &got)
	if got.Version != "1.2.3" || got.Providers["fake"] != "ok" {
		t.Errorf("health = %+v", got)
	}
	p.health = fmt.Errorf("down")
	h.do(t, http.MethodGet, "/v1/health", &got)
	if got.Providers["fake"] != "down" {
		t.Errorf("health = %+v", got)
	}
}

// Every slot taken: search and fetch answer busy after the wait; health
// still answers at once.
func TestSlots(t *testing.T) {
	block := make(chan struct{})
	p := &fakeProvider{block: block}
	h := newHarness(p, nil, true)
	h.srv.slotWait = 100 * time.Millisecond
	var wg sync.WaitGroup
	for i := range Slots {
		wg.Go(func() {
			// Distinct queries: identical ones would share one call (§8.1).
			h.srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/search?q=x%d", i), nil))
		})
	}
	waitFor(t, func() bool { return len(h.srv.slots) == Slots })

	start := time.Now()
	h.wantError(t, "/v1/search?q=y", 503, "busy")
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("busy after %v, before the wait", d)
	}
	start = time.Now()
	var got Health
	h.do(t, http.MethodGet, "/v1/health", &got)
	if time.Since(start) > 50*time.Millisecond || got.Providers["fake"] != "ok" {
		t.Errorf("health waited %v: %+v", time.Since(start), got)
	}
	close(block)
	wg.Wait()
	if len(h.srv.slots) != 0 {
		t.Errorf("%d slots still held", len(h.srv.slots))
	}
}

// A fetch whose extraction outlived its request keeps its slot until the
// extraction ends.
func TestSlotHeldByAbandonedExtraction(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(&fakeProvider{}, &fakeFetcher{block: block}, true)
	h.srv.slotWait = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/fetch?url=https://example.org/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != 504 {
		t.Fatalf("status %d", rec.Code)
	}
	if len(h.srv.slots) != 1 {
		t.Fatalf("%d slots held after the answer, want 1", len(h.srv.slots))
	}
	close(block)
	waitFor(t, func() bool { return len(h.srv.slots) == 0 })
}

func TestPanicRecovered(t *testing.T) {
	p := &fakeProvider{panics: true}
	h := newHarness(p, nil, true)
	h.wantError(t, "/v1/search?q=x", 500, "internal")
	if !strings.Contains(h.logged(), "provider bug") || !strings.Contains(h.logged(), "level=ERROR") {
		t.Errorf("log:\n%s", h.logged())
	}
	if len(h.srv.slots) != 0 {
		t.Errorf("the panic kept a slot")
	}
	p.panics = false
	var s Search
	if resp := h.do(t, http.MethodGet, "/v1/search?q=x", &s); resp.StatusCode != 200 {
		t.Errorf("after the panic: %d", resp.StatusCode)
	}
}

func TestLogQueriesOff(t *testing.T) {
	f := &fakeFetcher{page: &fetch.Page{FinalURL: "https://example.org/secret/final", Status: 200, ContentType: "text/plain", Text: "x"}}
	h := newHarness(&fakeProvider{}, f, false)
	h.do(t, http.MethodGet, "/v1/search?q=private+words", nil)
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/secret/path", nil)
	log := h.logged()
	for _, leak := range []string{"private", "secret"} {
		if strings.Contains(log, leak) {
			t.Errorf("log has %q:\n%s", leak, log)
		}
	}
	if !strings.Contains(log, "host=example.org") || !strings.Contains(log, "endpoint=/v1/fetch") {
		t.Errorf("log:\n%s", log)
	}
}

func TestLogLine(t *testing.T) {
	f := &fakeFetcher{page: &fetch.Page{FinalURL: "https://example.org/b/", Status: 200, ContentType: "text/plain", Text: "x"}}
	h := newHarness(&fakeProvider{}, f, true)
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/b", nil)
	h.wantError(t, "/v1/fetch?url=http://10.0.0.1/", 403, "blocked_address")
	log := h.logged()
	for _, want := range []string{
		"msg=request endpoint=/v1/fetch status=200", "host=example.org", "url=https://example.org/b",
		"final_url=https://example.org/b/", "ua=nuclis/9",
		"status=403 code=blocked_address", "url=http://10.0.0.1/",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestNewHTTPServer(t *testing.T) {
	s := NewHTTPServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 10*time.Second || s.WriteTimeout != 35*time.Second ||
		s.IdleTimeout != 60*time.Second || s.MaxHeaderBytes != 16<<10 {
		t.Errorf("server = %+v", s)
	}
}

func TestWindow(t *testing.T) {
	for _, tt := range []struct {
		s        string
		start, n int
		want     string
		chars    int
	}{
		{"héllo", 0, 2, "hé", 5},
		{"héllo", 1, 10, "éllo", 5},
		{"héllo", 5, 1, "", 5},
		{"", 0, 1, "", 0},
		{"日本語", 2, 1, "語", 3},
	} {
		got, chars := window(tt.s, tt.start, tt.n)
		if got != tt.want || chars != tt.chars {
			t.Errorf("window(%q, %d, %d) = %q, %d", tt.s, tt.start, tt.n, got, chars)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// §10: the level says whose fault a failure is, and the line carries the
// answer's message while --log-queries is on.
func TestLogLevels(t *testing.T) {
	for _, tt := range []struct {
		err   error
		level string
	}{
		{&fetch.UpstreamError{Msg: "example.org answered 403 Forbidden"}, "WARN"},
		{&fetch.TimeoutError{Msg: "slow"}, "WARN"},
		{&guard.BlockedError{Msg: "example.org resolves to 10.0.0.1, not a public address"}, "WARN"},
		{&fetch.UnsupportedError{Msg: "example.org is application/pdf"}, "INFO"},
	} {
		for _, logQueries := range []bool{true, false} {
			h := newHarness(&fakeProvider{}, &fakeFetcher{err: tt.err}, logQueries)
			h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/", nil)
			log := h.logged()
			msg := Classify(tt.err).Message
			if !strings.Contains(log, "level="+tt.level+" ") || strings.Contains(log, msg) != logQueries {
				t.Errorf("%v (log queries %v):\n%s", tt.err, logQueries, log)
			}
		}
	}
	h := newHarness(&fakeProvider{}, nil, true)
	h.do(t, http.MethodGet, "/v1/search?q=", nil)
	h.do(t, http.MethodGet, "/v1/nope", nil)
	if log := h.logged(); strings.Contains(log, "level=WARN") || !strings.Contains(log, `error="q is empty"`) {
		t.Errorf("client mistakes:\n%s", log)
	}
	h = newHarness(&fakeProvider{err: &provider.UnavailableError{Msg: "searxng: down"}}, nil, true)
	h.do(t, http.MethodGet, "/v1/search?q=x", nil)
	if log := h.logged(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, `error="searxng: down"`) {
		t.Errorf("no_engines:\n%s", log)
	}
}
