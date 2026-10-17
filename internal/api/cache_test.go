package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/tildaslashalef/holocron/internal/provider/searxng"
)

// clock is an injected now for the cache.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (h harness) clock() *clock {
	c := &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	h.srv.cache.now = c.now
	return c
}

func results(n int) provider.Results {
	var res provider.Results
	for i := range n {
		res.Items = append(res.Items, provider.Result{Title: fmt.Sprintf("r%d", i), URL: fmt.Sprintf("https://example.org/%d", i), Engine: "e"})
	}
	return res
}

// wantCache checks a response's X-Search-Cache and Age ("" for none).
func wantCache(t *testing.T, resp *http.Response, how, age string) {
	t.Helper()
	if got := resp.Header.Get("X-Search-Cache"); got != how {
		t.Errorf("X-Search-Cache %q, want %q", got, how)
	}
	if got, ok := resp.Header["Age"]; (age == "") != !ok || (ok && got[0] != age) {
		t.Errorf("Age %v, want %q", got, age)
	}
}

func jsonDecode(rec *httptest.ResponseRecorder, v any) error {
	return json.Unmarshal(rec.Body.Bytes(), v)
}

func refresh(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Cache-Control", "max-age=0, No-Cache")
	return req
}

func TestSearchCacheHitMiss(t *testing.T) {
	p := &fakeProvider{res: results(12)}
	h := newHarness(p, nil, true)
	c := h.clock()
	var s Search
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=Zig+std.http&n=2", &s), "miss", "")
	if len(s.Results) != 2 || s.Query != "Zig std.http" {
		t.Errorf("miss: %+v", s)
	}
	c.add(90 * time.Second)
	// The same query in another spelling, and a larger n: a slice of the
	// 10 kept.
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=++zig%0A+STD.http+&n=20", &s), "hit", "90")
	if len(s.Results) != MaxN || s.Query != "zig\n STD.http" || s.Results[9].URL != "https://example.org/9" {
		t.Errorf("hit: %+v", s)
	}
	if p.calls.Load() != 1 {
		t.Errorf("%d provider calls, want 1", p.calls.Load())
	}
	// Validation comes first: no cache header on its errors.
	resp := h.do(t, http.MethodGet, "/v1/search?q=zig+std.http&n=0", nil)
	if resp.StatusCode != 400 || resp.Header.Get("X-Search-Cache") != "" {
		t.Errorf("n=0: %d %v", resp.StatusCode, resp.Header)
	}
	if log := h.logged(); !strings.Contains(log, "cache=miss") || !strings.Contains(log, "cache=hit") {
		t.Errorf("log:\n%s", log)
	}
	var hl Health
	h.do(t, http.MethodGet, "/v1/health", &hl)
	if hl.Cache == nil || hl.Cache.Entries != 1 || hl.Cache.Bytes <= entryOverhead {
		t.Errorf("health cache = %+v", hl.Cache)
	}
}

func TestSearchCacheTTL(t *testing.T) {
	p := &fakeProvider{res: results(3)}
	h := newHarness(p, nil, true)
	c := h.clock()
	h.do(t, http.MethodGet, "/v1/search?q=x", nil)
	c.add(SearchTTL - time.Second)
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=x", nil), "hit", "1799")
	c.add(time.Second)
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=x", nil), "miss", "")
	if p.calls.Load() != 2 {
		t.Errorf("%d provider calls, want 2", p.calls.Load())
	}

	// Engines failed: 5 minutes.
	p.res.EnginesFailed = []string{"brave"}
	h.do(t, http.MethodGet, "/v1/search?q=partial", nil)
	c.add(SearchPartialTTL - time.Second)
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=partial", nil), "hit", "299")
	c.add(time.Second)
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=partial", nil), "miss", "")

	// Failures are not kept.
	for _, err := range []error{&provider.UnavailableError{Msg: "down"}, &provider.TimeoutError{Msg: "slow"}} {
		p := &fakeProvider{err: err}
		h := newHarness(p, nil, true)
		h.do(t, http.MethodGet, "/v1/search?q=x", nil)
		h.do(t, http.MethodGet, "/v1/search?q=x", nil)
		if p.calls.Load() != 2 {
			t.Errorf("%v: %d provider calls, want 2", err, p.calls.Load())
		}
	}
}

