// Package api is the v1 contract (SPEC.md §3): its types and error codes,
// the handlers, the request slots, and the server around them (§8, §10).
package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/tildaslashalef/holocron/internal/extract"
	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/guard"
	"github.com/tildaslashalef/holocron/internal/provider"
)

// The contract's bounds (§3.2, §3.3).
const (
	MaxQuery        = 400
	DefaultN        = 8
	MaxN            = 10
	MaxTitle        = 200
	MaxSnippet      = 500
	DefaultMaxChars = 20_000
	MaxMaxChars     = 100_000
	MinTokens       = 1_024
	DefaultTokens   = 4_096
	MaxTokens       = 16_384
	DefaultURLs     = 8
	MaxURLs         = 20
	CharsPerToken   = 8 // §3.5's backstop on the snippets' total
)

// Search is §3.2's answer.
type Search struct {
	Query         string   `json:"query"`
	Provider      string   `json:"provider"`
	Results       []Result `json:"results"`
	AlteredQuery  *string  `json:"altered_query"`
	EnginesFailed []string `json:"engines_failed"`
}

// Result is one search result within §3.2's bounds.
type Result struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Snippet       string   `json:"snippet"`
	Engine        string   `json:"engine"`
	Published     *string  `json:"published"`
	ExtraSnippets []string `json:"extra_snippets"`
}

// MaxExtraSnippets bounds a result's extra_snippets (§3.2).
const MaxExtraSnippets = 5

// Fetch is §3.3's answer.
type Fetch struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	Start       int    `json:"start"`
	Chars       int    `json:"chars"`
	Truncated   bool   `json:"truncated"`
}

// Context is §3.5's answer.
type Context struct {
	Query         string   `json:"query"`
	Provider      string   `json:"provider"`
	Sources       []Source `json:"sources"`
	EnginesFailed []string `json:"engines_failed"`
}

// Source is one page's passages within §3.5's bounds.
type Source struct {
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	Published *string  `json:"published"`
	Snippets  []string `json:"snippets"`
}

// Health is §3.4's answer.
type Health struct {
	Version   string                `json:"version"`
	Providers map[string]string     `json:"providers"`
	Endpoints []string              `json:"endpoints"`
	Billed    map[string]int64      `json:"billed,omitempty"`
	Quota     map[string]QuotaStats `json:"quota,omitempty"`
	Cache     *CacheStats           `json:"cache,omitempty"`
}

// QuotaStats is a metered provider's remaining requests (§3.4).
type QuotaStats struct {
	Remaining int64 `json:"remaining"`
	ResetsInS int64 `json:"resets_in_s"`
}

// Error is a failure with its §3.1 code and status.
type Error struct {
	Code    string
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ErrorBody is §3.1's error shape.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func badRequest(msg string) *Error {
	return &Error{"bad_request", http.StatusBadRequest, msg}
}

// Classify maps any error to its §3.1 code. An error it does not know is a
// bug: internal.
func Classify(err error) *Error {
	var (
		apiErr      *Error
		badURL      *guard.BadURLError
		blocked     *guard.BlockedError
		unsupported *fetch.UnsupportedError
		upstream    *fetch.UpstreamError
		fetchTime   *fetch.TimeoutError
		provTime    *provider.TimeoutError
		unavailable *provider.UnavailableError
		badQuery    *provider.BadQueryError
		notAvail    *provider.NotAvailableError
	)
	switch {
	case errors.As(err, &apiErr):
		return apiErr
	case errors.As(err, &badURL):
		return badRequest(badURL.Msg)
	case errors.As(err, &blocked):
		return &Error{"blocked_address", http.StatusForbidden, blocked.Msg}
	case errors.As(err, &unsupported):
		return &Error{"unsupported_content", http.StatusUnsupportedMediaType, unsupported.Msg}
	case errors.As(err, &upstream):
		return &Error{"upstream_failed", http.StatusBadGateway, upstream.Msg}
	case errors.As(err, &fetchTime):
		return &Error{"upstream_timeout", http.StatusGatewayTimeout, fetchTime.Msg}
	case errors.As(err, &provTime):
		return &Error{"upstream_timeout", http.StatusGatewayTimeout, provTime.Msg}
	case errors.As(err, &unavailable):
		return &Error{"no_engines", http.StatusServiceUnavailable, unavailable.Msg}
	case errors.As(err, &badQuery):
		return badRequest(badQuery.Msg)
	case errors.As(err, &notAvail):
		return &Error{"not_available", http.StatusNotImplemented, notAvail.Msg}
	}
	return &Error{"internal", http.StatusInternalServerError, "a bug in holocron; the log has the detail"}
}

