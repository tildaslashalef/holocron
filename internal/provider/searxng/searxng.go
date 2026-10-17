// Package searxng is the SearXNG provider (SPEC.md §5.1): holocron's own
// instance, read through its JSON API.
package searxng

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tildaslashalef/holocron/internal/provider"
)

// maxBody bounds a SearXNG answer; a page of results is tens of KiB.
const maxBody = 4 << 20

// Provider asks the SearXNG instance at Base.
type Provider struct {
	base   *url.URL
	client *http.Client
}

// New returns a Provider for the instance at base, e.g.
// "http://127.0.0.1:8888".
func New(base string) (*Provider, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("searxng: %q is not an http(s) base URL", base)
	}
	return &Provider{
		base: u,
		// Local and trusted: no guard, and no proxy from the environment.
		client: &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConns: 8, IdleConnTimeout: 60 * time.Second}},
	}, nil
}

func (p *Provider) Name() string { return "searxng" }

type response struct {
	Results []struct {
		URL           string `json:"url"`
		Title         string `json:"title"`
		Content       string `json:"content"`
		Engine        string `json:"engine"`
		PublishedDate any    `json:"publishedDate"`
	} `json:"results"`
	UnresponsiveEngines [][]any `json:"unresponsive_engines"`
}

// timeRanges maps §3.2's freshness to SearXNG's time_range; a date range
// has none.
var timeRanges = map[string]string{"pd": "day", "pw": "week", "pm": "month", "py": "year"}

// Search asks for one page of results; SearXNG has no count parameter, so
// n is left to the HTTP layer's bounds.
func (p *Provider) Search(ctx context.Context, req provider.SearchRequest) (provider.Results, error) {
	u := p.base.JoinPath("search")
	params := url.Values{
		"q":          {req.Query},
		"format":     {"json"},
		"categories": {"general,it"},
		"safesearch": {"0"},
	}
	if tr, ok := timeRanges[req.Freshness]; ok {
		params.Set("time_range", tr)
	}
	u.RawQuery = params.Encode()
	var r response
	if err := p.get(ctx, u, &r); err != nil {
		return provider.Results{}, err
	}
	out := provider.Results{EnginesFailed: []string{}}
	for _, e := range r.UnresponsiveEngines {
		if len(e) > 0 {
			if name, ok := e[0].(string); ok {
				out.EnginesFailed = append(out.EnginesFailed, name)
			}
		}
	}
	for _, it := range r.Results {
		out.Items = append(out.Items, provider.Result{
			Title:     it.Title,
			URL:       it.URL,
			Snippet:   it.Content,
			Engine:    it.Engine,
			Published: isoDate(it.PublishedDate),
		})
	}
	return out, nil
}

// Health is SearXNG's /healthz answering 200.
func (p *Provider) Health(ctx context.Context) error {
	return p.get(ctx, p.base.JoinPath("healthz"), nil)
}

// get fetches u and decodes its JSON into v (nil: the body is ignored).
func (p *Provider) get(ctx context.Context, u *url.URL, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return &provider.UnavailableError{Msg: fmt.Sprintf("searxng: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return &provider.TimeoutError{Msg: fmt.Sprintf("searxng at %s did not answer in time", p.base.Host)}
		}
		return &provider.UnavailableError{Msg: fmt.Sprintf("searxng at %s is unreachable: %v", p.base.Host, unwrap(err))}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &provider.UnavailableError{Msg: fmt.Sprintf("searxng at %s answered %s", p.base.Host, resp.Status)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if ctx.Err() != nil {
			return &provider.TimeoutError{Msg: fmt.Sprintf("searxng at %s did not answer in time", p.base.Host)}
		}
		return &provider.UnavailableError{Msg: fmt.Sprintf("searxng at %s: %v", p.base.Host, err)}
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return &provider.UnavailableError{Msg: fmt.Sprintf("searxng at %s sent an unreadable answer: %v", p.base.Host, err)}
	}
	return nil
}

// isoDate turns SearXNG's publishedDate (an ISO date-time, or null) into
// an ISO date, or "".
func isoDate(v any) string {
	s, ok := v.(string)
	if !ok || len(s) < 10 {
		return ""
	}
	if _, err := time.Parse(time.DateOnly, s[:10]); err != nil {
		return ""
	}
	return s[:10]
}

func unwrap(err error) error {
	if u, ok := errors.AsType[*url.Error](err); ok {
		return u.Err
	}
	return err
}
