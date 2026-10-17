package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/extract"
	"github.com/tildaslashalef/holocron/internal/guard"
	"github.com/tildaslashalef/holocron/internal/guard/guardtest"
)

const article = `<!doctype html><html><head><title>The page</title></head><body><article>
<h1>Heading</h1>
<p>A first paragraph with enough words in it to count as the content of this page, and a <a href="next/">relative link</a>.</p>
<p>A second paragraph, also long enough that the extractor keeps it as part of the main content here.</p>
<p>A third paragraph closes the article with a few more words about nothing in particular at all.</p>
</article></body></html>`

// newClient returns a Client whose site.test (any port) is srv, and whose
// other names go through the guard with zone as their DNS.
func newClient(t *testing.T, srv *httptest.Server, zone map[string][]string) *Client {
	t.Helper()
	target := strings.TrimPrefix(srv.URL, "http://")
	guarded := &guard.Dialer{Resolver: guardtest.Resolver(zone)}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(addr); host == "site.test" {
			var d net.Dialer
			return d.DialContext(ctx, network, target)
		}
		return guarded.DialContext(ctx, network, addr)
	}
	return New(dial, "holocron-test/1")
}

func fetch(t *testing.T, c *Client, raw string, budget time.Duration) (*Page, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return c.Fetch(ctx, raw, nil)
}

func TestFetchHTML(t *testing.T) {
	var ua atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/page/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/docs/page/", func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, article)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := fetch(t, newClient(t, srv, nil), "http://site.test/start", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "http://site.test/start" || p.FinalURL != "http://site.test/docs/page/" || p.Status != 200 ||
		p.ContentType != "text/html" || p.Title != "Heading" || p.Truncated {
		t.Errorf("page = %+v", p)
	}
	if !strings.Contains(p.Text, "[relative link](http://site.test/docs/page/next/)") {
		t.Errorf("link not resolved against the final URL:\n%s", p.Text)
	}
	if ua.Load() != "holocron-test/1" {
		t.Errorf("User-Agent = %v", ua.Load())
	}
}

func TestFetchRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(r.URL.Path, "/hop/%d", &n)
		if n == 0 {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "arrived")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	mux.HandleFunc("/to", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("url"), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient(t, srv, map[string][]string{"private.test": {"192.168.1.1"}, "public.test": {"93.184.216.34"}})

	if p, err := fetch(t, c, "http://site.test/hop/5", 5*time.Second); err != nil || p.Text != "arrived" {
		t.Errorf("5 redirects: %+v, %v", p, err)
	}
	_, err := fetch(t, c, "http://site.test/hop/6", 5*time.Second)
	if u, ok := errors.AsType[*UpstreamError](err); !ok || !strings.Contains(u.Msg, "more than 5 redirects") {
		t.Errorf("6 redirects: %v", err)
	}

	tests := []struct {
		to   string
		want string // "blocked" or "bad"
	}{
		{"http://192.168.1.1/", "blocked"},
		{"http://[::1]/", "blocked"},
		{"http://nas.home.arpa/", "blocked"},
		{"http://localhost/", "blocked"},
		{"http://private.test/", "blocked"}, // refused at connect time
		{"ftp://example.org/", "bad"},
		{"http://example.org:8443/", "bad"},
		{"http://user:pw@example.org/", "bad"},
	}
	for _, tt := range tests {
		_, err := fetch(t, c, "http://site.test/to?url="+url.QueryEscape(tt.to), 5*time.Second)
		_, blocked := errors.AsType[*guard.BlockedError](err)
		_, bad := errors.AsType[*guard.BadURLError](err)
		if (tt.want == "blocked" && !blocked) || (tt.want == "bad" && !bad) {
			t.Errorf("redirect to %s: %T %v, want %s", tt.to, err, err, tt.want)
		}
	}
}

func TestFetchBodyCap(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxBody+1<<20)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(bytes.Repeat([]byte("b"), 20<<20))
	zw.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(big)
	})
	mux.HandleFunc("/bomb", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz.Bytes())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient(t, srv, nil)
	for _, path := range []string{"/big", "/bomb"} {
		p, err := fetch(t, c, "http://site.test"+path, 10*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !p.Truncated || len(p.Text) != MaxBody {
			t.Errorf("%s: truncated %v, %d bytes", path, p.Truncated, len(p.Text))
		}
	}
}

