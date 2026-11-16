// Package parallel is the Parallel provider (SPEC.md §5.1): its Search API
// for /v1/search and /v1/context, and its Extract API for /v1/fetch,
// metered by a key.
package parallel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/guard"
	"github.com/tildaslashalef/holocron/internal/provider"
)

// API is Parallel's base URL.
const API = "https://api.parallel.ai"

// The modes of the Search API (§5.1): turbo and fast cost $1 per 1,000
// requests, basic and advanced $5.
const (
	ModeTurbo    = "turbo"
	ModeFast     = "fast"
	ModeBasic    = "basic"
	ModeAdvanced = "advanced"
)

// Modes are the valid modes.
var Modes = []string{ModeTurbo, ModeFast, ModeBasic, ModeAdvanced}

// Prices in micro-dollars (Parallel's pricing page, read 2026-11-16).
const (
	priceFast      = 1_000 // per search request, turbo or fast, 10 results included
	priceAdvanced  = 5_000 // per search request, basic or advanced
	pricePerResult = 1_000 // each result beyond 10
	priceExtract   = 1_000 // per URL extracted
)

const (
	keyHold   = 10 * time.Minute
	quotaHold = time.Hour
	maxBody   = 8 << 20
	maxRetry  = time.Second

	// maxQuery bounds a search query (Parallel: 200 characters).
	maxQuery = 200
	// MaxFullContent bounds an extracted page's text, in characters.
	MaxFullContent = 500_000
	// focusChars bounds a focused extraction's excerpts.
	focusChars = 8_000
	// searchCharsPerResult is the excerpt budget asked per search result:
	// §3.2 keeps a snippet and five more of ≤ 500 characters.
	searchCharsPerResult = 3_000
	// charsPerToken converts §3.5's tokens to Parallel's characters.
	charsPerToken = 4
)

// Provider asks Parallel with one key. It is safe for concurrent use.
type Provider struct {
	base        *url.URL
	key         string
	userAgent   string
	searchMode  string
	contextMode string
	client      *http.Client
	resolver    *net.Resolver
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error

	hold  provider.Hold
	meter provider.Meter
}

// Options configure New.
type Options struct {
	SearchMode  string // /v1/search's mode; "" is fast
	ContextMode string // /v1/context's mode; "" is advanced
	UserAgent   string
}

// New reads the key from keyFile and returns a Provider.
func New(keyFile string, o Options) (*Provider, error) {
	if keyFile == "" {
		return nil, errors.New("parallel: no key: --parallel-key-file, or the parallel-key credential")
	}
	key, err := provider.ReadKey("parallel", keyFile)
	if err != nil {
		return nil, err
	}
	return newProvider(API, key, o)
}

