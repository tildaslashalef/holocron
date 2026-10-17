// Package check is SPEC.md §12's acceptance suite, run against a live
// server: one line per check, and an error if any failed.
package check

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tildaslashalef/holocron/internal/api"
)

// The public pages the suite uses (§12): a failure names its site, so an
// outage elsewhere is told apart from a bug in holocron.
const (
	PEP8         = "https://peps.python.org/pep-0008/"
	PDF          = "https://www.rfc-editor.org/rfc/rfc9110.pdf"
	NotFound     = "https://go.dev/holocron-check-missing-page"
	RedirectSite = "https://httpbin.org/redirect-to?url=http%3A%2F%2F192.168.1.1%2F"
	SearchQuery  = "zig std.http.Client"
)

// Blocked are the guard's cases, each answered blocked_address.
var Blocked = []string{
	"http://192.168.1.20:8006/",
	"http://nas.home.arpa:8006/",
	"http://127.0.0.1:8888/",
	"http://192.168.1.1.nip.io/",
	RedirectSite,
}

// Run runs every check against base (e.g. http://127.0.0.1:8080) and
// reports on out; the error says how many failed.
func Run(ctx context.Context, base, userAgent string, out io.Writer) error {
	c := &checker{
		base: strings.TrimSuffix(base, "/"),
		ua:   userAgent,
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}},
		out:  out,
	}
	endpoints := c.health(ctx)
	c.search(ctx)
	c.context(ctx, endpoints)
	c.fetchPEP8(ctx)
	for _, u := range Blocked {
		c.wantError(ctx, "guard "+u, "/v1/fetch?url="+url.QueryEscape(u), "blocked_address", http.StatusForbidden)
	}
	c.wantError(ctx, "pdf ("+host(PDF)+")", "/v1/fetch?url="+url.QueryEscape(PDF), "unsupported_content", http.StatusUnsupportedMediaType)
	c.wantError(ctx, "404 ("+host(NotFound)+")", "/v1/fetch?url="+url.QueryEscape(NotFound), "upstream_failed", http.StatusBadGateway)
	c.wantError(ctx, "port 8443", "/v1/fetch?url="+url.QueryEscape("https://example.org:8443/"), "bad_request", http.StatusBadRequest)
	if c.failed > 0 {
		return fmt.Errorf("%d of %d checks failed", c.failed, c.total)
	}
	fmt.Fprintf(out, "all %d checks passed\n", c.total)
	return nil
}

type checker struct {
	base, ua      string
	http          *http.Client
	out           io.Writer
	total, failed int
}

func (c *checker) report(name string, err error) {
	c.total++
	if err != nil {
		c.failed++
		fmt.Fprintf(c.out, "FAIL %s: %v\n", name, err)
		return
	}
	fmt.Fprintf(c.out, "ok   %s\n", name)
}

// get requests path and decodes a 200 into v, or returns the error body.
func (c *checker) get(ctx context.Context, path string, v any) (int, *api.ErrorBody, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var eb api.ErrorBody
		if err := json.Unmarshal(body, &eb); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("status %d with a body that is not the error shape: %.200q", resp.StatusCode, body)
		}
		return resp.StatusCode, &eb, nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("unreadable answer: %v", err)
	}
	return resp.StatusCode, nil, nil
}

// getOK is get that treats an error answer as a failure.
func (c *checker) getOK(ctx context.Context, path string, v any) error {
	_, eb, err := c.get(ctx, path, v)
	if err != nil {
		return err
	}
	if eb != nil {
		return fmt.Errorf("%s: %s", eb.Error.Code, eb.Error.Message)
	}
	return nil
}

// health checks /v1/health and returns the endpoints it lists.
func (c *checker) health(ctx context.Context) []string {
	var h api.Health
	err := c.getOK(ctx, "/v1/health", &h)
	if err == nil {
		if len(h.Providers) == 0 {
			err = fmt.Errorf("no providers")
		}
		for name, state := range h.Providers {
			if state != "ok" {
				err = fmt.Errorf("provider %s is %s", name, state)
			}
		}
	}
	c.report(fmt.Sprintf("health (version %s)", h.Version), err)
	return h.Endpoints
}

// context checks §3.5 when health lists it, else its not_available.
func (c *checker) context(ctx context.Context, endpoints []string) {
	path := "/v1/context?q=" + url.QueryEscape(SearchQuery)
	if !slices.Contains(endpoints, "/v1/context") {
		c.wantError(ctx, "context without a provider that has it", path, "not_available", http.StatusNotImplemented)
		return
	}
	var a api.Context
	err := c.getOK(ctx, path, &a)
	if err == nil {
		err = contextBounded(a, api.DefaultTokens)
	}
	if err == nil && len(a.Sources) == 0 {
		err = fmt.Errorf("no sources (engines failed: %v)", a.EnginesFailed)
	}
	c.report(fmt.Sprintf("context %q: %d sources", SearchQuery, len(a.Sources)), err)
}

