// Package fetch is §3.3's fetcher: one public page through the address
// guard, its body capped, its content type checked before the body is read,
// and its text extracted (SPEC.md §3.3, §6, §7).
package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tildaslashalef/holocron/internal/extract"
	"github.com/tildaslashalef/holocron/internal/guard"
)

// The server's limits (§3.3).
const (
	MaxURL       = 2048
	MaxBody      = 5 << 20
	MaxRedirects = 5
	Budget       = 20 * time.Second
)

// UnsupportedError is a response whose type §3.3 does not extract
// (unsupported_content).
type UnsupportedError struct{ Msg string }

func (e *UnsupportedError) Error() string { return e.Msg }

// UpstreamError is a remote failure: DNS, connection, TLS, a 4xx/5xx
// status, too many redirects (upstream_failed).
type UpstreamError struct{ Msg string }

func (e *UpstreamError) Error() string { return e.Msg }

// TimeoutError is the budget running out (upstream_timeout).
type TimeoutError struct{ Msg string }

func (e *TimeoutError) Error() string { return e.Msg }

// PanicError is a panic recovered during extraction: a bug (internal).
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic during extraction: %v", e.Value) }

// Page is one fetched and extracted document.
type Page struct {
	URL         string
	FinalURL    string
	Status      int
	ContentType string // the media type alone, e.g. "text/html"
	Title       string
	Text        string
	Truncated   bool // the raw body was cut at MaxBody
}

// kinds maps each extracted media type to whether it is HTML.
var kinds = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
	"text/plain":            false,
	"text/markdown":         false,
	"application/json":      false,
	"text/xml":              false,
	"application/xml":       false,
}

// Client fetches pages. It is safe for concurrent use.
type Client struct {
	http      *http.Client
	userAgent string

	// extractHTML is extract.HTML; tests slow it down.
	extractHTML func([]byte, string, *url.URL) extract.Result
}

// New returns a Client that connects through dial (a guard.Dialer's
// DialContext in production) and names itself userAgent.
func New(dial func(ctx context.Context, network, addr string) (net.Conn, error), userAgent string) *Client {
	transport := &http.Transport{
		Proxy:                  nil, // never from the environment
		DialContext:            dial,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		MaxIdleConns:           32,
		IdleConnTimeout:        60 * time.Second,
		MaxResponseHeaderBytes: 1 << 20,
	}
	c := &Client{userAgent: userAgent, extractHTML: extract.HTML}
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > MaxRedirects {
				return &UpstreamError{fmt.Sprintf("more than %d redirects", MaxRedirects)}
			}
			return guard.CheckURL(req.URL)
		},
	}
	return c
}

// Guarded is New with the address guard's dialer.
func Guarded(userAgent string) *Client {
	return New((&guard.Dialer{}).DialContext, userAgent)
}

// Parse checks a requested URL: its length, its form, then the guard's
// step 1. Its errors are guard errors.
func Parse(raw string) (*url.URL, error) {
	if len(raw) > MaxURL {
		return nil, &guard.BadURLError{Msg: fmt.Sprintf("the URL is longer than %d bytes", MaxURL)}
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return nil, &guard.BadURLError{Msg: fmt.Sprintf("%q is not an absolute URL", raw)}
	}
	if err := guard.CheckURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// Fetch fetches raw and extracts its text under ctx, whose deadline is the
// whole budget. When the deadline passes during extraction, Fetch returns a
// TimeoutError and the extraction runs on; done, if not nil, is called
// once all of Fetch's work has finished, after it returned or before.
func (c *Client) Fetch(ctx context.Context, raw string, done func()) (*Page, error) {
	finish := func() {
		if done != nil {
			done()
		}
	}
	u, err := Parse(raw)
	if err != nil {
		finish()
		return nil, err
	}
	page, body, contentType, err := c.get(ctx, u)
	if err != nil {
		finish()
		return nil, err
	}
	page.URL = raw

	type outcome struct {
		res extract.Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		// This goroutine outlives a timed-out request, beyond the reach of
		// the handler's recover: a panic here must not end the process.
		defer finish()
		defer func() {
			if v := recover(); v != nil {
				ch <- outcome{err: &PanicError{Value: v, Stack: debug.Stack()}}
			}
		}()
		if kinds[page.ContentType] {
			final, _ := url.Parse(page.FinalURL)
			ch <- outcome{res: c.extractHTML(body, contentType, final)}
		} else {
			ch <- outcome{res: extract.Plain(body, contentType)}
		}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			return nil, o.err
		}
		page.Title, page.Text = o.res.Title, o.res.Text
		return page, nil
	case <-ctx.Done():
		return nil, timeout(u.Hostname(), ctx.Err())
	}
}