// NewSearch applies §3.2's bounds to a provider's answer: http(s) URLs
// only, deduplicated, at most n, titles and snippets clipped. No results
// while engines failed is no_engines.
func NewSearch(query, providerName string, res provider.Results, n int) (*Search, error) {
	if res.Provider != "" {
		providerName = res.Provider
	}
	out := &Search{Query: query, Provider: providerName, Results: []Result{}, EnginesFailed: []string{}}
	if res.EnginesFailed != nil {
		out.EnginesFailed = res.EnginesFailed
	}
	if res.AlteredQuery != "" {
		a := res.AlteredQuery
		out.AlteredQuery = &a
	}
	seen := map[string]bool{}
	for _, it := range res.Items {
		if len(out.Results) == n {
			break
		}
		u, err := url.Parse(strings.TrimSpace(it.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		key := u.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		r := Result{
			Title:   extract.Clip(collapse(it.Title), MaxTitle),
			URL:     key,
			Snippet: extract.Clip(collapse(it.Snippet), MaxSnippet),
			Engine:  it.Engine,
		}
		r.ExtraSnippets = []string{}
		for _, e := range it.ExtraSnippets {
			if e = extract.Clip(collapse(e), MaxSnippet); e != "" && len(r.ExtraSnippets) < MaxExtraSnippets {
				r.ExtraSnippets = append(r.ExtraSnippets, e)
			}
		}
		if r.Engine == "" {
			r.Engine = providerName
		}
		if it.Published != "" {
			p := it.Published
			r.Published = &p
		}
		out.Results = append(out.Results, r)
	}
	if len(out.Results) == 0 && len(out.EnginesFailed) > 0 {
		return nil, &Error{"no_engines", http.StatusServiceUnavailable,
			"no results, and these engines failed: " + strings.Join(out.EnginesFailed, ", ")}
	}
	return out, nil
}

// NewContext applies §3.5's bounds to a provider's answer: http(s) URLs
// only, deduplicated, titles clipped, the snippets' total cut at
// CharsPerToken × tokens characters. No sources while engines failed is
// no_engines.
func NewContext(query, providerName string, res provider.Context, tokens int) (*Context, error) {
	if res.Provider != "" {
		providerName = res.Provider
	}
	out := &Context{Query: query, Provider: providerName, Sources: []Source{}, EnginesFailed: []string{}}
	if res.EnginesFailed != nil {
		out.EnginesFailed = res.EnginesFailed
	}
	budget := CharsPerToken * tokens
	seen := map[string]bool{}
	for _, it := range res.Sources {
		if budget <= 0 {
			break
		}
		u, err := url.Parse(strings.TrimSpace(it.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		key := u.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		src := Source{URL: key, Title: extract.Clip(collapse(it.Title), MaxTitle), Snippets: []string{}}
		if it.Published != "" {
			p := it.Published
			src.Published = &p
		}
		for _, sn := range it.Snippets {
			if budget <= 0 {
				break
			}
			if n := utf8.RuneCountInString(sn); n > budget {
				sn, _ = window(sn, 0, budget)
			}
			budget -= utf8.RuneCountInString(sn)
			src.Snippets = append(src.Snippets, sn)
		}
		out.Sources = append(out.Sources, src)
	}
	if len(out.Sources) == 0 && len(out.EnginesFailed) > 0 {
		return nil, &Error{"no_engines", http.StatusServiceUnavailable,
			"no sources, and these engines failed: " + strings.Join(out.EnginesFailed, ", ")}
	}
	return out, nil
}

// NewFetch pages a fetched document: maxChars characters from start,
// counted in code points.
func NewFetch(p *fetch.Page, start, maxChars int) *Fetch {
	text, chars := window(p.Text, start, maxChars)
	return &Fetch{
		URL:         p.URL,
		FinalURL:    p.FinalURL,
		Status:      p.Status,
		ContentType: p.ContentType,
		Title:       p.Title,
		Text:        text,
		Start:       start,
		Chars:       chars,
		Truncated:   p.Truncated || start+utf8.RuneCountInString(text) < chars,
	}
}

// window returns at most n code points of s from code point start, and the
// number of code points in s.
func window(s string, start, n int) (string, int) {
	chars := utf8.RuneCountInString(s)
	if start >= chars {
		return "", chars
	}
	from, to, i := len(s), len(s), 0
	for off := range s {
		if i == start {
			from = off
		}
		if i == start+n {
			to = off
			break
		}
		i++
	}
	return s[from:to], chars
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