func newProvider(base, key string, o Options) (*Provider, error) {
	if o.SearchMode == "" {
		o.SearchMode = ModeFast
	}
	if o.ContextMode == "" {
		o.ContextMode = ModeAdvanced
	}
	for _, m := range []string{o.SearchMode, o.ContextMode} {
		if !validMode(m) {
			return nil, fmt.Errorf("parallel: mode %q is not one of %s", m, strings.Join(Modes, ", "))
		}
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	return &Provider{
		base:        u,
		key:         key,
		userAgent:   o.UserAgent,
		searchMode:  o.SearchMode,
		contextMode: o.ContextMode,
		client:      provider.APIClient(),
		now:         time.Now,
		sleep:       sleep,
	}, nil
}

func validMode(m string) bool {
	for _, v := range Modes {
		if m == v {
			return true
		}
	}
	return false
}

func (p *Provider) Name() string { return "parallel" }

// Health makes no call, since every call is billed: an error while a
// refused key or a used-up credit holds calls back.
func (p *Provider) Health(context.Context) error { return p.hold.Err(p.now()) }

// Billed is what Parallel billed since start, as far as its answers say.
func (p *Provider) Billed() provider.Bill { return p.meter.Billed() }

// BudgetShare leaves the rest of a request's budget to the member after
// Parallel in a chain (§5.4).
func (p *Provider) BudgetShare() float64 { return 0.6 }

// Modes names the search and context modes, for the startup log line.
func (p *Provider) Modes() (search, context string) { return p.searchMode, p.contextMode }

type sourcePolicy struct {
	AfterDate string `json:"after_date,omitempty"`
}

type searchSettings struct {
	MaxResults   int           `json:"max_results"`
	SourcePolicy *sourcePolicy `json:"source_policy,omitempty"`
}

type searchBody struct {
	Objective        string         `json:"objective"`
	SearchQueries    []string       `json:"search_queries"`
	Mode             string         `json:"mode"`
	MaxCharsTotal    int            `json:"max_chars_total"`
	AdvancedSettings searchSettings `json:"advanced_settings"`
}

type usage []struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type searchResponse struct {
	Results []struct {
		URL         string   `json:"url"`
		Title       *string  `json:"title"`
		PublishDate *string  `json:"publish_date"`
		Excerpts    []string `json:"excerpts"`
	} `json:"results"`
	Usage usage `json:"usage"`
}

// search is one call to the Search API, its results bounded by freshness.
func (p *Provider) search(ctx context.Context, query, mode, freshness string, results, chars int) (*searchResponse, error) {
	after, before := window(freshness, p.now())
	body := searchBody{
		Objective:        query,
		SearchQueries:    []string{clip(query, maxQuery)},
		Mode:             mode,
		MaxCharsTotal:    chars,
		AdvancedSettings: searchSettings{MaxResults: results},
	}
	if after != "" {
		body.AdvancedSettings.SourcePolicy = &sourcePolicy{AfterDate: after}
	}
	var r searchResponse
	if err := p.post(ctx, "v1/search", body, &r, func(u usage) provider.Bill {
		price := int64(priceFast)
		if mode == ModeBasic || mode == ModeAdvanced {
			price = priceAdvanced
		}
		return provider.Bill{Units: 1, Micros: price + int64(max(len(r.Results)-10, 0))*pricePerResult}
	}); err != nil {
		return nil, err
	}
	if before != "" {
		kept := r.Results[:0]
		for _, it := range r.Results {
			if it.PublishDate == nil || *it.PublishDate <= before {
				kept = append(kept, it)
			}
		}
		r.Results = kept
	}
	return &r, nil
}

// Search asks the Search API in the search mode for n results.
func (p *Provider) Search(ctx context.Context, req provider.SearchRequest) (provider.Results, error) {
	n := min(max(req.N, 1), 20)
	r, err := p.search(ctx, req.Query, p.searchMode, req.Freshness, n, n*searchCharsPerResult)
	if err != nil {
		return provider.Results{}, err
	}
	var out provider.Results
	for _, it := range r.Results {
		passages := passages(it.Excerpts)
		res := provider.Result{Title: deref(it.Title), URL: it.URL, Published: isoDate(deref(it.PublishDate))}
		if len(passages) > 0 {
			res.Snippet, res.Snippets = passages[0], passages[1:]
		}
		out.Items = append(out.Items, res)
	}
	return out, nil
}

// Context asks the Search API in the context mode, its excerpts bounded
// by the request's tokens and URLs.
func (p *Provider) Context(ctx context.Context, req provider.ContextRequest) (provider.Context, error) {
	r, err := p.search(ctx, req.Query, p.contextMode, req.Freshness, min(max(req.URLs, 1), 20), req.Tokens*charsPerToken)
	if err != nil {
		return provider.Context{}, err
	}
	var out provider.Context
	for _, it := range r.Results {
		out.Sources = append(out.Sources, provider.Source{
			URL: it.URL, Title: deref(it.Title), Published: isoDate(deref(it.PublishDate)), Snippets: it.Excerpts,
		})
	}
	return out, nil
}

type fullContent struct {
	MaxCharsPerResult int `json:"max_chars_per_result"`
}

type excerptSettings struct {
	MaxCharsPerResult int `json:"max_chars_per_result"`
}

type extractSettings struct {
	FullContent     any             `json:"full_content"`
	ExcerptSettings excerptSettings `json:"excerpt_settings"`
}

type extractBody struct {
	URLs             []string        `json:"urls"`
	Objective        string          `json:"objective,omitempty"`
	MaxCharsTotal    int             `json:"max_chars_total,omitempty"`
	AdvancedSettings extractSettings `json:"advanced_settings"`
}

type extractResponse struct {
	Results []struct {
		URL         string   `json:"url"`
		Title       *string  `json:"title"`
		PublishDate *string  `json:"publish_date"`
		Excerpts    []string `json:"excerpts"`
		FullContent *string  `json:"full_content"`
	} `json:"results"`
	Errors []struct {
		URL            string  `json:"url"`
		ErrorType      string  `json:"error_type"`
		HTTPStatusCode *int    `json:"http_status_code"`
		Content        *string `json:"content"`
	} `json:"errors"`
	Usage usage `json:"usage"`
}

// Extract asks the Extract API for one page: its whole text, or with a
// focus the excerpts that answer it. The host is resolved first, so a
// name that is private here is never sent (§6).
func (p *Provider) Extract(ctx context.Context, req provider.ExtractRequest) (*provider.Page, error) {
	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, &guard.BadURLError{Msg: fmt.Sprintf("%q is not a URL", req.URL)}
	}
	host := u.Hostname()
	// Step 1 again, so this provider never sends a private target whoever
	// calls it; then the names' addresses.
	if err := guard.CheckURL(u); err != nil {
		return nil, err
	}
	if err := guard.CheckHost(ctx, p.resolver, u); err != nil {
		if b, ok := errors.AsType[*guard.BlockedError](err); ok {
			return nil, b
		}
		if ctx.Err() != nil {
			return nil, timeout()
		}
		if d, ok := errors.AsType[*net.DNSError](err); ok {
			return nil, &fetch.UpstreamError{Msg: fmt.Sprintf("%s: DNS: %s", host, d.Err)}
		}
		return nil, &fetch.UpstreamError{Msg: fmt.Sprintf("%s: %v", host, err)}
	}
	body := extractBody{URLs: []string{req.URL}}
	if req.Focus != "" {
		body.Objective = req.Focus
		body.MaxCharsTotal = focusChars
		body.AdvancedSettings = extractSettings{FullContent: false, ExcerptSettings: excerptSettings{focusChars}}
	} else {
		body.AdvancedSettings = extractSettings{FullContent: fullContent{MaxFullContent}, ExcerptSettings: excerptSettings{1_000}}
	}
	var r extractResponse
	if err := p.post(ctx, "v1/extract", body, &r, func(u usage) provider.Bill {
		var n int64
		for _, it := range u {
			if strings.HasPrefix(it.Name, "sku_extract") {
				n = max(n, it.Count)
			}
		}
		if n == 0 {
			n = int64(len(r.Results))
		}
		return provider.Bill{Units: n, Micros: n * priceExtract}
	}); err != nil {
		return nil, err
	}
	if len(r.Results) == 0 {
		if len(r.Errors) == 0 {
			return nil, &provider.UnavailableError{Msg: "parallel returned neither the page nor an error", Reason: provider.ReasonUnreadable}
		}
		e := r.Errors[0]
		if e.HTTPStatusCode != nil && (*e.HTTPStatusCode == http.StatusNotFound || *e.HTTPStatusCode == http.StatusGone) {
			return nil, &fetch.UpstreamError{Msg: fmt.Sprintf("%s answered %d %s", host, *e.HTTPStatusCode, http.StatusText(*e.HTTPStatusCode))}
		}
		msg := "parallel could not read " + host + ": " + e.ErrorType
		if e.HTTPStatusCode != nil {
			msg += " " + strconv.Itoa(*e.HTTPStatusCode)
		}
		return nil, &provider.UnavailableError{Msg: msg, Reason: provider.ReasonUnreadable}
	}
	it := r.Results[0]
	page := &provider.Page{
		URL:       req.URL,
		FinalURL:  it.URL,
		Title:     deref(it.Title),
		Published: isoDate(deref(it.PublishDate)),
	}
	if page.FinalURL == "" {
		page.FinalURL = req.URL
	}
	if req.Focus != "" {
		page.Text, page.Focused = strings.Join(it.Excerpts, "\n\n"), true
	} else {
		page.Text = deref(it.FullContent)
		page.Truncated = utf8.RuneCountInString(page.Text) >= MaxFullContent
	}
	if strings.TrimSpace(page.Text) == "" {
		return nil, &provider.UnavailableError{Msg: "parallel read no text from " + host, Reason: provider.ReasonUnreadable}
	}
	return page, nil
}

