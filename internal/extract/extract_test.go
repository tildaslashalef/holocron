package extract

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// filler is enough ordinary prose for go-trafilatura to accept a page.
var filler = func() string {
	var b strings.Builder
	for i := range 6 {
		fmt.Fprintf(&b, "<p>Paragraph %d has its own sentence about topic %d, long enough to count as content.</p>\n", i, i*7)
	}
	return b.String()
}()

func page(title, body string) []byte {
	return []byte("<!doctype html><html><head><title>" + title + "</title></head><body><nav><a href=\"/\">Home</a></nav><article>" +
		filler + body + filler + "</article><footer>Footer text</footer></body></html>")
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestHTMLMarkdown(t *testing.T) {
	body := `<h2 id="usage">Usage</h2>
<p>Read the <a href="#usage">usage</a>, the <a href="../other/">other page</a> and <a href="https://example.com/x">x</a>.</p>
<ul><li>first item</li><li>second item</li></ul>
<pre><code class="language-go">func main() {
	fmt.Println("hi")
}</code></pre>
<table><tr><th>Name</th><th>Value</th></tr><tr><td>a</td><td>1</td></tr></table>`
	res := HTML(page("A page", body), "text/html; charset=utf-8", mustURL(t, "https://example.org/docs/page/"))
	for _, want := range []string{
		"## Usage",
		"[usage](https://example.org/docs/page/#usage)",
		"[other page](https://example.org/docs/other/)",
		"[x](https://example.com/x)",
		"- first item",
		"```\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```",
		"| Name | Value |",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "<") {
		t.Errorf("text has HTML:\n%s", res.Text)
	}
	if res.Title != "A page" {
		t.Errorf("title = %q", res.Title)
	}
}

func TestHTMLCodeBlocks(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		// go-trafilatura marks <pre><code> as a bare <code>.
		{"pre code", "<pre><code>a := 1\nb := 2</code></pre>", "```\na := 1\nb := 2\n```"},
		{"pre", "<pre>line one\nline two</pre>", "```\nline one\nline two\n```"},
		{"code with br", "<p>x</p><code>line one<br>line two</code>", "```\nline one\nline two\n```"},
		{"inline", "<p>Call <code>f()</code> twice.</p>", "Call `f()` twice."},
	}
	for _, tt := range tests {
		res := HTML(page("t", tt.body), "text/html", nil)
		if !strings.Contains(res.Text, tt.want) {
			t.Errorf("%s: text lacks %q:\n%s", tt.name, tt.want, res.Text)
		}
	}
}

// Elements nested in inline code must stay in place: go-trafilatura moves
// them next to the paragraph's following element on pages like nginx's
// module docs.
func TestHTMLNestedInlineCode(t *testing.T) {
	directive := `<a name="d"></a><div class="directive"><table cellspacing="0"><tr><th>Syntax:</th><td><code><strong>proxy_buffer_size</strong> <code><i>size</i></code>;</code></td></tr><tr><th>Default:</th><td><pre>proxy_buffer_size 4k|8k;</pre></td></tr></table></div>`
	para := `<p>Sets the <code><i>size</i></code> of the buffer used for reading the first part of the response, if it is <a href="#x">invalid</a>. By default, the buffer size is one memory page.</p>`
	doc := "<html><body><div id=\"content\"><h1>Module</h1>" + strings.Repeat(directive+para, 6) + "</div></body></html>"
	res := HTML([]byte(doc), "text/html", mustURL(t, "https://example.org/m.html"))
	if !strings.Contains(res.Text, "Sets the `size` of the buffer") {
		t.Errorf("inline code moved:\n%s", res.Text)
	}
}

func TestHTMLCommentsKept(t *testing.T) {
	body := `<section id="comments"><h2>Comments</h2><p>Comments that contradict the code are worse than no comments, so keep them current.</p></section>`
	res := HTML(page("t", body), "text/html", nil)
	if !strings.Contains(res.Text, "Comments that contradict the code") {
		t.Errorf("comments section dropped:\n%s", res.Text)
	}
}

// A short page keeps its lists and tables: go-trafilatura alone would
// replace it with its paragraph-only baseline.
func TestHTMLShortPage(t *testing.T) {
	doc := `<html><body><article><p>Read the thing here please.</p><ul><li>first item</li><li>second item</li></ul>
<table><tr><th>Name</th><th>Value</th></tr><tr><td>a</td><td>1</td></tr></table></article></body></html>`
	res := HTML([]byte(doc), "text/html", nil)
	for _, want := range []string{"Read the thing", "- first item", "| Name | Value |"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, res.Text)
		}
	}
}