func TestFetchContentTypes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mux := http.NewServeMux()
	mux.HandleFunc("/pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select { // the body never comes: holocron must decide from the headers
		case <-release:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/sniff-html", func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		fmt.Fprint(w, article)
	})
	mux.HandleFunc("/sniff-pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		w.Write([]byte("%PDF-1.7\n\x00\x01\x02 binary"))
	})
	mux.HandleFunc("/latin1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-1")
		w.Write([]byte("caf\xe9"))
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Application/JSON")
		fmt.Fprint(w, `{"a": 1}`)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient(t, srv, nil)

	start := time.Now()
	_, err := fetch(t, c, "http://site.test/pdf", 5*time.Second)
	if _, ok := errors.AsType[*UnsupportedError](err); !ok || time.Since(start) > 2*time.Second {
		t.Errorf("pdf: %v after %v", err, time.Since(start))
	}
	if p, err := fetch(t, c, "http://site.test/sniff-html", 5*time.Second); err != nil || p.ContentType != "text/html" || p.Title != "Heading" {
		t.Errorf("sniffed html: %+v, %v", p, err)
	}
	if _, err := fetch(t, c, "http://site.test/sniff-pdf", 5*time.Second); err == nil {
		t.Error("sniffed pdf accepted")
	} else if _, ok := errors.AsType[*UnsupportedError](err); !ok {
		t.Errorf("sniffed pdf: %v", err)
	}
	if p, err := fetch(t, c, "http://site.test/latin1", 5*time.Second); err != nil || p.Text != "café" || p.Title != "" {
		t.Errorf("latin1: %+v, %v", p, err)
	}
	if p, err := fetch(t, c, "http://site.test/json", 5*time.Second); err != nil || p.ContentType != "application/json" || p.Text != `{"a": 1}` {
		t.Errorf("json: %+v, %v", p, err)
	}
	_, err = fetch(t, c, "http://site.test/missing", 5*time.Second)
	if u, ok := errors.AsType[*UpstreamError](err); !ok || !strings.Contains(u.Msg, "404 Not Found") {
		t.Errorf("404: %v", err)
	}
}

func TestFetchFailures(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := newClient(t, srv, map[string][]string{"private.test": {"10.0.0.1"}})

	start := time.Now()
	_, err := fetch(t, c, "http://site.test/slow", 200*time.Millisecond)
	if _, ok := errors.AsType[*TimeoutError](err); !ok || time.Since(start) > 2*time.Second {
		t.Errorf("slow: %T %v after %v", err, err, time.Since(start))
	}
	if _, err := fetch(t, c, "http://nowhere.test/", 5*time.Second); err == nil {
		t.Error("unknown name fetched")
	} else if u, ok := errors.AsType[*UpstreamError](err); !ok || !strings.Contains(u.Msg, "DNS") {
		t.Errorf("unknown name: %T %v", err, err)
	}
	_, err = fetch(t, c, "http://private.test/", 5*time.Second)
	if b, ok := errors.AsType[*guard.BlockedError](err); !ok || b.Msg != "private.test resolves to 10.0.0.1, not a public address" {
		t.Errorf("private: %v", err)
	}
	for _, raw := range []string{"http://example.org/" + strings.Repeat("a", MaxURL), "/relative", "example.org", "http://[::1"} {
		if _, err := fetch(t, c, raw, time.Second); err == nil {
			t.Errorf("%.40q fetched", raw)
		} else if _, ok := errors.AsType[*guard.BadURLError](err); !ok {
			t.Errorf("%.40q: %T %v", raw, err, err)
		}
	}
}

// An extraction past the deadline answers a timeout and runs on; done
// comes only when it ends.
func TestFetchExtractionDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, article)
	}))
	defer srv.Close()
	c := newClient(t, srv, nil)
	finished := make(chan struct{})
	c.extractHTML = func(b []byte, ct string, u *url.URL) extract.Result {
		time.Sleep(300 * time.Millisecond)
		close(finished)
		return extract.HTML(b, ct, u)
	}
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Fetch(ctx, "http://site.test/", func() { close(done) })
	if _, ok := errors.AsType[*TimeoutError](err); !ok {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-done:
		t.Fatal("done before the extraction finished")
	default:
	}
	<-finished
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("done never called")
	}
}

func TestFetchDoneOnError(t *testing.T) {
	c := New(nil, "x")
	called := 0
	if _, err := c.Fetch(context.Background(), "ftp://example.org/", func() { called++ }); err == nil {
		t.Fatal("fetched")
	}
	if called != 1 {
		t.Errorf("done called %d times", called)
	}
}

func TestFetchExtractionPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, article)
	}))
	defer srv.Close()
	c := newClient(t, srv, nil)
	c.extractHTML = func([]byte, string, *url.URL) extract.Result { panic("boom") }
	done := make(chan struct{})
	_, err := c.Fetch(context.Background(), "http://site.test/", func() { close(done) })
	if p, ok := errors.AsType[*PanicError](err); !ok || p.Value != "boom" || len(p.Stack) == 0 {
		t.Errorf("err = %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("done never called")
	}
}

// A Cloudflare challenge is named as one; another 403 is not.
func TestFetchChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/challenged" {
			w.Header().Set("Cf-Mitigated", "challenge")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := newClient(t, srv, nil)
	_, err := fetch(t, c, "http://site.test/challenged", 5*time.Second)
	if u, ok := errors.AsType[*UpstreamError](err); !ok ||
		u.Msg != "site.test answered 403 Forbidden: a bot challenge (Cloudflare) that only a browser passes; try /v1/context, whose provider has the page's text" {
		t.Errorf("challenge: %v", err)
	}
	_, err = fetch(t, c, "http://site.test/plain", 5*time.Second)
	if u, ok := errors.AsType[*UpstreamError](err); !ok || u.Msg != "site.test answered 403 Forbidden" {
		t.Errorf("plain 403: %v", err)
	}
}