func TestSearchNoCache(t *testing.T) {
	p := &fakeProvider{res: results(3)}
	h := newHarness(p, nil, true)
	h.clock()
	h.do(t, http.MethodGet, "/v1/search?q=x", nil)
	p.res = results(5)
	var s Search
	wantCache(t, h.send(t, refresh("/v1/search?q=x"), &s), "miss", "")
	if len(s.Results) != 5 || p.calls.Load() != 2 {
		t.Errorf("no-cache: %d results, %d calls", len(s.Results), p.calls.Load())
	}
	// The entry was refreshed.
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=x", &s), "hit", "0")
	if len(s.Results) != 5 {
		t.Errorf("after the refresh: %d results", len(s.Results))
	}
}

// An expired search is answered, marked stale, only instead of no_engines,
// and only within 24 hours.
func TestSearchStaleIfError(t *testing.T) {
	p := &fakeProvider{res: results(3)}
	h := newHarness(p, nil, true)
	c := h.clock()
	h.do(t, http.MethodGet, "/v1/search?q=x", nil)
	c.add(time.Hour)

	var s Search
	p.res, p.err = provider.Results{}, &provider.UnavailableError{Msg: "searxng is unreachable"}
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=X&n=2", &s), "stale", "3600")
	if len(s.Results) != 2 || s.Query != "X" {
		t.Errorf("stale: %+v", s)
	}
	// No results while engines failed is no_engines too.
	p.res, p.err = provider.Results{EnginesFailed: []string{"brave", "duckduckgo"}}, nil
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=x", &s), "stale", "3600")

	// Not for another error.
	p.res, p.err = provider.Results{}, &provider.TimeoutError{Msg: "slow"}
	wantCache(t, h.do(t, http.MethodGet, "/v1/search?q=x", nil), "miss", "")
	h.wantError(t, "/v1/search?q=x", 504, "upstream_timeout")
	// Not with no-cache.
	p.err = &provider.UnavailableError{Msg: "down"}
	resp := h.send(t, refresh("/v1/search?q=x"), nil)
	if resp.StatusCode != 503 {
		t.Errorf("no-cache: %d, want no_engines", resp.StatusCode)
	}
	// Not past 24 hours.
	c.add(StaleMax - time.Hour)
	h.wantError(t, "/v1/search?q=x", 503, "no_engines")
}

// Concurrent identical searches share one SearXNG call, even when they
// skip the lookup.
func TestSearchSharedCall(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	sx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		fmt.Fprint(w, `{"results": [{"url": "https://ziglang.org/", "title": "Zig", "content": "c", "engine": "brave"}], "unresponsive_engines": []}`)
	}))
	defer sx.Close()
	p, err := searxng.New(sx.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(p, nil, true)
	key := searchKey("searxng", "zig", "")
	var wg sync.WaitGroup
	got := make([]Search, 2)
	for i, q := range []string{"zig", "ZIG"} {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			h.srv.ServeHTTP(rec, refresh("/v1/search?q="+q))
			if rec.Code != 200 || rec.Header().Get("X-Search-Cache") != "miss" {
				t.Errorf("%s: %d %v", q, rec.Code, rec.Header())
			}
			if err := jsonDecode(rec, &got[i]); err != nil {
				t.Error(err)
			}
		})
	}
	waitFor(t, func() bool { return h.srv.cache.searchCalls.Waiting(key) == 2 })
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d SearXNG calls, want 1", calls.Load())
	}
	if got[0].Query != "zig" || got[1].Query != "ZIG" || len(got[1].Results) != 1 {
		t.Errorf("answers: %+v", got)
	}
	if len(h.srv.slots) != 0 {
		t.Errorf("%d slots held", len(h.srv.slots))
	}
}

