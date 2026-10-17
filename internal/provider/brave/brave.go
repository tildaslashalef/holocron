// Package brave is the Brave Search API provider (SPEC.md §5.2): web
// search for /v1/search and LLM Context for /v1/context, metered by a key.
package brave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tildaslashalef/holocron/internal/provider"
)

// API is Brave's base URL.
const API = "https://api.search.brave.com/res"

// The holds after a failure that more calls cannot fix (§5.2).
const (
	keyHold   = 10 * time.Minute
	quotaHold = time.Hour // when Brave gave no reset
	maxBody   = 4 << 20
	maxRetry  = time.Second
)

// Provider asks Brave with one key. It is safe for concurrent use.
type Provider struct {
	base      *url.URL
	key       string
	userAgent string
	client    *http.Client
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	billed    atomic.Int64

	mu         sync.Mutex
	quota      provider.Quota
	quotaAt    time.Time // when quota was read; zero before the first answer
	heldUntil  time.Time
	heldWhy    string
	heldReason string
}

// New reads the key from keyFile and returns a Provider that names itself
// userAgent.
func New(keyFile, userAgent string) (*Provider, error) {
	if keyFile == "" {
		return nil, errors.New("brave: --brave-key-file is required")
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("brave: reading the key: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, fmt.Errorf("brave: %s does not hold one key on one line", keyFile)
	}
	return newProvider(API, key, userAgent), nil
}

func newProvider(base, key, userAgent string) *Provider {
	u, _ := url.Parse(base)
	return &Provider{
		base:      u,
		key:       key,
		userAgent: userAgent,
		client:    &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConns: 8, IdleConnTimeout: 60 * time.Second, ForceAttemptHTTP2: true}},
		now:       time.Now,
		sleep:     sleep,
	}
}

func (p *Provider) Name() string { return "brave" }

// Health makes no call, since every call is billed: an error while a
// refused key or a used-up quota holds calls back.
func (p *Provider) Health(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.now().Before(p.heldUntil) {
		return &provider.UnavailableError{Msg: p.heldWhy, Reason: p.heldReason}
	}
	return nil
}

// Quota is the month's remaining requests from the last answer, the reset
// counted down since.
func (p *Provider) Quota() (provider.Quota, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.quotaAt.IsZero() {
		return provider.Quota{}, false
	}
	q := p.quota
	q.ResetsIn = max(q.ResetsIn-p.now().Sub(p.quotaAt), 0)
	return q, true
}

// Billed is the successful answers since start: what Brave bills.
func (p *Provider) Billed() int64 { return p.billed.Load() }

type webResponse struct {
	Query struct {
		Original string `json:"original"`
		Altered  string `json:"altered"`
	} `json:"query"`
	Web struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			Description   string   `json:"description"`
			PageAge       string   `json:"page_age"`
			ExtraSnippets []string `json:"extra_snippets"`
		} `json:"results"`
	} `json:"web"`
}

// Search asks Brave's web search for n results (at most 20).
func (p *Provider) Search(ctx context.Context, req provider.SearchRequest) (provider.Results, error) {
	params := url.Values{
		"q":                {req.Query},
		"count":            {strconv.Itoa(min(max(req.N, 1), 20))},
		"safesearch":       {"off"},
		"text_decorations": {"false"},
		"result_filter":    {"web"},
		"extra_snippets":   {"true"},
	}
	if req.Freshness != "" {
		params.Set("freshness", req.Freshness)
	}
	var r webResponse
	if err := p.get(ctx, "v1/web/search", params, &r); err != nil {
		return provider.Results{}, err
	}
	out := provider.Results{EnginesFailed: []string{}}
	if a := strings.TrimSpace(r.Query.Altered); a != "" && a != req.Query {
		out.AlteredQuery = a
	}
	for _, it := range r.Web.Results {
		extra := make([]string, 0, len(it.ExtraSnippets))
		for _, e := range it.ExtraSnippets {
			extra = append(extra, html.UnescapeString(e))
		}
		out.Items = append(out.Items, provider.Result{
			Title:         html.UnescapeString(it.Title),
			URL:           it.URL,
			Snippet:       html.UnescapeString(it.Description),
			ExtraSnippets: extra,
			Engine:        "brave",
			Published:     isoDate(it.PageAge),
		})
	}
	return out, nil
}

