package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/provider"
)

// contextProvider is fakeProvider with context, recording its requests.
type contextProvider struct {
	*fakeProvider
	ans     provider.Context
	err     error
	calls   atomic.Int32
	last    provider.ContextRequest
	noCache bool
	quota   *provider.Quota
}

func (p *contextProvider) Name() string { return "brave" }

func (p *contextProvider) Context(ctx context.Context, r provider.ContextRequest) (provider.Context, error) {
	p.calls.Add(1)
	p.last, p.noCache = r, provider.NoCache(ctx)
	return p.ans, p.err
}

func (p *contextProvider) Quota() (provider.Quota, bool) {
	if p.quota == nil {
		return provider.Quota{}, false
	}
	return *p.quota, true
}

func newContextProvider() *contextProvider {
	return &contextProvider{fakeProvider: &fakeProvider{}, ans: provider.Context{Sources: []provider.Source{
		{URL: "https://ziglang.org/a", Title: " Zig \n HTTP ", Published: "2026-09-01", Snippets: []string{"one", "two\nlines"}},
		{URL: "https://ziglang.org/a", Title: "dup", Snippets: []string{"x"}},
		{URL: "ftp://example.org/", Title: "ftp", Snippets: []string{"x"}},
		{URL: "https://example.org/b", Title: strings.Repeat("t", 300), Snippets: []string{"three"}},
	}}}
}

