package searxng

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/provider"
)

const answer = `{
  "query": "zig std.http.Client",
  "number_of_results": 0,
  "results": [
    {"url": "https://ziglang.org/documentation/master/std/#std.http.Client", "title": "std.http.Client", "content": "HTTP client.", "engine": "duckduckgo", "engines": ["duckduckgo", "brave"], "publishedDate": "2026-09-01T00:00:00"},
    {"url": "https://github.com/ziglang/zig", "title": "ziglang/zig", "content": "", "engine": "github", "publishedDate": null},
    {"url": "https://example.org/x", "title": "x", "content": "y", "engine": "brave", "publishedDate": "garbage"}
  ],
  "unresponsive_engines": [["mojeek", "timeout"], ["startpage", "CAPTCHA"]]
}`

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/search" || q.Get("q") != "zig std.http.Client" || q.Get("format") != "json" ||
			q.Get("categories") != "general,it" || q.Get("safesearch") != "0" {
			http.Error(w, "unexpected "+r.URL.String(), http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()
	p, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Search(context.Background(), provider.SearchRequest{Query: "zig std.http.Client", N: 8})
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Result{
		{Title: "std.http.Client", URL: "https://ziglang.org/documentation/master/std/#std.http.Client", Snippet: "HTTP client.", Engine: "duckduckgo", Published: "2026-09-01"},
		{Title: "ziglang/zig", URL: "https://github.com/ziglang/zig", Engine: "github"},
		{Title: "x", URL: "https://example.org/x", Snippet: "y", Engine: "brave"},
	}
	if !reflect.DeepEqual(res.Items, want) {
		t.Errorf("items = %+v", res.Items)
	}
	if !slices.Equal(res.EnginesFailed, []string{"mojeek", "startpage"}) {
		t.Errorf("engines_failed = %v", res.EnginesFailed)
	}
}

func TestSearchNoneFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results": [], "unresponsive_engines": []}`)
	}))
	defer srv.Close()
	p, _ := New(srv.URL + "/")
	res, err := p.Search(context.Background(), provider.SearchRequest{Query: "q", N: 8})
	if err != nil || len(res.Items) != 0 || res.EnginesFailed == nil || len(res.EnginesFailed) != 0 {
		t.Errorf("Search = %+v, %v", res, err)
	}
}

func TestFailures(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/broken/search", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/garbled/search", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>not json</html>")
	})
	mux.HandleFunc("/slow/search", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(release)

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	tests := []struct {
		base    string
		timeout bool
	}{
		{srv.URL + "/broken", false},
		{srv.URL + "/garbled", false},
		{closedURL, false},
		{srv.URL + "/slow", true},
	}
	for _, tt := range tests {
		p, _ := New(tt.base)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := p.Search(ctx, provider.SearchRequest{Query: "q", N: 8})
		cancel()
		_, unavailable := errors.AsType[*provider.UnavailableError](err)
		_, timedOut := errors.AsType[*provider.TimeoutError](err)
		if tt.timeout && !timedOut || !tt.timeout && !unavailable {
			t.Errorf("%s: %T %v", tt.base, err, err)
		}
	}
}

func TestHealth(t *testing.T) {
	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || !healthy {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "OK")
	}))
	defer srv.Close()
	p, _ := New(srv.URL)
	if err := p.Health(context.Background()); err != nil {
		t.Errorf("healthy: %v", err)
	}
	healthy = false
	if err := p.Health(context.Background()); err == nil {
		t.Error("unhealthy reported ok")
	}
}

func TestNew(t *testing.T) {
	for _, base := range []string{"", "127.0.0.1:8888", "ftp://x", "http://"} {
		if _, err := New(base); err == nil {
			t.Errorf("New(%q) accepted", base)
		}
	}
}

// §5.1: freshness pd, pw, pm, py become time_range; a date range is not
// sent.
func TestSearchFreshness(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query().Get("time_range"))
		fmt.Fprint(w, `{"results":[],"unresponsive_engines":[]}`)
	}))
	defer srv.Close()
	p, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pd", "pw", "pm", "py", "2026-01-01to2026-02-01", ""} {
		if _, err := p.Search(context.Background(), provider.SearchRequest{Query: "q", N: 8, Freshness: f}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(got, []string{"day", "week", "month", "year", "", ""}) {
		t.Errorf("time_range %q", got)
	}
}
