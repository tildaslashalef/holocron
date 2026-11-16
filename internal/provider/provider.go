// Package provider is the provider interfaces (SPEC.md §5), the chaining
// of several providers per endpoint (§5.4), and what a request records of
// how it was answered (§10). The HTTP layer applies §3's bounds to every
// provider's answers.
package provider

import (
	"context"
	"sync"
	"time"
)

// Provider is a backend: a search API, an extraction API, or holocron's
// own fetcher. What it can answer is the capability interfaces it also
// implements: Searcher, Contexter, Extractor.
type Provider interface {
	Name() string
	Health(ctx context.Context) error
}

// SearchRequest is one §3.2 request, its bounds applied.
type SearchRequest struct {
	Query     string
	N         int
	Freshness string // pd, pw, pm, py, YYYY-MM-DDtoYYYY-MM-DD, or ""
}

// Result is one search result as the provider gave it.
type Result struct {
	Title     string
	URL       string
	Snippet   string
	Snippets  []string // more passages from the page
	Published string   // an ISO date, or ""
}

// Results is one search answer: the items in the provider's rank order,
// and the query it searched when it corrected this one ("" when it did
// not).
type Results struct {
	Items        []Result
	AlteredQuery string
}

// Searcher is a provider of /v1/search.
type Searcher interface {
	Search(ctx context.Context, r SearchRequest) (Results, error)
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

// Context is one §3.5 answer.
type Context struct {
	Sources []Source
}

// Contexter is a provider of /v1/context.
type Contexter interface {
	Context(ctx context.Context, r ContextRequest) (Context, error)
}

// ExtractRequest is one §3.3 request: a URL that passed the guard's step
// 1, and the question its text should answer ("" for the whole page).
type ExtractRequest struct {
	URL   string
	Focus string
}

// Page is one extracted document. Status and ContentType are 0 and ""
// when the provider does not report them (an extraction API).
type Page struct {
	URL         string
	FinalURL    string
	Status      int
	ContentType string // the media type alone, e.g. "text/html"
	Title       string
	Published   string // an ISO date, or ""
	Text        string
	Focused     bool // Text is the parts relevant to the request's Focus
	Truncated   bool // the source was cut before extraction
}

// Extractor is a provider of /v1/fetch.
type Extractor interface {
	Extract(ctx context.Context, r ExtractRequest) (*Page, error)
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

// Bill is what a provider charged, as far as holocron can count it: the
// billed units (requests, or URLs for an extraction) and their estimated
// cost in millionths of a US dollar, from §5's price table.
type Bill struct {
	Units  int64
	Micros int64
}

// Add returns b with o added.
func (b Bill) Add(o Bill) Bill { return Bill{b.Units + o.Units, b.Micros + o.Micros} }

// Biller is a metered provider: what it billed since the process started.
type Biller interface {
	Billed() Bill
}

// Budgeter is a provider that gets at most a share (0 to 1) of the
// request's remaining budget when another member follows it, so that one
// is still asked in time (§5.4).
type Budgeter interface {
	BudgetShare() float64
}

// UnavailableError is a provider that cannot answer: unreachable, held
// back, failing (provider_unavailable). Reason is §10's category: quota,
// key_refused, rate_limited, timeout; "" is error.
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
	ReasonTimeout     = "timeout"
	ReasonUnreadable  = "unreadable" // a page the provider could not read
	ReasonError       = "error"
)

// Fallback is a provider a chain moved past, and why.
type Fallback struct {
	Provider string
	Reason   string
}

// Trace collects how one call was answered, for its log line (§10): the
// providers a chain moved past, each metered provider's quota after its
// answer, and what each billed. It is safe for concurrent use; a nil
// *Trace ignores everything.
type Trace struct {
	mu        sync.Mutex
	fallbacks []Fallback
	quotas    map[string]Quota
	billed    map[string]Bill
	claimed   bool
}

// Bill records what provider billed for this call.
func (t *Trace) Bill(provider string, b Bill) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.billed == nil {
		t.billed = map[string]Bill{}
	}
	t.billed[provider] = t.billed[provider].Add(b)
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

// Billed returns what each provider billed.
func (t *Trace) Billed() map[string]Bill {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]Bill, len(t.billed))
	for k, v := range t.billed {
		out[k] = v
	}
	return out
}

// Cost is the call's estimated cost in micro-dollars, all providers.
func (t *Trace) Cost() int64 {
	var n int64
	for _, b := range t.Billed() {
		n += b.Micros
	}
	return n
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
// no-cache, for a provider that has a cache of its own (§8.1).
func WithNoCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, noCacheKey{}, true)
}

// NoCache reports WithNoCache.
func NoCache(ctx context.Context) bool {
	v, _ := ctx.Value(noCacheKey{}).(bool)
	return v
}