// Paging through a document fetches it once.
func TestFetchCachePaging(t *testing.T) {
	text := strings.Repeat("abcdefghij", 600)
	f := &fakeFetcher{page: &fetch.Page{FinalURL: "https://example.org/doc/", Status: 200, ContentType: "text/html", Title: "Doc", Text: text}}
	h := newHarness(&fakeProvider{}, f, true)
	c := h.clock()
	var pages []string
	for i, start := range []int{0, 2000, 4000} {
		var got Fetch
		resp := h.do(t, http.MethodGet, fmt.Sprintf("/v1/fetch?url=https://example.org/doc&start=%d&max_chars=2000", start), &got)
		if i == 0 {
			wantCache(t, resp, "miss", "")
		} else {
			wantCache(t, resp, "hit", fmt.Sprint(i*10))
		}
		if got.Start != start || got.Chars != 6000 || got.Truncated != (i < 2) || got.URL != "https://example.org/doc" || got.Title != "Doc" {
			t.Errorf("page %d: %+v", i, got)
		}
		pages = append(pages, got.Text)
		c.add(10 * time.Second)
	}
	if strings.Join(pages, "") != text {
		t.Error("the pages do not make the document")
	}
	if f.calls.Load() != 1 {
		t.Errorf("%d upstream fetches, want 1", f.calls.Load())
	}

	// Another spelling of the URL: scheme and host case, a fragment. The
	// answer names this request's URL.
	var got Fetch
	raw := "HTTPS://Example.ORG/doc#part-2"
	wantCache(t, h.do(t, http.MethodGet, "/v1/fetch?url="+url.QueryEscape(raw)+"&max_chars=5", &got), "hit", "30")
	if got.URL != raw || got.Text != "abcde" || f.calls.Load() != 1 {
		t.Errorf("respelled: %+v, %d fetches", got, f.calls.Load())
	}
	// The path's case and the query are part of the key.
	for _, u := range []string{"https://example.org/DOC", "https://example.org/doc?v=2"} {
		wantCache(t, h.do(t, http.MethodGet, "/v1/fetch?url="+url.QueryEscape(u), nil), "miss", "")
	}

	// 15 minutes fresh.
	c.add(FetchTTL - 30*time.Second)
	wantCache(t, h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/doc", nil), "miss", "")
	// no-cache refetches.
	before := f.calls.Load()
	wantCache(t, h.send(t, refresh("/v1/fetch?url=https://example.org/doc"), nil), "miss", "")
	if f.calls.Load() != before+1 {
		t.Error("no-cache did not fetch")
	}
	if !strings.Contains(h.logged(), "cache=hit") {
		t.Errorf("log:\n%s", h.logged())
	}
}

func TestFetchCacheFailures(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
		kept   bool
	}{
		{&fetch.UpstreamError{Msg: "example.org answered 404 Not Found"}, 502, "upstream_failed", true},
		{&fetch.UnsupportedError{Msg: "pdf"}, 415, "unsupported_content", true},
		{&fetch.TimeoutError{Msg: "slow"}, 504, "upstream_timeout", false},
		{&guard.BlockedError{Msg: "example.org resolves to 10.0.0.1"}, 403, "blocked_address", false},
		{fmt.Errorf("bug"), 500, "internal", false},
	} {
		f := &fakeFetcher{err: tt.err}
		h := newHarness(&fakeProvider{}, f, true)
		c := h.clock()
		target := "/v1/fetch?url=https://example.org/x"
		h.wantError(t, target, tt.status, tt.code)
		c.add(FailTTL - time.Second)
		var eb ErrorBody
		resp := h.do(t, http.MethodGet, target, &eb)
		if resp.StatusCode != tt.status || eb.Error.Code != tt.code {
			t.Errorf("%s again: %d %s", tt.code, resp.StatusCode, eb.Error.Code)
		}
		if tt.kept {
			wantCache(t, resp, "hit", "59")
		} else {
			wantCache(t, resp, "miss", "")
		}
		if want := map[bool]int32{true: 1, false: 2}[tt.kept]; f.calls.Load() != want {
			t.Errorf("%s: %d fetches, want %d", tt.code, f.calls.Load(), want)
		}
		if tt.kept {
			c.add(time.Second)
			h.wantError(t, target, tt.status, tt.code)
			if f.calls.Load() != 2 {
				t.Errorf("%s: kept past a minute", tt.code)
			}
		}
	}
}

