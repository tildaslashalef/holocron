package main

import (
	"bytes"
	"flag"
	"slices"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{nil, 2, "", "usage:"},
		{[]string{"version"}, 0, "holocron dev (", ""},
		{[]string{"version"}, 0, "searxng docker.io/searxng/searxng@sha256:", ""},
		{[]string{"help"}, 0, "usage:", ""},
		{[]string{"bogus"}, 2, "", `unknown command "bogus"`},
		{[]string{"fetch"}, 2, "", "fetch needs one URL"},
		{[]string{"fetch", "a", "b"}, 2, "", "fetch needs one URL"},
		{[]string{"fetch", "--bogus", "x"}, 2, "", "flag provided but not defined"},
		{[]string{"fetch", "http://192.168.1.20:8443/"}, 1, "", "blocked_address: 192.168.1.20 is not a public address"},
		{[]string{"fetch", "http://nas/", "--text"}, 1, "", "blocked_address"},
		{[]string{"fetch", "https://example.org:8443/"}, 1, "", "bad_request"},
		{[]string{"fetch", "--max-chars", "0", "https://example.org/"}, 2, "", "--max-chars"},
		{[]string{"search"}, 2, "", "search needs a QUERY"},
		{[]string{"search", "x", "--provider", "bing"}, 2, "", `unknown provider "bing"`},
		{[]string{"search", "x", "--provider", "searxng,searxng"}, 2, "", `provider "searxng" is named twice`},
		{[]string{"search", "x", "--provider", "brave"}, 1, "", "brave: --brave-key-file is required"},
		{[]string{"search", "x", "--provider", "brave,searxng", "--brave-key-file", "/nonexistent/key"}, 1, "", "brave: reading the key"},
		{[]string{"context"}, 2, "", "context needs a QUERY"},
		{[]string{"search", "x", "--freshness", "lastweek"}, 2, "", "--freshness must be"},
		{[]string{"context", "x", "--freshness", "2026-10-02to2026-10-01"}, 2, "", "--freshness must be"},
		{[]string{"context", "x", "--tokens", "10"}, 2, "", "--tokens must be 1024 or more"},
		{[]string{"context", "x", "--searxng", "http://127.0.0.1:1"}, 1, "", "not_available: searxng has no context endpoint"},
		{[]string{"serve", "extra"}, 2, "", "serve takes no arguments"},
		{[]string{"serve", "--provider", "kagi"}, 2, "", `unknown provider "kagi"`},
		{[]string{"serve", "--searxng", "nope"}, 1, "", "not an http(s) base URL"},
		{[]string{"serve", "-h"}, 0, "usage:", ""},
		{[]string{"install", "now"}, 2, "", "install takes no arguments"},
		{[]string{"check", "a", "b"}, 2, "", "at most one BASE_URL"},
		{[]string{"check", "http://127.0.0.1:1"}, 1, "FAIL health", "15 of 15 checks failed"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		code := run(tt.args, &stdout, &stderr)
		if code != tt.code {
			t.Errorf("run(%q) = %d, want %d (stderr %q)", tt.args, code, tt.code, stderr.String())
		}
		if !strings.Contains(stdout.String(), tt.stdout) {
			t.Errorf("run(%q) stdout = %q, want it to contain %q", tt.args, stdout.String(), tt.stdout)
		}
		if !strings.Contains(stderr.String(), tt.stderr) {
			t.Errorf("run(%q) stderr = %q, want it to contain %q", tt.args, stderr.String(), tt.stderr)
		}
	}
}

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	text := fs.Bool("text", false, "")
	n := fs.Int("n", 0, "")
	pos, err := parse(fs, []string{"--n", "3", "first", "--text", "second"})
	if err != nil || !*text || *n != 3 || !slices.Equal(pos, []string{"first", "second"}) {
		t.Errorf("parse = %v %v, text %v n %d", pos, err, *text, *n)
	}
}