func TestContext(t *testing.T) {
	p := newContextProvider()
	h := newHarness(p, nil, true)
	c := h.clock()
	var got Context
	resp := h.do(t, http.MethodGet, "/v1/context?q=+Zig+http+", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	wantCache(t, resp, "miss", "")
	if got.Query != "Zig http" || got.Provider != "brave" || len(got.Sources) != 2 || got.EnginesFailed == nil {
		t.Fatalf("context = %+v", got)
	}
	s0, s1 := got.Sources[0], got.Sources[1]
	if s0.Title != "Zig HTTP" || s0.Published == nil || *s0.Published != "2026-09-01" || !slices.Equal(s0.Snippets, []string{"one", "two\nlines"}) {
		t.Errorf("source 0 = %+v", s0)
	}
	if len([]rune(s1.Title)) > MaxTitle || s1.Published != nil {
		t.Errorf("source 1 = %+v", s1)
	}
	if p.last != (provider.ContextRequest{Query: "Zig http", Tokens: DefaultTokens, URLs: DefaultURLs}) {
		t.Errorf("request %+v", p.last)
	}

	c.add(time.Minute)
	wantCache(t, h.do(t, http.MethodGet, "/v1/context?q=zig+HTTP", &got), "hit", "60")
	if got.Query != "zig HTTP" || p.calls.Load() != 1 {
		t.Errorf("hit: %+v, %d calls", got, p.calls.Load())
	}
	// Other bounds are another entry; larger ones are clamped.
	wantCache(t, h.do(t, http.MethodGet, "/v1/context?q=zig+http&tokens=999999&urls=99", nil), "miss", "")
	if p.last.Tokens != MaxTokens || p.last.URLs != MaxURLs {
		t.Errorf("clamped: %+v", p.last)
	}
	// no-cache reaches the provider.
	h.send(t, refresh("/v1/context?q=zig+http"), nil)
	if !p.noCache || p.calls.Load() != 3 {
		t.Errorf("no-cache: %v, %d calls", p.noCache, p.calls.Load())
	}

	for _, target := range []string{
		"/v1/context", "/v1/context?q=++", "/v1/context?q=" + strings.Repeat("a", MaxQuery+1),
		"/v1/context?q=x&tokens=1023", "/v1/context?q=x&tokens=many", "/v1/context?q=x&urls=0",
	} {
		resp := h.do(t, http.MethodGet, target, nil)
		if resp.StatusCode != 400 || resp.Header.Get("X-Search-Cache") != "" {
			t.Errorf("%s: %d %v", target, resp.StatusCode, resp.Header)
		}
	}
}

// The snippets' total is cut at CharsPerToken characters per token.
func TestContextBudget(t *testing.T) {
	long := strings.Repeat("é", CharsPerToken*MinTokens-10)
	p := newContextProvider()
	p.ans = provider.Context{Sources: []provider.Source{
		{URL: "https://a.example/", Snippets: []string{long, "0123456789abcdef", "dropped"}},
		{URL: "https://b.example/", Snippets: []string{"dropped too"}},
	}}
	h := newHarness(p, nil, true)
	var got Context
	h.do(t, http.MethodGet, "/v1/context?q=x&tokens=1024", &got)
	if len(got.Sources) != 1 || len(got.Sources[0].Snippets) != 2 || got.Sources[0].Snippets[1] != "0123456789" {
		t.Errorf("budget: %d sources, %+v", len(got.Sources), got.Sources[0].Snippets[1:])
	}
}

func TestContextFailures(t *testing.T) {
	// SearXNG alone: not_available, after validation, before the cache.
	h := newHarness(&fakeProvider{}, nil, true)
	var eb ErrorBody
	resp := h.do(t, http.MethodGet, "/v1/context?q=x", &eb)
	if resp.StatusCode != 501 || eb.Error.Code != "not_available" ||
		eb.Error.Message != "fake has no context endpoint; use /v1/search and /v1/fetch" || resp.Header.Get("X-Search-Cache") != "" {
		t.Errorf("not_available: %d %+v %v", resp.StatusCode, eb, resp.Header)
	}
	h.wantError(t, "/v1/context?q=", 400, "bad_request")

	// No sources while engines failed is no_engines; an expired entry
	// answers it within 24 hours.
	p := newContextProvider()
	h = newHarness(p, nil, true)
	c := h.clock()
	h.do(t, http.MethodGet, "/v1/context?q=x", nil)
	c.add(SearchTTL)
	p.ans = provider.Context{EnginesFailed: []string{"brave"}}
	var got Context
	wantCache(t, h.do(t, http.MethodGet, "/v1/context?q=x", &got), "stale", "1800")
	if len(got.Sources) != 2 {
		t.Errorf("stale: %+v", got)
	}
	h.wantError(t, "/v1/context?q=other", 503, "no_engines")
	p.err = &provider.BadQueryError{Msg: "brave refused the query: too many words"}
	h.wantError(t, "/v1/context?q=third", 400, "bad_request")
	p.err = &provider.TimeoutError{Msg: "slow"}
	h.wantError(t, "/v1/context?q=x", 504, "upstream_timeout")
}

func TestHealthEndpointsAndQuota(t *testing.T) {
	var got Health
	newHarness(&fakeProvider{}, nil, true).do(t, http.MethodGet, "/v1/health", &got)
	if !slices.Equal(got.Endpoints, []string{"/v1/search", "/v1/fetch"}) || got.Quota != nil {
		t.Errorf("searxng alone: %+v", got)
	}

	p := newContextProvider()
	searx := &fakeProvider{health: errDown}
	h := newHarness(provider.NewChain(p, searx), nil, true)
	h.do(t, http.MethodGet, "/v1/health", &got)
	if !slices.Equal(got.Endpoints, []string{"/v1/search", "/v1/context", "/v1/fetch"}) ||
		got.Providers["brave"] != "ok" || got.Providers["fake"] != "down" || got.Quota != nil {
		t.Errorf("chain: %+v", got)
	}
	p.quota = &provider.Quota{Remaining: 948, ResetsIn: 90 * time.Second}
	h.do(t, http.MethodGet, "/v1/health", &got)
	if q := got.Quota["brave"]; q.Remaining != 948 || q.ResetsInS != 90 {
		t.Errorf("quota: %+v", got.Quota)
	}
}

// A chain's answer names the provider that answered; the cache key is the
// chain's.
func TestSearchChainProvider(t *testing.T) {
	brave := &fakeProvider{err: &provider.UnavailableError{Msg: "brave: down"}}
	searx := &fakeProvider{res: results(2)}
	h := newHarness(provider.NewChain(namedProvider{brave, "brave"}, namedProvider{searx, "searxng"}), nil, true)
	var s Search
	h.do(t, http.MethodGet, "/v1/search?q=x", &s)
	if s.Provider != "searxng" || !slices.Equal(s.EnginesFailed, []string{"brave"}) || s.Results[0].Engine != "e" {
		t.Errorf("search = %+v", s)
	}
	if !strings.Contains(h.logged(), "provider=brave,searxng") {
		t.Errorf("log:\n%s", h.logged())
	}
}

type namedProvider struct {
	*fakeProvider
	name string
}

func (n namedProvider) Name() string { return n.name }

var errDown = &provider.UnavailableError{Msg: "down"}

// traceProvider answers a search as a named member, recording a quota in
// the request's trace as Brave does.
type traceProvider struct {
	name   string
	res    provider.Results
	err    error
	quota  *provider.Quota
	block  chan struct{}
	billed atomic.Int64
}

func (p *traceProvider) Billed() int64 { return p.billed.Load() }

// answer records what Brave would: the quota and, on success, a bill.
func (p *traceProvider) answer(ctx context.Context) {
	if p.block != nil {
		<-p.block
	}
	if p.quota != nil {
		provider.TraceOf(ctx).SetQuota(p.name, *p.quota)
	}
	if p.err == nil {
		p.billed.Add(1)
		provider.TraceOf(ctx).Bill(p.name)
	}
}

func (p *traceProvider) Name() string                     { return p.name }
func (p *traceProvider) Health(ctx context.Context) error { return nil }
func (p *traceProvider) Search(ctx context.Context, r provider.SearchRequest) (provider.Results, error) {
	p.answer(ctx)
	return p.res, p.err
}
func (p *traceProvider) Context(ctx context.Context, r provider.ContextRequest) (provider.Context, error) {
	p.answer(ctx)
	return provider.Context{Sources: []provider.Source{{URL: "https://a.example/", Snippets: []string{"s"}}}}, p.err
}

// §10: which provider answered, the chain's fallbacks, the results, and
// Brave's quota from this request's answer.
func TestLogAnsweredBy(t *testing.T) {
	brave := &traceProvider{name: "brave", res: results(3), quota: &provider.Quota{Remaining: 948, ResetsIn: 90 * time.Second}}
	searx := &traceProvider{name: "searxng", res: provider.Results{Items: results(2).Items, EnginesFailed: []string{"duckduckgo", "mojeek"}}}
	h := newHarness(provider.NewChain(brave, searx), nil, false)
	h.clock()
	line := func() string {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(h.logged()), "\n")
		return lines[len(lines)-1]
	}
	want := func(l string, has []string, lacks []string) {
		t.Helper()
		for _, w := range has {
			if !strings.Contains(l, w) {
				t.Errorf("line lacks %q:\n%s", w, l)
			}
		}
		for _, w := range lacks {
			if strings.Contains(l, w) {
				t.Errorf("line has %q:\n%s", w, l)
			}
		}
	}

	h.do(t, http.MethodGet, "/v1/search?q=private+words&n=2", nil)
	want(line(), []string{"provider=brave,searxng answered_by=brave results=2 brave_billed=1 brave_remaining=948 brave_resets_in_s=90", "cache=miss"},
		[]string{"fallback=", "engines_failed=", "private"})

	// A hit: how it was answered, and no call.
	h.do(t, http.MethodGet, "/v1/search?q=private+words", nil)
	want(line(), []string{"answered_by=brave results=3", "cache=hit"}, []string{"brave_remaining", "brave_billed", "fallback="})

	// Brave's quota used up: SearXNG answers.
	brave.err, brave.quota = &provider.UnavailableError{Msg: "brave: quota or credit used up", Reason: provider.ReasonQuota}, nil
	h.do(t, http.MethodGet, "/v1/search?q=other", nil)
	want(line(), []string{"answered_by=searxng fallback=brave:quota results=2 engines_failed=brave,duckduckgo,mojeek"}, []string{"brave_remaining", "brave_billed"})

	// Both fail: the error, and the fallback that led to it.
	brave.err = &provider.UnavailableError{Msg: "brave refused the key", Reason: provider.ReasonKeyRefused}
	searx.err = &provider.UnavailableError{Msg: "searxng: down"}
	h.do(t, http.MethodGet, "/v1/search?q=third", nil)
	want(line(), []string{"code=no_engines", "fallback=brave:key_refused"}, []string{"answered_by", "results="})

	// Context.
	brave.err, brave.quota = nil, &provider.Quota{Remaining: 947, ResetsIn: time.Minute}
	h.do(t, http.MethodGet, "/v1/context?q=x", nil)
	want(line(), []string{"endpoint=/v1/context", "answered_by=brave results=1 brave_billed=1 brave_remaining=947 brave_resets_in_s=60"}, nil)

	var hl Health
	h.do(t, http.MethodGet, "/v1/health", &hl)
	if hl.Billed["brave"] != 2 || hl.Billed["searxng"] != 1 {
		t.Errorf("health billed = %+v", hl.Billed)
	}
}

