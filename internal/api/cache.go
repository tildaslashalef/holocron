package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tildaslashalef/holocron/internal/cache"
	"github.com/tildaslashalef/holocron/internal/provider"
)

// The cache (§8.1).
const (
	SearchTTL         = 30 * time.Minute
	SearchPartialTTL  = 5 * time.Minute // when a provider failed before the one that answered
	StaleMax          = 24 * time.Hour  // a search kept for provider_unavailable
	FetchTTL          = 15 * time.Minute
	FocusTTL          = 30 * time.Minute // a fetch with q, like a search
	FailTTL           = time.Minute      // a fetch's upstream_failed, unsupported_content
	SearchCacheBytes  = 8 << 20
	ContextCacheBytes = 8 << 20
	FetchCacheBytes   = 48 << 20

	// entryOverhead is counted for every entry beside its strings: the
	// list element, the map slot, the struct.
	entryOverhead = 256
)

// The X-Search-Cache values.
const (
	cacheHit   = "hit"
	cacheMiss  = "miss"
	cacheStale = "stale"
)

// CacheStats is §3.4's count of the cache.
type CacheStats struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
}

// caches are the server's LRUs and their shared calls.
type caches struct {
	searches     *cache.LRU[*Search]
	contexts     *cache.LRU[*Context]
	pages        *cache.LRU[pageEntry]
	searchCalls  cache.Group[traced[*Search]]
	contextCalls cache.Group[traced[*Context]]
	fetchCalls   cache.Group[traced[pageEntry]]
	now          func() time.Time
}

// traced is a shared call's answer with the trace of how it was got
// (§10), which every request sharing it logs.
type traced[T any] struct {
	v     T
	trace *provider.Trace
}

// pageEntry is an extracted document, who answered it and what it cost,
// or a failure kept for FailTTL.
type pageEntry struct {
	page *provider.Page
	by   provider.Answered
	cost int64
	err  error
}

func newCaches() *caches {
	return &caches{
		searches: cache.NewLRU[*Search](SearchCacheBytes),
		contexts: cache.NewLRU[*Context](ContextCacheBytes),
		pages:    cache.NewLRU[pageEntry](FetchCacheBytes),
		now:      time.Now,
	}
}

// kinds are the caches by §8.2's kind label, for the metrics and stats.
func (c *caches) kinds() []struct {
	kind string
	len  func() (int, int64)
} {
	return []struct {
		kind string
		len  func() (int, int64)
	}{{"search", c.searches.Len}, {"context", c.contexts.Len}, {"fetch", c.pages.Len}}
}

func (c *caches) stats() *CacheStats {
	sn, sb := c.searches.Len()
	cn, cb := c.contexts.Len()
	pn, pb := c.pages.Len()
	return &CacheStats{Entries: sn + cn + pn, Bytes: sb + cb + pb}
}

// searchKey is the provider, the query trimmed, its whitespace collapsed,
// lowercased, and the freshness.
func searchKey(provider, query, freshness string) string {
	return provider + "\x00" + strings.ToLower(collapse(query)) + "\x00" + freshness
}

// fetchKey is the URL with its scheme and host lowercased and its fragment
// dropped; the query is kept.
func fetchKey(u *url.URL) string {
	k := *u
	k.Scheme = strings.ToLower(k.Scheme)
	k.Host = strings.ToLower(k.Host)
	k.Fragment, k.RawFragment = "", ""
	return k.String()
}

// pageKey is a fetch's entry: the fetch chain, the URL's key, and the
// question collapsed and lowercased ("" for the whole page).
func pageKey(chain string, u *url.URL, focus string) string {
	return chain + "\x00" + fetchKey(u) + "\x00" + strings.ToLower(collapse(focus))
}

// noCache reports a request's Cache-Control: no-cache.
func noCache(r *http.Request) bool {
	for _, v := range r.Header.Values("Cache-Control") {
		for d := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(d), "no-cache") {
				return true
			}
		}
	}
	return false
}

// slice is s for one request: its own query and at most n results. The
// cached answer is shared, so it is copied, not changed.
func (s *Search) slice(query string, n int) *Search {
	out := *s
	out.Query = query
	out.Results = s.Results[:min(n, len(s.Results))]
	return &out
}

func (s *Search) size() int64 {
	n := entryOverhead + len(s.Provider)
	for _, r := range s.Results {
		n += 64 + len(r.Title) + len(r.URL) + len(r.Snippet)
		if r.Published != nil {
			n += len(*r.Published)
		}
		for _, e := range r.Snippets {
			n += 16 + len(e)
		}
	}
	for _, e := range s.ProvidersFailed {
		n += 16 + len(e)
	}
	return int64(n)
}

// slice is c for one request: its own query.
func (c *Context) slice(query string) *Context {
	out := *c
	out.Query = query
	return &out
}

func (c *Context) size() int64 {
	n := entryOverhead + len(c.Provider)
	for _, s := range c.Sources {
		n += 64 + len(s.URL) + len(s.Title)
		if s.Published != nil {
			n += len(*s.Published)
		}
		for _, sn := range s.Snippets {
			n += 16 + len(sn)
		}
	}
	for _, e := range c.ProvidersFailed {
		n += 16 + len(e)
	}
	return int64(n)
}

func pageSize(e pageEntry) int64 {
	n := entryOverhead + len(e.by.Provider)
	for _, f := range e.by.Failed {
		n += 16 + len(f)
	}
	if p := e.page; p != nil {
		n += len(p.URL) + len(p.FinalURL) + len(p.ContentType) + len(p.Title) + len(p.Published) + len(p.Text)
	}
	if e.err != nil {
		n += len(e.err.Error())
	}
	return int64(n)
}

// waited maps a shared call's result for one request: the request ending
// before the call did is upstream_timeout, as an upstream that outlived
// the budget is.
func waited(ctx context.Context, err error, budget time.Duration) error {
	if err == nil || ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return &Error{"upstream_timeout", http.StatusGatewayTimeout, "the request was cancelled"}
	}
	return &Error{"upstream_timeout", http.StatusGatewayTimeout, "no answer within the " + budget.String() + " budget"}
}