// errorBody is either of the error shapes Parallel answers: the
// documented {"error": {"message", "detail"}}, and {"code", "message"}
// (a refused key, measured 2026-11-16).
type errorBody struct {
	Message string `json:"message"`
	Error   struct {
		Message string `json:"message"`
		Detail  any    `json:"detail"`
	} `json:"error"`
}

func detail(body []byte) string {
	var e errorBody
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	msg := e.Error.Message
	if msg == "" {
		msg = e.Message
	}
	if e.Error.Detail != nil {
		if d, err := json.Marshal(e.Error.Detail); err == nil && len(d) < 300 {
			msg += " " + string(d)
		}
	}
	if msg == "" {
		return ""
	}
	return ": " + msg
}

// post sends one request and decodes a 200 into v, mapping Parallel's
// failures to the contract's (§5.1's table); bill says what a 200 cost.
// A 429 with at most a second to wait is retried once.
func (p *Provider) post(ctx context.Context, path string, body, v any, bill func(usage) provider.Bill) error {
	if err := p.hold.Err(p.now()); err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u := p.base.JoinPath(path)
	for attempt := 0; ; attempt++ {
		status, header, b, err := p.do(ctx, u, payload)
		if err != nil {
			return err
		}
		switch {
		case status == http.StatusOK:
			if err := json.Unmarshal(b, v); err != nil {
				return &provider.UnavailableError{Msg: fmt.Sprintf("parallel sent an unreadable answer: %v", err)}
			}
			var w struct {
				Usage usage `json:"usage"`
			}
			json.Unmarshal(b, &w)
			cost := bill(w.Usage)
			p.meter.Add(cost)
			provider.TraceOf(ctx).Bill(p.Name(), cost)
			return nil
		case status == http.StatusTooManyRequests:
			wait := retryAfter(header.Get("Retry-After"))
			if attempt == 0 && wait <= maxRetry {
				if err := p.sleep(ctx, max(wait, 200*time.Millisecond)); err != nil {
					return timeout()
				}
				continue
			}
			return &provider.UnavailableError{Msg: "parallel: rate-limited" + detail(b), Reason: provider.ReasonRateLimited}
		case status == http.StatusPaymentRequired:
			return p.hold.Set(p.now(), quotaHold, provider.ReasonQuota, "parallel: credit used up"+detail(b))
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return p.hold.Set(p.now(), keyHold, provider.ReasonKeyRefused, fmt.Sprintf("parallel refused the key (%d)%s", status, detail(b)))
		default:
			return &provider.UnavailableError{Msg: fmt.Sprintf("parallel answered %d%s", status, detail(b))}
		}
	}
}