func TestHTMLEmpty(t *testing.T) {
	res := HTML([]byte("<html><head><title>Nothing</title></head><body><script>x()</script></body></html>"), "text/html", nil)
	if res.Text != "" || res.Title != "Nothing" {
		t.Errorf("HTML = %+v", res)
	}
}

func TestVisibleText(t *testing.T) {
	doc := `<html><head><title>t</title><style>p{}</style></head><body><script>var x = 1;</script>
<div>Hello <b>there</b></div><noscript>enable js</noscript><template><p>t</p></template><svg><text>s</text></svg><p>second</p></body></html>`
	root, err := html.ParseWithOptions(strings.NewReader(doc), html.ParseOptionEnableScripting(false))
	if err != nil {
		t.Fatal(err)
	}
	if got := visibleText(root); got != "Hello there\nsecond" {
		t.Errorf("visibleText = %q", got)
	}
	if got := titleOf(root); got != "t" {
		t.Errorf("titleOf = %q", got)
	}
}

func TestHTMLBase(t *testing.T) {
	body := `<p>For a gentler introduction to the module and its address types, see <a href="../howto/ipaddress.html#howto">the howto</a>, which walks through creating addresses, networks and interfaces.</p>`
	res := HTML(page("t", body), "text/html", mustURL(t, "https://docs.python.org/3/library/ipaddress.html"))
	if !strings.Contains(res.Text, "[the howto](https://docs.python.org/3/howto/ipaddress.html#howto)") {
		t.Errorf("relative link:\n%s", res.Text)
	}
	doc := `<html><head><base href="https://cdn.example.net/root/"></head><body><article>` + filler +
		`<p>The documentation that goes with this page lives under a <a href="sub/page">based link</a>, resolved against the base element of the document.</p>` + filler + `</article></body></html>`
	res = HTML([]byte(doc), "text/html", mustURL(t, "https://example.org/a/b"))
	if !strings.Contains(res.Text, "[based link](https://cdn.example.net/root/sub/page)") {
		t.Errorf("<base> link:\n%s", res.Text)
	}
}

func TestHTMLTitleClipped(t *testing.T) {
	res := HTML(page(strings.Repeat("word ", 60), ""), "text/html", nil)
	if n := utf8.RuneCountInString(res.Title); n > MaxTitle || !strings.HasSuffix(res.Title, "word…") {
		t.Errorf("title (%d) = %q", n, res.Title)
	}
}

func TestDecode(t *testing.T) {
	latin1 := []byte("caf\xe9")
	tests := []struct {
		name        string
		body        []byte
		contentType string
		html        bool
		want        string
	}{
		{"utf-8 default", []byte("café"), "", false, "café"},
		{"header", latin1, "text/plain; charset=iso-8859-1", false, "café"},
		{"bom wins", append([]byte{0xef, 0xbb, 0xbf}, "café"...), "text/plain; charset=iso-8859-1", false, "café"},
		{"meta", append([]byte(`<meta charset="windows-1252">`), latin1...), "text/html", true, `<meta charset="windows-1252">café`},
		{"http-equiv", append([]byte(`<meta http-equiv="Content-Type" content="text/html; charset=iso-8859-1">`), latin1...), "text/html", true, `<meta http-equiv="Content-Type" content="text/html; charset=iso-8859-1">café`},
		{"meta ignored for plain", append([]byte(`<meta charset="windows-1252">`), latin1...), "text/plain", false, "<meta charset=\"windows-1252\">caf\uFFFD"},
		{"unknown charset", latin1, "text/plain; charset=bogus", false, "caf\uFFFD"},
		{"broken utf-8", []byte("a\xffb"), "text/plain; charset=utf-8", false, "a\uFFFDb"},
		{"utf-16 meta is utf-8", []byte(`<meta charset="utf-16">é`), "text/html", true, `<meta charset="utf-16">é`},
	}
	for _, tt := range tests {
		if got := decode(tt.body, tt.contentType, tt.html); got != tt.want {
			t.Errorf("%s: decode = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestPlain(t *testing.T) {
	res := Plain([]byte("{\"a\": 1}\n"), "application/json")
	if res.Text != "{\"a\": 1}\n" || res.Title != "" {
		t.Errorf("Plain = %+v", res)
	}
}

func TestClip(t *testing.T) {
	tests := []struct {
		s    string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"one two three four", 10, "one two…"},
		{"unbrokenlongword", 6, "unbro…"},
		{"héllo wörld again", 12, "héllo wörld…"},
	}
	for _, tt := range tests {
		if got := Clip(tt.s, tt.max); got != tt.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
	}
}