// contextBounded checks every field of a context answer against §3.5.
func contextBounded(a api.Context, tokens int) error {
	if a.Provider == "" || a.EnginesFailed == nil || a.Sources == nil {
		return fmt.Errorf("provider, sources or engines_failed missing")
	}
	seen := map[string]bool{}
	total, snippets := 0, 0
	for i, s := range a.Sources {
		u, err := url.Parse(s.URL)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https"):
			return fmt.Errorf("source %d: url %q is not http(s)", i, s.URL)
		case seen[s.URL]:
			return fmt.Errorf("source %d: duplicate url %q", i, s.URL)
		case utf8.RuneCountInString(s.Title) > api.MaxTitle:
			return fmt.Errorf("source %d: title longer than %d", i, api.MaxTitle)
		case s.Published != nil && !isoDate.MatchString(*s.Published):
			return fmt.Errorf("source %d: published %q is not an ISO date", i, *s.Published)
		case s.Snippets == nil:
			return fmt.Errorf("source %d: no snippets list", i)
		}
		seen[s.URL] = true
		for _, sn := range s.Snippets {
			total += utf8.RuneCountInString(sn)
			snippets++
		}
	}
	if total > api.CharsPerToken*tokens {
		return fmt.Errorf("%d characters of snippets, more than %d", total, api.CharsPerToken*tokens)
	}
	if len(a.Sources) > 0 && snippets == 0 {
		return fmt.Errorf("sources without a single snippet")
	}
	return nil
}

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (c *checker) search(ctx context.Context) {
	q := "/v1/search?q=" + url.QueryEscape(SearchQuery)
	var s api.Search
	err := c.getOK(ctx, q, &s)
	if err == nil {
		err = bounded(s)
	}
	if err == nil && len(s.Results) < 3 {
		err = fmt.Errorf("%d results, want at least 3 (engines failed: %v)", len(s.Results), s.EnginesFailed)
	}
	c.report(fmt.Sprintf("search %q: %d results", SearchQuery, len(s.Results)), err)

	var wide api.Search
	err = c.getOK(ctx, q+"&n=20", &wide)
	if err == nil {
		err = bounded(wide)
	}
	if err == nil && len(wide.Results) > api.MaxN {
		err = fmt.Errorf("%d results for n=20, want at most %d", len(wide.Results), api.MaxN)
	}
	c.report(fmt.Sprintf("search n=20 clamped to %d: %d results", api.MaxN, len(wide.Results)), err)

	c.wantError(ctx, "search with an empty q", "/v1/search?q=", "bad_request", http.StatusBadRequest)
}

// bounded checks every field of a search answer against §3.2.
func bounded(s api.Search) error {
	if s.Provider == "" || s.EnginesFailed == nil || s.Results == nil {
		return fmt.Errorf("provider, results or engines_failed missing")
	}
	seen := map[string]bool{}
	for i, r := range s.Results {
		u, err := url.Parse(r.URL)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https"):
			return fmt.Errorf("result %d: url %q is not http(s)", i, r.URL)
		case seen[r.URL]:
			return fmt.Errorf("result %d: duplicate url %q", i, r.URL)
		case utf8.RuneCountInString(r.Title) > api.MaxTitle:
			return fmt.Errorf("result %d: title longer than %d", i, api.MaxTitle)
		case utf8.RuneCountInString(r.Snippet) > api.MaxSnippet:
			return fmt.Errorf("result %d: snippet longer than %d", i, api.MaxSnippet)
		case r.Engine == "":
			return fmt.Errorf("result %d: no engine", i)
		case r.Published != nil && !isoDate.MatchString(*r.Published):
			return fmt.Errorf("result %d: published %q is not an ISO date", i, *r.Published)
		}
		seen[r.URL] = true
	}
	return nil
}

func (c *checker) fetchPEP8(ctx context.Context) {
	name := "fetch " + host(PEP8)
	base := "/v1/fetch?url=" + url.QueryEscape(PEP8)
	var first, next, both api.Fetch
	err := c.getOK(ctx, base+"&max_chars=2000", &first)
	if err == nil {
		switch n := utf8.RuneCountInString(first.Text); {
		case n != 2000:
			err = fmt.Errorf("%d characters, want 2000", n)
		case !strings.HasPrefix(first.Text, "#"):
			err = fmt.Errorf("does not start with a heading: %.60q", first.Text)
		case first.Chars <= 2000:
			err = fmt.Errorf("chars %d, want more than 2000", first.Chars)
		case !first.Truncated:
			err = fmt.Errorf("not truncated")
		}
	}
	c.report(fmt.Sprintf("%s: 2000 of %d characters", name, first.Chars), err)

	err = c.getOK(ctx, base+"&start=2000&max_chars=2000", &next)
	if err == nil {
		err = c.getOK(ctx, base+"&max_chars=4000", &both)
	}
	if err == nil && (next.Start != 2000 || next.Text == "" || first.Text+next.Text != both.Text) {
		err = fmt.Errorf("start=2000 does not continue where the first 2000 characters end")
	}
	c.report(name+": start=2000 pages on", err)
}

// wantError checks that path answers code with status.
func (c *checker) wantError(ctx context.Context, name, path, code string, status int) {
	got, eb, err := c.get(ctx, path, &json.RawMessage{})
	switch {
	case err != nil:
	case eb == nil:
		err = fmt.Errorf("status %d, want %d %s", got, status, code)
	case eb.Error.Code != code || got != status:
		err = fmt.Errorf("%d %s (%s), want %d %s", got, eb.Error.Code, eb.Error.Message, status, code)
	}
	c.report(fmt.Sprintf("%s → %s", name, code), err)
}

func host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}