// do sends one POST and reads its body.
func (p *Provider) do(ctx context.Context, u *url.URL, payload []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, &provider.UnavailableError{Msg: fmt.Sprintf("parallel: %v", err)}
	}
	req.Header.Set("x-api-key", p.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, nil, timeout()
		}
		// The error names the URL, never the key, which is a header.
		return 0, nil, nil, &provider.UnavailableError{Msg: fmt.Sprintf("parallel is unreachable: %v", unwrap(err))}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, timeout()
		}
		return 0, nil, nil, &provider.UnavailableError{Msg: fmt.Sprintf("parallel: %v", err)}
	}
	return resp.StatusCode, resp.Header, b, nil
}

// window turns §3.2's freshness into Parallel's after_date and the latest
// publish date to keep ("" for none): Parallel filters only from a date.
func window(freshness string, now time.Time) (after, before string) {
	day := now.UTC()
	switch freshness {
	case "":
		return "", ""
	case "pd":
		return day.AddDate(0, 0, -1).Format(time.DateOnly), ""
	case "pw":
		return day.AddDate(0, 0, -7).Format(time.DateOnly), ""
	case "pm":
		return day.AddDate(0, -1, 0).Format(time.DateOnly), ""
	case "py":
		return day.AddDate(-1, 0, 0).Format(time.DateOnly), ""
	}
	from, to, _ := strings.Cut(freshness, "to")
	return from, to
}

// passages splits excerpts into their paragraphs, dropping fragments too
// short to read on their own when longer ones exist.
func passages(excerpts []string) []string {
	var all, long []string
	for _, e := range excerpts {
		for part := range strings.SplitSeq(e, "\n\n") {
			part = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(part), "#"))
			if part == "" || part == "..." || part == "[...]" {
				continue
			}
			all = append(all, part)
			if utf8.RuneCountInString(part) >= 40 {
				long = append(long, part)
			}
		}
	}
	if len(long) > 0 {
		return long
	}
	return all
}

// clip cuts s to at most n characters at a word.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n]
	if i := strings.LastIndexByte(string(r), ' '); i > 0 {
		return string(r)[:i]
	}
	return string(r)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// isoDate is s when it is a YYYY-MM-DD date, else "".
func isoDate(s string) string {
	if len(s) < 10 {
		return ""
	}
	if _, err := time.Parse(time.DateOnly, s[:10]); err != nil {
		return ""
	}
	return s[:10]
}

// retryAfter reads a Retry-After in seconds; absent or unreadable is
// longer than any retry.
func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n < 0 {
		return time.Hour
	}
	return time.Duration(n) * time.Second
}

func timeout() error {
	return &provider.TimeoutError{Msg: "parallel did not answer in time"}
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