// get does the request and reads the body, and returns the page so far,
// the body, and the Content-Type to decode it with.
func (c *Client) get(ctx context.Context, u *url.URL) (*Page, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, "", &guard.BadURLError{Msg: err.Error()}
	}
	req.Header.Set("User-Agent", c.userAgent)
	// A browser's Accept: a site that answers text/markdown with its raw
	// source (react.dev sends MDX, components and all) sends HTML instead,
	// which §7.2 measured.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, "", classify(u.Hostname(), ctx, err)
	}
	defer resp.Body.Close()
	host := resp.Request.URL.Hostname()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := fmt.Sprintf("%s answered %s", host, resp.Status)
		if challenged(resp) {
			// A bot check that wants a browser to run its JavaScript; a
			// browser's User-Agent does not pass it either (measured
			// 2026-10-16), and holocron does not try to get past it.
			msg += ": a bot challenge (Cloudflare) that only a browser passes; try /v1/context, whose provider has the page's text"
		}
		return nil, nil, "", &UpstreamError{msg}
	}

	page := &Page{FinalURL: resp.Request.URL.String(), Status: resp.StatusCode}
	// The declared type decides before the body is read; an absent or
	// unparsable one is sniffed after.
	header := resp.Header.Get("Content-Type")
	media, _, err := mime.ParseMediaType(header)
	if err != nil {
		media = ""
	}
	if media != "" {
		media = strings.ToLower(media)
		if _, ok := kinds[media]; !ok {
			return nil, nil, "", &UnsupportedError{fmt.Sprintf("%s is %s, which holocron does not extract", host, media)}
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, nil, "", classify(host, ctx, err)
	}
	if len(body) > MaxBody {
		body, page.Truncated = body[:MaxBody], true
	}
	if media == "" {
		// Sniffed: decode by the document's own <meta>, not the sniffer's
		// charset guess.
		sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(body))
		if _, ok := kinds[sniffed]; !ok {
			return nil, nil, "", &UnsupportedError{fmt.Sprintf("%s sent no usable Content-Type and looks like %s, which holocron does not extract", host, sniffed)}
		}
		media, header = sniffed, sniffed
	}
	page.ContentType = media
	return page, body, header, nil
}

// challenged reports a Cloudflare bot challenge: cf-mitigated: challenge.
func challenged(resp *http.Response) bool {
	return strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge")
}

// classify maps a transport error to the contract's kinds.
func classify(host string, ctx context.Context, err error) error {
	if b, ok := errors.AsType[*guard.BlockedError](err); ok {
		return b
	}
	if b, ok := errors.AsType[*guard.BadURLError](err); ok {
		return b
	}
	if u, ok := errors.AsType[*UpstreamError](err); ok {
		return u
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return timeout(host, ctx.Err())
	}
	if n, ok := errors.AsType[net.Error](err); ok && n.Timeout() {
		return timeout(host, err)
	}
	if d, ok := errors.AsType[*net.DNSError](err); ok {
		return &UpstreamError{fmt.Sprintf("%s: DNS: %s", host, d.Err)}
	}
	if o, ok := errors.AsType[*net.OpError](err); ok {
		return &UpstreamError{fmt.Sprintf("%s: %s", host, o.Err)}
	}
	return &UpstreamError{fmt.Sprintf("%s: %v", host, unwrapURL(err))}
}

func timeout(host string, err error) error {
	if errors.Is(err, context.Canceled) {
		return &TimeoutError{fmt.Sprintf("%s: the request was cancelled", host)}
	}
	return &TimeoutError{fmt.Sprintf("%s did not answer within the %s budget", host, Budget)}
}

// unwrapURL drops net/http's "Get \"…\": " prefix; the host is named
// already.
func unwrapURL(err error) error {
	if u, ok := errors.AsType[*url.Error](err); ok {
		return u.Err
	}
	return err
}
