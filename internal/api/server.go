package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tildaslashalef/holocron/internal/cache"
	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/provider"
)

// Serving limits (§8).
const (
	Slots         = 8
	SlotWait      = 2 * time.Second
	SearchBudget  = 10 * time.Second
	ContextBudget = 20 * time.Second
	HealthTimeout = 3 * time.Second
)

// Fetcher is what /v1/fetch needs of fetch.Client.
type Fetcher interface {
	Fetch(ctx context.Context, raw string, done func()) (*fetch.Page, error)
}

// Server is the v1 handler. Search and fetch share Slots request slots;
// health takes none.
type Server struct {
	version    string
	provider   provider.Provider
	fetcher    Fetcher
	log        *slog.Logger
	logQueries bool
	slots      chan struct{}
	slotWait   time.Duration
	cache      *caches
	metrics    *serverMetrics
	// searxngImage is the pin §8.2's build info reports.
	searxngImage string
}

// NewServer returns the handler. With logQueries false the log keeps the
// host but not the query or the URLs (§10).
func NewServer(version string, p provider.Provider, f Fetcher, log *slog.Logger, logQueries bool) *Server {
	return &Server{
		version:    version,
		provider:   p,
		fetcher:    f,
		log:        log,
		logQueries: logQueries,
		slots:      make(chan struct{}, Slots),
		slotWait:   SlotWait,
		cache:      newCaches(),
		metrics:    newServerMetrics(),
	}
}

// SetSearXNGImage names the SearXNG pin in §8.2's build info.
func (s *Server) SetSearXNGImage(image string) { s.searxngImage = image }

// entry is one request's log line, filled in by its handler.
type entry struct {
	status   int
	code     string
	provider string
	host     string
	query    string
	url      string
	finalURL string
	cache    string
	message  string // the error answer's message
	detail   string // for internal errors

	// How a search or context was answered (§10).
	answeredBy    string
	results       int
	enginesFailed []string
	trace         *provider.Trace
	// For the request that claimed its call's trace: what happened once
	// per call.
	firstOfCall bool
	billed      map[string]int
}

// answered records a search or context answer for the log line.
func (e *entry) answered(by string, results int, enginesFailed []string) {
	e.answeredBy, e.results, e.enginesFailed = by, results, enginesFailed
}

// answer is what a handler returns: a body to encode, or an error; and
// for a search or fetch past validation, how the cache answered (§8.1).
type answer struct {
	body   any
	err    error
	cache  string    // hit, miss or stale; "" before the lookup
	stored time.Time // a hit or stale entry's
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// §8.2: no slot, no cache, not logged or counted.
	if r.URL.Path == "/v1/metrics" && r.Method == http.MethodGet {
		s.serveMetrics(w, r)
		return
	}
	start := time.Now()
	e := &entry{}
	a := s.dispatch(r, e)
	status, body := http.StatusOK, a.body
	if a.err != nil {
		ae := Classify(a.err)
		status, e.code, e.message = ae.Status, ae.Code, ae.Message
		var eb ErrorBody
		eb.Error.Code, eb.Error.Message = ae.Code, ae.Message
		body = eb
		if ae.Code == "internal" && e.detail == "" {
			e.detail = a.err.Error()
			if p, ok := errors.AsType[*fetch.PanicError](a.err); ok {
				e.detail += "\n" + string(p.Stack)
			}
			if p, ok := errors.AsType[*cache.PanicError](a.err); ok {
				e.detail += "\n" + string(p.Stack)
			}
		}
	}
	if a.cache != "" {
		e.cache = a.cache
		w.Header().Set("X-Search-Cache", a.cache)
		if a.cache != cacheMiss {
			age := max(s.cache.now().Sub(a.stored), 0)
			w.Header().Set("Age", strconv.FormatInt(int64(age/time.Second), 10))
		}
	}
	e.status = status
	if e.firstOfCall = e.trace.Claim(); e.firstOfCall {
		e.billed = e.trace.Billed()
	}
	write(w, status, body)
	d := time.Since(start)
	s.logRequest(r, e, d)
	s.metrics.record(r, e, d)
}