// Requests sharing one call log its bill once, so the log's sum is the
// bill.
func TestLogBilledOncePerSharedCall(t *testing.T) {
	brave := &traceProvider{name: "brave", res: results(3), block: make(chan struct{})}
	h := newHarness(provider.NewChain(brave, &traceProvider{name: "searxng"}), nil, true)
	key := searchKey("brave,searxng", "shared", "")
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			h.srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/search?q=shared", nil))
		})
	}
	waitFor(t, func() bool { return h.srv.cache.searchCalls.Waiting(key) == 3 })
	close(brave.block)
	wg.Wait()
	log := h.logged()
	if n := strings.Count(log, "brave_billed=1"); n != 1 || strings.Count(log, "answered_by=brave") != 3 {
		t.Errorf("brave_billed on %d lines:\n%s", n, log)
	}
}

// §3.2 and §3.5: freshness is validated, passed on and part of the key;
// results carry extra_snippets within bounds; altered_query says what was
// searched.
func TestFreshnessExtraSnippetsAltered(t *testing.T) {
	long := strings.Repeat("word ", 150)
	p := &freshProvider{res: provider.Results{AlteredQuery: "zig std.http", Items: []provider.Result{
		{URL: "https://a.example/", Title: "A", ExtraSnippets: []string{" one \n two ", "", long, "3", "4", "5", "6"}},
		{URL: "https://b.example/", Title: "B"},
	}}}
	h := newHarness(p, nil, true)
	var s Search
	h.do(t, http.MethodGet, "/v1/search?q=zig+std.htpp&freshness=pw", &s)
	if p.last.Freshness != "pw" || s.AlteredQuery == nil || *s.AlteredQuery != "zig std.http" {
		t.Errorf("search: %+v, request %+v", s, p.last)
	}
	e := s.Results[0].ExtraSnippets
	if len(e) != MaxExtraSnippets || e[0] != "one two" || len([]rune(e[1])) > MaxSnippet || !strings.HasSuffix(e[1], "…") || e[4] != "5" {
		t.Errorf("extra snippets = %q", e)
	}
	var raw map[string]any
	h.do(t, http.MethodGet, "/v1/search?q=zig+std.htpp&freshness=pw", &raw)
	if r1 := raw["results"].([]any)[1].(map[string]any); r1["extra_snippets"] == nil || len(r1["extra_snippets"].([]any)) != 0 {
		t.Errorf("no extra snippets is not an empty list: %v", r1)
	}
	if p.calls != 1 {
		t.Errorf("%d calls; the second should be a hit", p.calls)
	}
	// Another freshness is another entry; none is "".
	h.do(t, http.MethodGet, "/v1/search?q=zig+std.htpp&freshness=2026-01-01to2026-10-01", nil)
	h.do(t, http.MethodGet, "/v1/search?q=zig+std.htpp", &raw)
	if p.calls != 3 || p.last.Freshness != "" || raw["altered_query"] != "zig std.http" {
		t.Errorf("%d calls, last %+v", p.calls, p.last)
	}
	p.res.AlteredQuery = ""
	h.do(t, http.MethodGet, "/v1/search?q=other", &raw)
	if v, ok := raw["altered_query"]; !ok || v != nil {
		t.Errorf("altered_query without a correction: %v", raw)
	}
	for _, f := range []string{"week", "PD", "2026-13-01to2026-12-01", "2026-10-02to2026-10-01", "2026-10-01"} {
		h.wantError(t, "/v1/search?q=x&freshness="+f, 400, "bad_request")
		h.wantError(t, "/v1/context?q=x&freshness="+f, 400, "bad_request")
	}

	cp := newContextProvider()
	h = newHarness(cp, nil, true)
	h.do(t, http.MethodGet, "/v1/context?q=x&freshness=py", nil)
	h.do(t, http.MethodGet, "/v1/context?q=x", nil)
	if cp.calls.Load() != 2 || cp.last.Freshness != "" {
		t.Errorf("context: %d calls, last %+v", cp.calls.Load(), cp.last)
	}
}

type freshProvider struct {
	res   provider.Results
	last  provider.SearchRequest
	calls int
}

func (p *freshProvider) Name() string                     { return "brave" }
func (p *freshProvider) Health(ctx context.Context) error { return nil }
func (p *freshProvider) Search(ctx context.Context, r provider.SearchRequest) (provider.Results, error) {
	p.last = r
	p.calls++
	return p.res, nil
}
