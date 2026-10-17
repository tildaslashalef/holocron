package check

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tildaslashalef/holocron/internal/api"
	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/guard"
	"github.com/tildaslashalef/holocron/internal/provider"
)

type fakeProvider struct{ n int }

func (fakeProvider) Name() string { return "fake" }
func (p fakeProvider) Search(ctx context.Context, r provider.SearchRequest) (provider.Results, error) {
	var res provider.Results
	for i := range p.n {
		res.Items = append(res.Items, provider.Result{Title: fmt.Sprintf("r%d", i), URL: fmt.Sprintf("https://example.org/%d", i), Engine: "e"})
	}
	return res, nil
}
func (fakeProvider) Health(ctx context.Context) error { return nil }

// contextProvider is fakeProvider with context.
type contextProvider struct{ fakeProvider }

func (contextProvider) Context(ctx context.Context, r provider.ContextRequest) (provider.Context, error) {
	return provider.Context{Sources: []provider.Source{
		{URL: "https://ziglang.org/a", Title: "Zig", Published: "2026-09-01", Snippets: []string{"std.http.Client", "proxy"}},
		{URL: "https://example.org/b", Title: "B", Snippets: []string{"more"}},
	}}, nil
}

// fakeFetcher answers the suite's public URLs as a working holocron would.
type fakeFetcher struct{}

func (fakeFetcher) Fetch(ctx context.Context, raw string, done func()) (*fetch.Page, error) {
	defer done()
	switch raw {
	case PEP8:
		return &fetch.Page{URL: raw, FinalURL: raw, Status: 200, ContentType: "text/html",
			Text: "# PEP 8\n\n" + strings.Repeat("Style guide prose. ", 400)}, nil
	case "http://192.168.1.1.nip.io/":
		return nil, &guard.BlockedError{Msg: "192.168.1.1.nip.io resolves to 192.168.1.1, not a public address"}
	case RedirectSite:
		return nil, &guard.BlockedError{Msg: "192.168.1.1 is not a public address"}
	case PDF:
		return nil, &fetch.UnsupportedError{Msg: "pdf"}
	case NotFound:
		return nil, &fetch.UpstreamError{Msg: "go.dev answered 404 Not Found"}
	}
	return nil, fmt.Errorf("unexpected fetch of %s", raw)
}

func server(p provider.Provider) *httptest.Server {
	return httptest.NewServer(api.NewServer("t", p, fakeFetcher{}, slog.New(slog.DiscardHandler), true))
}

func TestPasses(t *testing.T) {
	srv := server(fakeProvider{n: 12})
	defer srv.Close()
	var out bytes.Buffer
	if err := Run(context.Background(), srv.URL+"/", "check-test", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "all 15 checks passed") ||
		!strings.Contains(out.String(), "ok   context without a provider that has it → not_available") {
		t.Errorf("output:\n%s", out.String())
	}

	// With context.
	srv = server(contextProvider{fakeProvider{n: 12}})
	defer srv.Close()
	out.Reset()
	if err := Run(context.Background(), srv.URL, "check-test", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `ok   context "zig std.http.Client": 2 sources`) || !strings.Contains(out.String(), "all 15 checks passed") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestFailures(t *testing.T) {
	srv := server(fakeProvider{n: 2})
	defer srv.Close()
	var out bytes.Buffer
	err := Run(context.Background(), srv.URL, "check-test", &out)
	if err == nil || err.Error() != "1 of 15 checks failed" {
		t.Errorf("err = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "FAIL search \"zig std.http.Client\": 2 results: 2 results, want at least 3") {
		t.Errorf("output:\n%s", out.String())
	}

	// Nothing listening: every check fails, none panics.
	srv.Close()
	out.Reset()
	if err := Run(context.Background(), srv.URL, "check-test", &out); err == nil || !strings.HasPrefix(err.Error(), "15 of 15") {
		t.Errorf("err = %v", err)
	}
}

func TestContextBounded(t *testing.T) {
	bad := "yesterday"
	ok := []api.Source{{URL: "https://x/", Snippets: []string{"s"}}}
	if err := contextBounded(api.Context{Provider: "p", EnginesFailed: []string{}, Sources: ok}, 1024); err != nil {
		t.Errorf("ok: %v", err)
	}
	for _, a := range []api.Context{
		{Provider: "p", EnginesFailed: []string{}},
		{Provider: "p", EnginesFailed: []string{}, Sources: []api.Source{{URL: "ftp://x/", Snippets: []string{}}}},
		{Provider: "p", EnginesFailed: []string{}, Sources: []api.Source{ok[0], ok[0]}},
		{Provider: "p", EnginesFailed: []string{}, Sources: []api.Source{{URL: "https://x/", Snippets: []string{"s"}, Published: &bad}}},
		{Provider: "p", EnginesFailed: []string{}, Sources: []api.Source{{URL: "https://x/", Snippets: []string{}}}},
		{Provider: "p", EnginesFailed: []string{}, Sources: []api.Source{{URL: "https://x/", Snippets: []string{strings.Repeat("a", 8*1024+1)}}}},
	} {
		if contextBounded(a, 1024) == nil {
			t.Errorf("contextBounded(%+v) = nil", a)
		}
	}
}

func TestBounded(t *testing.T) {
	bad := "2026/01/01"
	for _, s := range []api.Search{
		{Provider: "p", EnginesFailed: []string{}, Results: []api.Result{{URL: "ftp://x/", Engine: "e"}}},
		{Provider: "p", EnginesFailed: []string{}, Results: []api.Result{{URL: "https://x/", Engine: "e"}, {URL: "https://x/", Engine: "e"}}},
		{Provider: "p", EnginesFailed: []string{}, Results: []api.Result{{URL: "https://x/", Engine: "e", Title: strings.Repeat("t", 201)}}},
		{Provider: "p", EnginesFailed: []string{}, Results: []api.Result{{URL: "https://x/", Engine: "e", Published: &bad}}},
		{Provider: "p", EnginesFailed: []string{}, Results: []api.Result{{URL: "https://x/"}}},
		{Provider: "p", Results: []api.Result{}},
	} {
		if bounded(s) == nil {
			t.Errorf("bounded(%+v) = nil", s)
		}
	}
}

var _ http.Handler = (*api.Server)(nil)