// dispatch routes and recovers: a panic is internal, and the process keeps
// serving.
func (s *Server) dispatch(r *http.Request, e *entry) (a answer) {
	defer func() {
		if v := recover(); v != nil {
			e.detail = fmt.Sprintf("panic: %v\n%s", v, debug.Stack())
			a = answer{err: fmt.Errorf("panic: %v", v)}
		}
	}()
	if r.Method != http.MethodGet {
		return answer{err: &Error{"method_not_allowed", http.StatusMethodNotAllowed, r.Method + " is not allowed; only GET is"}}
	}
	switch r.URL.Path {
	case "/v1/search":
		return s.search(r, e)
	case "/v1/context":
		return s.context(r, e)
	case "/v1/fetch":
		return s.fetch(r, e)
	case "/v1/health":
		return s.health(r)
	}
	return answer{err: &Error{"not_found", http.StatusNotFound, r.URL.Path + " is not part of the contract"}}
}

func (s *Server) search(r *http.Request, e *entry) answer {
	name := s.provider.Name()
	e.provider = name
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	e.query = query
	if query == "" {
		return answer{err: badRequest("q is empty")}
	}
	if len(query) > MaxQuery {
		return answer{err: badRequest(fmt.Sprintf("q is longer than %d bytes", MaxQuery))}
	}
	n, err := intParam(q, "n", DefaultN, 1, MaxN)
	if err != nil {
		return answer{err: err}
	}
	freshness, err := freshnessParam(q)
	if err != nil {
		return answer{err: err}
	}

	c := s.cache
	key := searchKey(name, query, freshness)
	out, how, stored, trace, err := cachedCall(s, r, c.searches, &c.searchCalls, key, SearchBudget,
		func(ctx context.Context) (*Search, error) {
			// Every n is a slice of the 10 results kept.
			res, err := s.provider.Search(ctx, provider.SearchRequest{Query: query, N: MaxN, Freshness: freshness})
			if err != nil {
				return nil, err
			}
			return NewSearch(query, name, res, MaxN)
		},
		func(v *Search) (int64, bool) { return v.size(), len(v.EnginesFailed) > 0 })
	e.trace = trace
	if err != nil {
		return answer{err: err, cache: how}
	}
	body := out.slice(query, n)
	e.answered(body.Provider, len(body.Results), body.EnginesFailed)
	return answer{body: body, cache: how, stored: stored}
}

func (s *Server) context(r *http.Request, e *entry) answer {
	name := s.provider.Name()
	e.provider = name
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	e.query = query
	if query == "" {
		return answer{err: badRequest("q is empty")}
	}
	if len(query) > MaxQuery {
		return answer{err: badRequest(fmt.Sprintf("q is longer than %d bytes", MaxQuery))}
	}
	tokens, err := intParam(q, "tokens", DefaultTokens, MinTokens, MaxTokens)
	if err != nil {
		return answer{err: err}
	}
	urls, err := intParam(q, "urls", DefaultURLs, 1, MaxURLs)
	if err != nil {
		return answer{err: err}
	}
	freshness, err := freshnessParam(q)
	if err != nil {
		return answer{err: err}
	}
	if !provider.HasContext(s.provider) {
		_, err := provider.ContextOf(r.Context(), s.provider, provider.ContextRequest{})
		return answer{err: err}
	}
	c := s.cache
	key := fmt.Sprintf("%s\x00%d\x00%d", searchKey(name, query, freshness), tokens, urls)
	out, how, stored, trace, err := cachedCall(s, r, c.contexts, &c.contextCalls, key, ContextBudget,
		func(ctx context.Context) (*Context, error) {
			res, err := provider.ContextOf(ctx, s.provider, provider.ContextRequest{Query: query, Tokens: tokens, URLs: urls, Freshness: freshness})
			if err != nil {
				return nil, err
			}
			return NewContext(query, name, res, tokens)
		},
		func(v *Context) (int64, bool) { return v.size(), len(v.EnginesFailed) > 0 })
	e.trace = trace
	if err != nil {
		return answer{err: err, cache: how}
	}
	body := out.slice(query)
	e.answered(body.Provider, len(body.Sources), body.EnginesFailed)
	return answer{body: body, cache: how, stored: stored}
}

