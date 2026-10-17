package extract

import (
	"bytes"
	"mime"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// fromBOM, fromContentType and fromMeta return a charset label, or "".

func fromBOM(body []byte) string {
	switch {
	case bytes.HasPrefix(body, utf8BOM):
		return "utf-8"
	case bytes.HasPrefix(body, []byte{0xfe, 0xff}):
		return "utf-16be"
	case bytes.HasPrefix(body, []byte{0xff, 0xfe}):
		return "utf-16le"
	}
	return ""
}

func fromContentType(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	if enc, _ := charset.Lookup(params["charset"]); enc == nil {
		return ""
	}
	return params["charset"]
}

// fromMeta finds <meta charset> or <meta http-equiv=content-type> in the
// first 1024 bytes, as browsers prescan.
func fromMeta(body []byte) string {
	z := html.NewTokenizer(bytes.NewReader(body[:min(len(body), 1024)]))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "meta" || !hasAttr {
				continue
			}
			var label, httpEquiv, content string
			for more := true; more; {
				var k, v []byte
				k, v, more = z.TagAttr()
				switch strings.ToLower(string(k)) {
				case "charset":
					label = string(v)
				case "http-equiv":
					httpEquiv = strings.ToLower(string(v))
				case "content":
					content = string(v)
				}
			}
			if label == "" && httpEquiv == "content-type" {
				if _, params, err := mime.ParseMediaType(content); err == nil {
					label = params["charset"]
				}
			}
			if enc, name := charset.Lookup(label); enc != nil {
				// A document that says UTF-16 in ASCII bytes is not UTF-16.
				if strings.HasPrefix(name, "utf-16") {
					return "utf-8"
				}
				return label
			}
		}
	}
}