type contextResponse struct {
	Grounding struct {
		Generic []struct {
			URL      string   `json:"url"`
			Title    string   `json:"title"`
			Snippets []string `json:"snippets"`
		} `json:"generic"`
	} `json:"grounding"`
	Sources map[string]struct {
		Age []string `json:"age"`
	} `json:"sources"`
}

// Context asks Brave's LLM Context.
func (p *Provider) Context(ctx context.Context, req provider.ContextRequest) (provider.Context, error) {
	params := url.Values{
		"q":                        {req.Query},
		"count":                    {"20"},
		"maximum_number_of_urls":   {strconv.Itoa(req.URLs)},
		"maximum_number_of_tokens": {strconv.Itoa(req.Tokens)},
		"enable_local":             {"false"},
	}
	if req.Freshness != "" {
		params.Set("freshness", req.Freshness)
	}
	var r contextResponse
	if err := p.get(ctx, "v1/llm/context", params, &r); err != nil {
		return provider.Context{}, err
	}
	out := provider.Context{EnginesFailed: []string{}}
	for _, g := range r.Grounding.Generic {
		src := provider.Source{URL: g.URL, Title: html.UnescapeString(g.Title), Snippets: g.Snippets}
		// age: a full date, YYYY-MM-DD, a relative age, an ISO timestamp.
		if age := r.Sources[g.URL].Age; len(age) > 1 {
			src.Published = isoDate(age[1])
		}
		out.Sources = append(out.Sources, src)
	}
	return out, nil
}

type errorBody struct {
	Error struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Meta   struct {
			Component string `json:"component"`
			Errors    []struct {
				Loc []any  `json:"loc"`
				Msg string `json:"msg"`
			} `json:"errors"`
		} `json:"meta"`
	} `json:"error"`
}

// keyRefused reports Brave refusing the key. It answers 422 with
// SUBSCRIPTION_TOKEN_INVALID (measured 2026-10-16), not 401.
func (e *errorBody) keyRefused() bool {
	return strings.HasPrefix(e.Error.Code, "SUBSCRIPTION_TOKEN") || e.Error.Meta.Component == "authentication"
}

// queryRefused returns Brave's complaints about q, the client's own
// parameter; a complaint about another is holocron's request.
func (e *errorBody) queryRefused() []string {
	var msgs []string
	for _, v := range e.Error.Meta.Errors {
		if len(v.Loc) == 2 && v.Loc[0] == "query" && v.Loc[1] == "q" {
			msgs = append(msgs, v.Msg)
		}
	}
	return msgs
}

