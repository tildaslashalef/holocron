// Package extract turns a fetched body into the text §7.1 of SPEC.md
// returns: decoded to UTF-8, and for HTML the main content as Markdown.
package extract

import (
	"bytes"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	trafilatura "github.com/markusmobius/go-trafilatura/v2"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// MaxTitle bounds Result.Title, in characters.
const MaxTitle = 200

// Result is one document's extracted text.
type Result struct {
	Title string
	Text  string
}

// Plain decodes a plain-text body (§3.3's text types); the title is empty.
func Plain(body []byte, contentType string) Result {
	return Result{Text: decode(body, contentType, false)}
}

// HTML extracts the main content of an HTML body as Markdown, with links
// resolved against pageURL (the final URL, path included). It never fails:
// when the main-content pass yields nothing, the text is the body's visible
// text, one block per line.
func HTML(body []byte, contentType string, pageURL *url.URL) Result {
	root, err := html.ParseWithOptions(strings.NewReader(decode(body, contentType, true)),
		html.ParseOptionEnableScripting(false))
	if err != nil {
		return Result{}
	}
	prepare(root, pageURL)
	opts := trafilatura.Options{
		OriginalURL:    pageURL,
		EnableFallback: true,
		Focus:          trafilatura.FavorRecall,
		IncludeLinks:   true,
		HtmlDateMode:   trafilatura.Disabled,
	}
	tr, _ := trafilatura.ExtractDocument(root, opts)
	// Under MinExtractedSize, go-trafilatura (a port of Python's 2.2.0)
	// replaces the result with its paragraph-only baseline; Python 2.3
	// keeps the baseline only when it is longer (trafilatura#896). Redo
	// such a page without the baseline and keep the longer text.
	if minSize := trafilatura.DefaultConfig().MinExtractedSize; tr == nil || utf8.RuneCountInString(tr.ContentText) < minSize {
		cfg := trafilatura.DefaultConfig()
		cfg.MinExtractedSize = 1
		opts.Config = cfg
		if alt, _ := trafilatura.ExtractDocument(root, opts); alt != nil && (tr == nil || len(alt.ContentText) >= len(tr.ContentText)) {
			tr = alt
		}
	}
	var res Result
	if tr != nil {
		res.Title = tr.Metadata.Title
		// Comments are kept and appended, as Python trafilatura does: what
		// looks like a comment thread is often content (PEP 8's
		// <section id="comments">).
		var parts []string
		for _, n := range []*html.Node{tr.ContentNode, tr.CommentsNode} {
			if md := markdown(n, pageURL); md != "" {
				parts = append(parts, md)
			}
		}
		res.Text = strings.Join(parts, "\n\n")
	}
	if res.Text == "" {
		res.Text = visibleText(root)
	}
	if res.Title == "" {
		res.Title = titleOf(root)
	}
	res.Title = Clip(collapse(res.Title), MaxTitle)
	return res
}

// prepare fixes up a parsed page before go-trafilatura sees it:
//   - every <a href> is resolved against the page's base (its <base href>,
//     else pageURL): go-trafilatura's own resolution joins paths as
//     directories, so ../howto/x.html from /3/library/ipaddress.html
//     lands under /3/library/;
//   - soft hyphens go, as trafilatura.Extract drops them;
//   - each inline <code> outside <pre> becomes plain text: go-trafilatura,
//     like Python trafilatura, can move an element nested in inline code
//     (nginx's <code><i>size</i></code>) out of its place in the paragraph.
func prepare(root *html.Node, pageURL *url.URL) {
	base := pageURL
	if pageURL != nil {
		if href, ok := baseHref(root); ok {
			if u, err := pageURL.Parse(href); err == nil {
				base = u
			}
		}
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			n.Data = strings.ReplaceAll(n.Data, "\u00ad", "")
			return
		case n.Type != html.ElementNode:
		case n.DataAtom == atom.A && base != nil:
			for i, a := range n.Attr {
				if a.Namespace == "" && a.Key == "href" {
					if u, err := base.Parse(strings.TrimSpace(a.Val)); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
						n.Attr[i].Val = u.String()
					}
				}
			}
		case n.DataAtom == atom.Pre:
			return
		case n.DataAtom == atom.Code && n.FirstChild != nil:
			text := codeText(n)
			for n.FirstChild != nil {
				n.RemoveChild(n.FirstChild)
			}
			n.AppendChild(&html.Node{Type: html.TextNode, Data: text})
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
}

// baseHref returns the href of the document's first <base href>.
func baseHref(n *html.Node) (string, bool) {
	if n.Type == html.ElementNode && n.DataAtom == atom.Base {
		for _, a := range n.Attr {
			if a.Namespace == "" && a.Key == "href" {
				return strings.TrimSpace(a.Val), true
			}
		}
	}
	if n.Type == html.ElementNode && n.DataAtom == atom.Body {
		return "", false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if href, ok := baseHref(c); ok {
			return href, true
		}
	}
	return "", false
}

func markdown(node *html.Node, pageURL *url.URL) string {
	if node == nil {
		return ""
	}
	blockCode(node)
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
	))
	var opts []converter.ConvertOptionFunc
	if pageURL != nil {
		opts = append(opts, converter.WithDomain(pageURL.String()))
	}
	md, err := conv.ConvertNode(node, opts...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(md))
}