// Validation runs before the lookup: whatever the cache holds, a URL step
// 1 refuses is refused, with no cache header.
func TestFetchCacheRefusedURL(t *testing.T) {
	f := &fakeFetcher{page: &fetch.Page{FinalURL: "https://example.org/", Status: 200, ContentType: "text/plain", Text: "public"}}
	h := newHarness(&fakeProvider{}, f, true)
	h.clock()
	h.do(t, http.MethodGet, "/v1/fetch?url=https://example.org/", nil)
	// Entries a refused URL would map to, as if stored by mistake.
	for _, raw := range []string{"http://192.168.1.20/", "https://example.org:8443/", "http://nas.home.arpa/"} {
		u, _ := url.Parse(raw)
		h.srv.cache.pages.Put(fetchKey(u), pageEntry{page: f.page}, 10, h.srv.cache.now(), time.Hour, time.Hour)
	}
	for _, tt := range []struct {
		raw    string
		status int
		code   string
	}{
		{"http://192.168.1.20/", 403, "blocked_address"},
		{"http://nas.home.arpa/", 403, "blocked_address"},
		{"https://example.org:8443/", 400, "bad_request"},
		{"https://user:pw@example.org/", 400, "bad_request"},
		{"ftp://example.org/", 400, "bad_request"},
	} {
		var eb ErrorBody
		resp := h.do(t, http.MethodGet, "/v1/fetch?url="+url.QueryEscape(tt.raw), &eb)
		if resp.StatusCode != tt.status || eb.Error.Code != tt.code {
			t.Errorf("%s: %d %s, want %d %s", tt.raw, resp.StatusCode, eb.Error.Code, tt.status, tt.code)
		}
		if got := resp.Header.Get("X-Search-Cache"); got != "" {
			t.Errorf("%s: X-Search-Cache %q", tt.raw, got)
		}
	}
	if f.calls.Load() != 1 {
		t.Errorf("%d fetches, want 1", f.calls.Load())
	}
}

// A request that leaves does not fail another waiting for the same fetch.
func TestFetchSharedCallOutlivesALeaver(t *testing.T) {
	release := make(chan struct{})
	f := &slowFetcher{release: release}
	h := newHarness(&fakeProvider{}, f, true)
	key := fetchKey(&url.URL{Scheme: "https", Host: "example.org", Path: "/"})
	ctx, leave := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/fetch?url=https://example.org/", nil).WithContext(ctx))
		first <- rec.Code
	}()
	waitFor(t, func() bool { return h.srv.cache.fetchCalls.Waiting(key) == 1 })
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/fetch?url=https://example.org/", nil))
		second <- rec
	}()
	waitFor(t, func() bool { return h.srv.cache.fetchCalls.Waiting(key) == 2 })
	leave()
	if code := <-first; code != 504 {
		t.Errorf("the leaver got %d", code)
	}
	close(release)
	if rec := <-second; rec.Code != 200 {
		t.Errorf("the one that stayed got %d: %s", rec.Code, rec.Body)
	}
	if f.calls.Load() != 1 {
		t.Errorf("%d fetches", f.calls.Load())
	}
	waitFor(t, func() bool { return len(h.srv.slots) == 0 })
}

// slowFetcher answers once release closes, or fails when its context
// ends.
type slowFetcher struct {
	calls   atomic.Int32
	release chan struct{}
}

func (f *slowFetcher) Fetch(ctx context.Context, raw string, done func()) (*fetch.Page, error) {
	defer done()
	f.calls.Add(1)
	select {
	case <-f.release:
		return &fetch.Page{URL: raw, FinalURL: raw, Status: 200, ContentType: "text/plain", Text: "ok"}, nil
	case <-ctx.Done():
		return nil, &fetch.TimeoutError{Msg: "cancelled"}
	}
}

func TestCacheKeys(t *testing.T) {
	if searchKey("p", "  Zig\t std.HTTP \n", "") != searchKey("p", "zig std.http", "") || searchKey("p", "x", "") == searchKey("q", "x", "") {
		t.Error("search keys")
	}
	for _, tt := range [][2]string{
		{"HTTPS://Example.ORG/A?b=C#frag", "https://example.org/A?b=C"},
		{"https://example.org/#", "https://example.org/"},
		{"http://EXAMPLE.org:80/a%2Fb", "http://example.org:80/a%2Fb"},
	} {
		u, err := fetch.Parse(tt[0])
		if err != nil {
			t.Fatal(err)
		}
		if got := fetchKey(u); got != tt[1] {
			t.Errorf("fetchKey(%s) = %s, want %s", tt[0], got, tt[1])
		}
	}
}