// get asks Brave and decodes its JSON into v, mapping its failures to the
// contract's (§5.2's table). A 429 with a second to wait is retried once.
func (p *Provider) get(ctx context.Context, path string, params url.Values, v any) error {
	p.mu.Lock()
	if p.now().Before(p.heldUntil) {
		err := &provider.UnavailableError{Msg: p.heldWhy, Reason: p.heldReason}
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	u := p.base.JoinPath(path)
	u.RawQuery = params.Encode()
	for attempt := 0; ; attempt++ {
		status, header, body, err := p.do(ctx, u, provider.NoCache(ctx))
		if err != nil {
			return err
		}
		if q, ok := p.readQuota(header); ok {
			provider.TraceOf(ctx).SetQuota(p.Name(), q)
		}
		var eb errorBody
		if status != http.StatusOK {
			json.Unmarshal(body, &eb)
		}
		switch {
		case status == http.StatusOK:
			// Brave bills every successful answer, readable or not.
			p.billed.Add(1)
			provider.TraceOf(ctx).Bill(p.Name())
			if err := json.Unmarshal(body, v); err != nil {
				return &provider.UnavailableError{Msg: fmt.Sprintf("brave sent an unreadable answer: %v", err)}
			}
			return nil
		case status == http.StatusTooManyRequests:
			second, month, hasMonth := limits(header.Get("X-RateLimit-Remaining"))
			resetSecond, resetMonth, _ := limits(header.Get("X-RateLimit-Reset"))
			if hasMonth && month == 0 && metered(header) {
				hold := quotaHold
				if resetMonth > 0 {
					hold = time.Duration(resetMonth) * time.Second
				}
				return p.hold(hold, provider.ReasonQuota, "brave: quota or credit used up")
			}
			wait := time.Duration(resetSecond) * time.Second
			if attempt == 0 && second == 0 && wait <= maxRetry {
				if err := p.sleep(ctx, max(wait, 100*time.Millisecond)); err != nil {
					return timeout()
				}
				continue
			}
			return &provider.UnavailableError{Msg: "brave: rate-limited" + detail(body), Reason: provider.ReasonRateLimited}
		case status == http.StatusPaymentRequired:
			return p.hold(quotaHold, provider.ReasonQuota, "brave: quota or credit used up"+detail(body))
		case status == http.StatusUnauthorized || status == http.StatusForbidden || eb.keyRefused():
			return p.hold(keyHold, provider.ReasonKeyRefused, fmt.Sprintf("brave refused the key (%d)%s", status, detail(body)))
		case (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && len(eb.queryRefused()) > 0:
			return &provider.BadQueryError{Msg: "brave refused the query: " + strings.Join(eb.queryRefused(), "; ")}
		default:
			return &provider.UnavailableError{Msg: fmt.Sprintf("brave answered %d%s", status, detail(body))}
		}
	}
}

// do sends one request and reads its body.
func (p *Provider) do(ctx context.Context, u *url.URL, noCache bool) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, nil, &provider.UnavailableError{Msg: fmt.Sprintf("brave: %v", err)}
	}
	req.Header.Set("X-Subscription-Token", p.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	if noCache {
		req.Header.Set("Cache-Control", "no-cache")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, nil, timeout()
		}
		// The error names the URL, never the key, which is a header.
		return 0, nil, nil, &provider.UnavailableError{Msg: fmt.Sprintf("brave is unreachable: %v", unwrap(err))}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, timeout()
		}
		return 0, nil, nil, &provider.UnavailableError{Msg: fmt.Sprintf("brave: %v", err)}
	}
	return resp.StatusCode, resp.Header, body, nil
}

// readQuota keeps the month's remaining requests and reset from an
// answer's X-RateLimit headers ("per-second, per-month").
func (p *Provider) readQuota(h http.Header) (provider.Quota, bool) {
	_, remaining, ok := limits(h.Get("X-RateLimit-Remaining"))
	if !ok || !metered(h) {
		return provider.Quota{}, false
	}
	_, reset, _ := limits(h.Get("X-RateLimit-Reset"))
	q := provider.Quota{Remaining: remaining, ResetsIn: time.Duration(reset) * time.Second}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quota, p.quotaAt = q, p.now()
	return q, true
}

// metered reports a monthly limit: X-RateLimit-Limit's month value is 0
// for unlimited, and its remaining count then means nothing.
func metered(h http.Header) bool {
	_, limit, ok := limits(h.Get("X-RateLimit-Limit"))
	return ok && limit > 0
}

// hold stops calls for d and returns the failure that explains it, with
// its §10 reason.
func (p *Provider) hold(d time.Duration, reason, why string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.heldUntil, p.heldWhy, p.heldReason = p.now().Add(d), why, reason
	return &provider.UnavailableError{Msg: why, Reason: reason}
}

// limits parses an X-RateLimit header's "per-second, per-month" values;
// hasMonth is false when there is no second value.
func limits(h string) (second, month int64, hasMonth bool) {
	parts := strings.Split(h, ",")
	if len(parts) < 2 {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	m, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return s, m, true
}

// detail is ": " and Brave's error.detail, or "".
func detail(body []byte) string {
	var e errorBody
	if json.Unmarshal(body, &e) != nil || e.Error.Detail == "" {
		return ""
	}
	return ": " + e.Error.Detail
}

func timeout() error {
	return &provider.TimeoutError{Msg: "brave did not answer in time"}
}

var datePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// isoDate is the date of an ISO date or date-time, or "".
func isoDate(s string) string {
	d := datePrefix.FindString(s)
	if d == "" {
		return ""
	}
	if _, err := time.Parse(time.DateOnly, d); err != nil {
		return ""
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unwrap(err error) error {
	if u, ok := errors.AsType[*url.Error](err); ok {
		return u.Err
	}
	return err
}
