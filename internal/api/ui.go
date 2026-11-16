package api

import (
	"embed"
	"net/http"
	"strconv"
)

// The dashboard's files (§8.3), embedded so the release stays one binary.
//
//go:embed ui
var ui embed.FS

// assets map the dashboard's paths to their files and types.
var assets = map[string]struct{ file, ctype string }{
	"/":            {"ui/index.html", "text/html; charset=utf-8"},
	"/ui/app.js":   {"ui/app.js", "text/javascript; charset=utf-8"},
	"/ui/app.css":  {"ui/app.css", "text/css; charset=utf-8"},
	"/ui/icon.svg": {"ui/icon.svg", "image/svg+xml"},
	"/favicon.ico": {"ui/icon.svg", "image/svg+xml"},
}

func isAsset(path string) bool {
	_, ok := assets[path]
	return ok
}

// csp keeps the page to its own files: no inline script, no other host,
// no frames, no forms. What the page shows from /v1/stats is set as text.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func serveAsset(w http.ResponseWriter, r *http.Request) {
	a := assets[r.URL.Path]
	b, err := ui.ReadFile(a.file)
	if err != nil {
		http.Error(w, "missing asset", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}