// cachedCall is §8.1 for search and context: the lookup unless the request
// sent no-cache, else one shared call per key under budget and a slot, its
// answer stored (size and whether engines failed from measure), and an
// expired entry answered instead of no_engines. trace is how the call got
// its answer (§10); nil on a hit, or when this request left before it.
func cachedCall[T any](s *Server, r *http.Request, lru *cache.LRU[T], group *cache.Group[traced[T]], key string, budget time.Duration,
	call func(context.Context) (T, error), measure func(T) (size int64, partial bool)) (v T, how string, stored time.Time, trace *provider.Trace, err error) {
	c := s.cache
	refresh := noCache(r)
	var stale *cache.Entry[T]
	if !refresh {
		if ent, ok := lru.Get(key, c.now()); ok {
			if ent.Fresh {
				return ent.Value, cacheHit, ent.Stored, nil, nil
			}
			stale = &ent
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	t, err := group.Do(ctx, key, func(ctx context.Context) (traced[T], error) {
		t := traced[T]{trace: &provider.Trace{}}
		release, err := s.acquire(ctx)
		if err != nil {
			return t, err
		}
		defer release()
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		ctx = provider.WithTrace(ctx, t.trace)
		if refresh {
			ctx = provider.WithNoCache(ctx)
		}
		v, err := call(ctx)
		if err != nil {
			return t, err
		}
		t.v = v
		size, partial := measure(v)
		ttl := SearchTTL
		if partial {
			ttl = SearchPartialTTL
		}
		lru.Put(key, v, int64(len(key))+size, c.now(), ttl, StaleMax)
		return t, nil
	})
	if err != nil {
		err = waited(ctx, err, budget)
		if stale != nil && Classify(err).Code == "no_engines" {
			return stale.Value, cacheStale, stale.Stored, t.trace, nil
		}
		return t.v, cacheMiss, time.Time{}, t.trace, err
	}
	return t.v, cacheMiss, time.Time{}, t.trace, nil
}

func (s *Server) fetch(r *http.Request, e *entry) answer {
	q := r.URL.Query()
	raw := q.Get("url")
	e.url = raw
	if u, err := url.Parse(raw); err == nil {
		e.host = u.Hostname()
	}
	if raw == "" {
		return answer{err: badRequest("url is missing")}
	}
	start, err := intParam(q, "start", 0, 0, -1)
	if err != nil {
		return answer{err: err}
	}
	maxChars, err := intParam(q, "max_chars", DefaultMaxChars, 1, MaxMaxChars)
	if err != nil {
		return answer{err: err}
	}
	// A URL the guard refuses outright needs no slot, and never reaches
	// the cache (§8.1).
	u, err := fetch.Parse(raw)
	if err != nil {
		return answer{err: err}
	}

	c := s.cache
	key := fetchKey(u)
	// page is one request's answer from a shared document.
	page := func(p *fetch.Page, how string, stored time.Time) answer {
		e.finalURL = p.FinalURL
		own := *p
		own.URL = raw
		return answer{body: NewFetch(&own, start, maxChars), cache: how, stored: stored}
	}
	if !noCache(r) {
		if ent, ok := c.pages.Get(key, c.now()); ok && ent.Fresh {
			if ent.Value.err != nil {
				return answer{err: ent.Value.err, cache: cacheHit, stored: ent.Stored}
			}
			return page(ent.Value.page, cacheHit, ent.Stored)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), fetch.Budget)
	defer cancel()
	p, err := c.fetchCalls.Do(ctx, key, func(ctx context.Context) (*fetch.Page, error) {
		release, err := s.acquire(ctx)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, fetch.Budget)
		defer cancel()
		// The slot is released by Fetch when all its work ends, which may
		// be after a timed-out request has been answered (§7.1).
		p, err := s.fetcher.Fetch(ctx, raw, release)
		if err != nil {
			if code := Classify(err).Code; code == "upstream_failed" || code == "unsupported_content" {
				c.pages.Put(key, pageEntry{err: err}, int64(entryOverhead+len(key)+len(err.Error())), c.now(), FailTTL, FailTTL)
			}
			return nil, err
		}
		c.pages.Put(key, pageEntry{page: p}, int64(len(key))+pageSize(p), c.now(), FetchTTL, FetchTTL)
		return p, nil
	})
	if err != nil {
		return answer{err: waited(ctx, err, fetch.Budget), cache: cacheMiss}
	}
	return page(p, cacheMiss, time.Time{})
}

func (s *Server) health(r *http.Request) answer {
	ctx, cancel := context.WithTimeout(r.Context(), HealthTimeout)
	defer cancel()
	h := Health{Version: s.version, Providers: map[string]string{}, Endpoints: []string{"/v1/search"}, Cache: s.cache.stats()}
	for _, p := range provider.Members(s.provider) {
		state := "ok"
		if err := p.Health(ctx); err != nil {
			state = "down"
		}
		h.Providers[p.Name()] = state
		if b, ok := p.(provider.Biller); ok {
			if h.Billed == nil {
				h.Billed = map[string]int64{}
			}
			h.Billed[p.Name()] = b.Billed()
		}
		if q, ok := p.(provider.Quotaer); ok {
			if quota, ok := q.Quota(); ok {
				if h.Quota == nil {
					h.Quota = map[string]QuotaStats{}
				}
				h.Quota[p.Name()] = QuotaStats{Remaining: quota.Remaining, ResetsInS: int64(quota.ResetsIn / time.Second)}
			}
		}
	}
	if provider.HasContext(s.provider) {
		h.Endpoints = append(h.Endpoints, "/v1/context")
	}
	h.Endpoints = append(h.Endpoints, "/v1/fetch")
	return answer{body: h}
}

// acquire takes a request slot, waiting at most slotWait; release gives it
// back exactly once.
func (s *Server) acquire(ctx context.Context) (release func(), err error) {
	var once sync.Once
	release = func() { once.Do(func() { <-s.slots }) }
	select {
	case s.slots <- struct{}{}:
		return release, nil
	default:
	}
	t := time.NewTimer(s.slotWait)
	defer t.Stop()
	select {
	case s.slots <- struct{}{}:
	case <-t.C:
		return nil, &Error{"busy", http.StatusServiceUnavailable,
			fmt.Sprintf("all %d request slots stayed taken for %s", cap(s.slots), s.slotWait)}
	case <-ctx.Done():
		return nil, &Error{"busy", http.StatusServiceUnavailable, "the request ended while waiting for a slot"}
	}
	return release, nil
}

// freshnessParam reads §3.2's freshness: "" when absent, bad_request when
// not one of its forms.
func freshnessParam(q url.Values) (string, error) {
	f := q.Get("freshness")
	if f != "" && !ValidFreshness(f) {
		return "", badRequest(fmt.Sprintf("freshness %q is not pd, pw, pm, py or YYYY-MM-DDtoYYYY-MM-DD", f))
	}
	return f, nil
}

// ValidFreshness reports one of §3.2's freshness forms: pd, pw, pm, py, or
// a date range YYYY-MM-DDtoYYYY-MM-DD whose start is not after its end.
func ValidFreshness(f string) bool {
	switch f {
	case "pd", "pw", "pm", "py":
		return true
	}
	from, to, ok := strings.Cut(f, "to")
	if !ok {
		return false
	}
	a, err1 := time.Parse(time.DateOnly, from)
	b, err2 := time.Parse(time.DateOnly, to)
	return err1 == nil && err2 == nil && !a.After(b)
}

// intParam reads an integer parameter: def when absent, bad_request when
// not an integer or below lo, clamped to hi (hi < 0: no bound).
func intParam(q url.Values, name string, def, lo, hi int) (int, error) {
	raw := q.Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, badRequest(fmt.Sprintf("%s is not an integer: %q", name, raw))
	}
	if v < lo {
		return 0, badRequest(fmt.Sprintf("%s is below %d", name, lo))
	}
	if hi >= 0 && v > hi {
		v = hi
	}
	return v, nil
}

