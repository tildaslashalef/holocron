// Package provider is the search provider interface (SPEC.md §5) and the
// chaining of several providers (§5.3). The HTTP layer applies §3.2's and
// §3.5's bounds to every provider's answers.
package provider

import (
	"context"
	"sync"
	"time"
)

// SearchRequest is one §3.2 request, its bounds applied.
type SearchRequest struct {
	Query     string
	N         int
	Freshness string // pd, pw, pm, py, YYYY-MM-DDtoYYYY-MM-DD, or ""
}

// Result is one search result as the provider gave it.
type Result struct {
	Title         string
	URL           string
	Snippet       string
	ExtraSnippets []string
	Engine        string
	Published     string // an ISO date, or ""
}

// Results is one answer: the items in the provider's rank order, the
// provider that answered ("" for the one asked), the query it searched
// when it corrected this one ("" when it did not), and the engines that
// failed for this query.
type Results struct {
	Items         []Result
	Provider      string
	AlteredQuery  string
	EnginesFailed []string
}

// Provider is a search backend.
type Provider interface {
	Name() string
	Search(ctx context.Context, r SearchRequest) (Results, error)
	Health(ctx context.Context) error
}

// ContextRequest is one §3.5 request, its bounds applied.
type ContextRequest struct {
	Query     string
	Tokens    int
	URLs      int
	Freshness string // as SearchRequest's
}

// Source is one page's passages.
type Source struct {
	URL       string
	Title     string
	Published string // an ISO date, or ""
	Snippets  []string
}

// Context is one §3.5 answer, as Results is for a search.
type Context struct {
	Sources       []Source
	Provider      string
	EnginesFailed []string
}

// Contexter is a provider with context (§3.5).
type Contexter interface {
	Context(ctx context.Context, r ContextRequest) (Context, error)
}

// Quota is a metered provider's remaining requests (§3.4).
type Quota struct {
	Remaining int64
	ResetsIn  time.Duration
}

// Quotaer is a metered provider; ok is false before its first answer,
// and when its plan has no monthly limit.
type Quotaer interface {
	Quota() (q Quota, ok bool)
}

// Biller is a metered provider that counts its billed requests (its
// successful answers) since the process started (§3.4).
type Biller interface {
	Billed() int64
}

// UnavailableError is a provider that cannot answer: unreachable, or every
// engine failed (no_engines). Reason is §10's category when the provider
// knows it: quota, key_refused, rate_limited; "" is error.
type UnavailableError struct {
	Msg    string
	Reason string
}

func (e *UnavailableError) Error() string { return e.Msg }

// TimeoutError is a provider that exceeded the budget (upstream_timeout).
type TimeoutError struct{ Msg string }

func (e *TimeoutError) Error() string { return e.Msg }

// BadQueryError is a query the provider refuses (bad_request).
type BadQueryError struct{ Msg string }

func (e *BadQueryError) Error() string { return e.Msg }

// NotAvailableError is an endpoint no configured provider has
// (not_available).
type NotAvailableError struct{ Msg string }

func (e *NotAvailableError) Error() string { return e.Msg }

// The reasons a chain moves past a provider (§10).
const (
	ReasonQuota       = "quota"
	ReasonKeyRefused  = "key_refused"
	ReasonRateLimited = "rate_limited"
	ReasonNoResults   = "no_results"
	ReasonError       = "error"
)

// Fallback is a provider a chain moved past, and why.
type Fallback struct {
	Provider string
	Reason   string
}

// Trace collects how one request was answered, for its log line (§10):
// the providers a chain moved past, and each metered provider's quota
// after its answer. It is safe for concurrent use; a nil *Trace ignores
// everything.
type Trace struct {
	mu        sync.Mutex
	fallbacks []Fallback
	quotas    map[string]Quota
	billed    map[string]int
	claimed   bool
}

// Bill records one billed request to provider.
func (t *Trace) Bill(provider string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.billed == nil {
		t.billed = map[string]int{}
	}
	t.billed[provider]++
}

// Claim reports true to its first caller only: of the requests sharing one
// call, the one that logs and counts what happens once per call, such as
// its bill (§10, §8.2).
func (t *Trace) Claim() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	first := !t.claimed
	t.claimed = true
	return first
}

// Billed returns the billed requests by provider.
func (t *Trace) Billed() map[string]int {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.billed))
	for k, v := range t.billed {
		out[k] = v
	}
	return out
}

// Fallback records that the chain moved past provider for reason.
func (t *Trace) Fallback(provider, reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fallbacks = append(t.fallbacks, Fallback{provider, reason})
}

// SetQuota records provider's quota from its answer.
func (t *Trace) SetQuota(provider string, q Quota) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.quotas == nil {
		t.quotas = map[string]Quota{}
	}
	t.quotas[provider] = q
}

// Fallbacks returns the fallbacks in order.
func (t *Trace) Fallbacks() []Fallback {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Fallback(nil), t.fallbacks...)
}

// Quotas returns the recorded quotas by provider.
func (t *Trace) Quotas() map[string]Quota {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]Quota, len(t.quotas))
	for k, v := range t.quotas {
		out[k] = v
	}
	return out
}

type traceKey struct{}

// WithTrace makes t the trace of requests under ctx.
func WithTrace(ctx context.Context, t *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceOf returns ctx's trace, or nil.
func TraceOf(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

type noCacheKey struct{}

// WithNoCache marks ctx as a request that asked for Cache-Control:
// no-cache, for a provider that has a cache of its own (§5.2).
func WithNoCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, noCacheKey{}, true)
}

// NoCache reports WithNoCache.
func NoCache(ctx context.Context) bool {
	v, _ := ctx.Value(noCacheKey{}).(bool)
	return v
}