// blockCode turns each outermost <code> whose text spans lines into
// <pre><code>. go-trafilatura's output, like Python trafilatura's, marks a
// code block as a bare <code> (often <code><code>); Python's Markdown writer
// fences one with a newline, and so must we, or the converter renders it as
// inline code on one line. Its nodes carry no DataAtom: match on Data.
func blockCode(root *html.Node) {
	var found []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "code" || n.Data == "pre") {
			if n.Data == "code" {
				found = append(found, n)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	for _, n := range found {
		text := codeText(n)
		if !strings.Contains(text, "\n") || n.Parent == nil {
			continue
		}
		code := &html.Node{Type: html.ElementNode, Data: "code", DataAtom: atom.Code}
		code.AppendChild(&html.Node{Type: html.TextNode, Data: strings.Trim(text, "\n")})
		pre := &html.Node{Type: html.ElementNode, Data: "pre", DataAtom: atom.Pre}
		pre.AppendChild(code)
		n.Parent.InsertBefore(pre, n)
		n.Parent.RemoveChild(n)
	}
}

// codeText is the text under n, with <br> as a newline.
func codeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
		case n.Type == html.ElementNode && n.Data == "br":
			b.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// skipped holds the elements whose text is never visible.
var skipped = map[atom.Atom]bool{
	atom.Head: true, atom.Script: true, atom.Style: true, atom.Noscript: true,
	atom.Template: true, atom.Svg: true,
}

// blocks holds the elements that start a new line in visibleText.
var blocks = map[atom.Atom]bool{
	atom.Address: true, atom.Article: true, atom.Aside: true, atom.Blockquote: true,
	atom.Br: true, atom.Dd: true, atom.Details: true, atom.Div: true, atom.Dl: true,
	atom.Dt: true, atom.Figcaption: true, atom.Figure: true, atom.Footer: true,
	atom.Form: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true,
	atom.H5: true, atom.H6: true, atom.Header: true, atom.Hr: true, atom.Li: true,
	atom.Main: true, atom.Nav: true, atom.Ol: true, atom.P: true, atom.Pre: true,
	atom.Section: true, atom.Summary: true, atom.Table: true, atom.Td: true,
	atom.Th: true, atom.Tr: true, atom.Ul: true,
}

// visibleText is §7.1's fallback: the text of <body> without invisible
// elements, one block per line.
func visibleText(root *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipped[n.DataAtom] {
				return
			}
			if blocks[n.DataAtom] {
				b.WriteByte('\n')
				defer b.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	var lines []string
	for line := range strings.SplitSeq(b.String(), "\n") {
		if line = collapse(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func titleOf(root *html.Node) string {
	var title string
	var find func(*html.Node) bool
	find = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.DataAtom == atom.Title {
			var b strings.Builder
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					b.WriteString(c.Data)
				}
			}
			title = b.String()
			return true
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Svg {
			return false
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if find(c) {
				return true
			}
		}
		return false
	}
	find(root)
	return title
}

// collapse trims s and folds every run of whitespace into one space.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Clip returns s if it has at most max characters, else s cut at the last
// word boundary that leaves room for "…", with "…" appended.
func Clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	cut := max - 1
	if !unicode.IsSpace(runes[cut]) {
		for i := cut - 1; i > 0; i-- {
			if unicode.IsSpace(runes[i]) {
				cut = i
				break
			}
		}
	}
	return strings.TrimRightFunc(string(runes[:cut]), unicode.IsSpace) + "…"
}

// decode returns body as valid UTF-8 (§7.1 step 1): the charset of the BOM,
// else of contentType, else (HTML only) of a <meta> in the first 1024
// bytes, else UTF-8. Undecodable bytes become U+FFFD.
func decode(body []byte, contentType string, isHTML bool) string {
	label := fromBOM(body)
	if label == "" {
		label = fromContentType(contentType)
	}
	if label == "" && isHTML {
		label = fromMeta(body)
	}
	enc, name := charset.Lookup(label)
	if enc == nil || name == "utf-8" {
		return strings.ToValidUTF8(string(bytes.TrimPrefix(body, utf8BOM)), "\uFFFD")
	}
	out, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		out = body
	}
	return strings.ToValidUTF8(string(out), "\uFFFD")
}