func write(w http.ResponseWriter, status int, body any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"error":{"code":"internal","message":"encoding the answer failed"}}` + "\n")
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	if status == http.StatusMethodNotAllowed {
		h.Set("Allow", http.MethodGet)
	}
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// levels is §10's level by whose fault a failure is: WARN for the
// upstream's or holocron's capacity, and for a refused address (someone
// tried); ERROR for a bug; INFO otherwise, success and the client's own
// mistakes.
var levels = map[string]slog.Level{
	"upstream_failed":  slog.LevelWarn,
	"upstream_timeout": slog.LevelWarn,
	"no_engines":       slog.LevelWarn,
	"busy":             slog.LevelWarn,
	"blocked_address":  slog.LevelWarn,
	"internal":         slog.LevelError,
}

// logRequest writes §10's one line per request.
func (s *Server) logRequest(r *http.Request, e *entry, d time.Duration) {
	attrs := []slog.Attr{
		slog.String("endpoint", r.URL.Path),
		slog.Int("status", e.status),
	}
	if e.code != "" {
		attrs = append(attrs, slog.String("code", e.code))
	}
	attrs = append(attrs, slog.Int64("ms", d.Milliseconds()))
	if e.provider != "" {
		attrs = append(attrs, slog.String("provider", e.provider))
	}
	if e.answeredBy != "" {
		attrs = append(attrs, slog.String("answered_by", e.answeredBy))
	}
	if fb := e.trace.Fallbacks(); len(fb) > 0 {
		parts := make([]string, len(fb))
		for i, f := range fb {
			parts[i] = f.Provider + ":" + f.Reason
		}
		attrs = append(attrs, slog.String("fallback", strings.Join(parts, ",")))
	}
	if e.answeredBy != "" {
		attrs = append(attrs, slog.Int("results", e.results))
		if len(e.enginesFailed) > 0 {
			attrs = append(attrs, slog.String("engines_failed", strings.Join(e.enginesFailed, ",")))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(e.billed)) {
		attrs = append(attrs, slog.Int(name+"_billed", e.billed[name]))
	}
	quotas := e.trace.Quotas()
	for _, name := range slices.Sorted(maps.Keys(quotas)) {
		q := quotas[name]
		attrs = append(attrs, slog.Int64(name+"_remaining", q.Remaining), slog.Int64(name+"_resets_in_s", int64(q.ResetsIn/time.Second)))
	}
	if e.host != "" {
		attrs = append(attrs, slog.String("host", e.host))
	}
	if e.cache != "" {
		attrs = append(attrs, slog.String("cache", e.cache))
	}
	if s.logQueries {
		if e.query != "" {
			attrs = append(attrs, slog.String("q", e.query))
		}
		if e.url != "" {
			attrs = append(attrs, slog.String("url", e.url))
		}
		if e.finalURL != "" && e.finalURL != e.url {
			attrs = append(attrs, slog.String("final_url", e.finalURL))
		}
	}
	// The message can quote the request (a URL that does not parse), so it
	// follows --log-queries.
	if e.message != "" && s.logQueries {
		attrs = append(attrs, slog.String("error", e.message))
	}
	attrs = append(attrs, slog.String("ua", r.UserAgent()))
	level := levels[e.code]
	if e.detail != "" {
		level = slog.LevelError
		attrs = append(attrs, slog.String("detail", e.detail))
	}
	s.log.LogAttrs(context.Background(), level, "request", attrs...)
}
