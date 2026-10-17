package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSystem struct {
	goos     string
	euid     int
	missing  []string // tools not on PATH
	booted   bool
	hasUser  bool
	commands []string
}

func newFake() *fakeSystem { return &fakeSystem{goos: "linux", booted: true} }

func (f *fakeSystem) GOOS() string { return f.goos }
func (f *fakeSystem) Euid() int    { return f.euid }
func (f *fakeSystem) LookPath(file string) (string, error) {
	if slices.Contains(f.missing, file) {
		return "", fmt.Errorf("%s: not found", file)
	}
	return "/usr/bin/" + file, nil
}
func (f *fakeSystem) UserExists(string) bool { return f.hasUser }
func (f *fakeSystem) Booted() bool           { return f.booted }
func (f *fakeSystem) Run(_ context.Context, w io.Writer, name string, args ...string) error {
	f.commands = append(f.commands, strings.Join(append([]string{name}, args...), " "))
	if name == "useradd" {
		f.hasUser = true
	}
	return nil
}

func healthServer(t *testing.T, ok *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			state := "down"
			if ok.Load() {
				state = "ok"
			}
			fmt.Fprintf(w, `{"version":"0.1.0","providers":{"searxng":%q}}`, state)
		case "/v1/search":
			if r.URL.Query().Get("n") != "3" {
				http.Error(w, "n", 400)
				return
			}
			fmt.Fprint(w, `{"query":"x","provider":"searxng","results":[],"engines_failed":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func install(t *testing.T, root string, sys System, dry bool, base string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), Options{Root: root, DryRun: dry, Out: &out, System: sys, HealthBase: base, Wait: 300 * time.Millisecond})
	return out.String(), err
}

func TestDryRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	sys := newFake()
	sys.goos, sys.euid, sys.booted, sys.missing = "darwin", 501, false, []string{"podman"}
	out, err := install(t, root, sys, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sys.commands) != 0 {
		t.Errorf("ran %v", sys.commands)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
	for _, want := range []string{
		"dry run: nothing is changed",
		"check: this host lacks: Linux (this is darwin); root (run with sudo); systemd as init; podman on PATH (continuing: dry run)",
		"would run: useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin holocron",
		"would create /etc/searxng/",
		"would create /var/cache/searxng/",
		"would create /etc/searxng/settings.yml (mode 600)",
		"would create /etc/containers/systemd/searxng.container (mode 644)",
		"would create /etc/systemd/system/holocron.service (mode 644)",
		"would run: podman pull " + SearXNGImage,
		"would run: systemctl daemon-reload",
		"would run: systemctl restart searxng.service",
		"would run: systemctl enable holocron.service",
		"would run: systemctl restart holocron.service",
		"would wait up to 300ms for http://127.0.0.1:8080/v1/health",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestInstallAndRerun(t *testing.T) {
	root := t.TempDir()
	var ok atomic.Bool
	ok.Store(true)
	srv := healthServer(t, &ok)
	sys := newFake()

	out, err := install(t, root, sys, false, srv.URL)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantCommands := []string{
		"useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin holocron",
		"podman pull " + SearXNGImage,
		"systemctl daemon-reload",
		"systemctl restart searxng.service",
		"systemctl enable holocron.service",
		"systemctl restart holocron.service",
	}
	if !slices.Equal(sys.commands, wantCommands) {
		t.Errorf("commands:\n%s", strings.Join(sys.commands, "\n"))
	}
	if !strings.Contains(out, `health: {"version":"0.1.0","providers":{"searxng":"ok"}}`) || !strings.Contains(out, "search (200 OK):") {
		t.Errorf("output:\n%s", out)
	}

	modes := map[string]fs.FileMode{settingsPath: 0o600, containerPath: 0o644, unitPath: 0o644}
	for p, mode := range modes {
		fi, err := os.Stat(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Errorf("%s: mode %o", p, fi.Mode().Perm())
		}
	}
	if fi, err := os.Stat(filepath.Join(root, cacheDir)); err != nil || !fi.IsDir() {
		t.Errorf("%s: %v", cacheDir, err)
	}
	settings := read(t, root, settingsPath)
	secret := regexp.MustCompile(`secret_key: "([0-9a-f]{64})"`).FindStringSubmatch(settings)
	if secret == nil {
		t.Fatalf("no secret:\n%s", settings)
	}
	for _, want := range []string{"keep_only:", "- pkg.go.dev", "- arch linux wiki", "- json", "limiter: false", "public_instance: false",
		"image_proxy: false", "request_timeout: 4.0", "max_request_timeout: 8.0", "name: mojeek\n    inactive: false"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.yml lacks %q", want)
		}
	}
	container := read(t, root, containerPath)
	for _, want := range []string{"Image=" + SearXNGImage, "Network=host", "Environment=GRANIAN_HOST=127.0.0.1", "Environment=SEARXNG_PORT=8888",
		"Volume=/etc/searxng:/etc/searxng", "Volume=/var/cache/searxng:/var/cache/searxng", "NoNewPrivileges=true", "Restart=always"} {
		if !strings.Contains(container, want) {
			t.Errorf("searxng.container lacks %q", want)
		}
	}
	unit := read(t, root, unitPath)
	for _, want := range []string{"User=holocron", "ExecStart=/usr/local/bin/holocron serve --listen 0.0.0.0:8080", "MemoryMax=1G",
		"Environment=GOMEMLIMIT=800MiB", "CapabilityBoundingSet=\n", "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX", "After=network-online.target searxng.service"} {
		if !strings.Contains(unit, want) {
			t.Errorf("holocron.service lacks %q", want)
		}
	}

	// Again: the secret is kept, nothing changes, no user is added.
	sys.commands = nil
	out, err = install(t, root, sys, false, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, root, settingsPath) != settings {
		t.Error("settings.yml changed on the second run")
	}
	for _, p := range []string{settingsPath, containerPath, unitPath} {
		if !strings.Contains(out, "unchanged "+p) {
			t.Errorf("second run: %s not unchanged:\n%s", p, out)
		}
	}
	if !slices.Equal(sys.commands, wantCommands[1:]) {
		t.Errorf("second run commands:\n%s", strings.Join(sys.commands, "\n"))
	}

	// A file edited by hand is put back, and a dry run says so first.
	os.WriteFile(filepath.Join(root, unitPath), []byte("edited"), 0o644)
	sys.commands = nil
	out, _ = install(t, root, sys, true, srv.URL)
	if !strings.Contains(out, "would update "+unitPath) || read(t, root, unitPath) != "edited" || len(sys.commands) != 0 {
		t.Errorf("dry run over an edit:\n%s", out)
	}
	if _, err := install(t, root, sys, false, srv.URL); err != nil || read(t, root, unitPath) != unit {
		t.Errorf("edit not put back: %v", err)
	}
}

func TestChecks(t *testing.T) {
	for _, tt := range []struct {
		change func(*fakeSystem)
		want   string
	}{
		{func(f *fakeSystem) { f.goos = "darwin" }, "Linux (this is darwin)"},
		{func(f *fakeSystem) { f.euid = 1000 }, "root (run with sudo)"},
		{func(f *fakeSystem) { f.booted = false }, "systemd as init"},
		{func(f *fakeSystem) { f.missing = []string{"podman"} }, "podman on PATH"},
		{func(f *fakeSystem) { f.missing = []string{"systemctl"} }, "systemctl on PATH"},
	} {
		sys := newFake()
		tt.change(sys)
		root := t.TempDir()
		_, err := install(t, root, sys, false, "")
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("err = %v, want %q", err, tt.want)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 || len(sys.commands) != 0 {
			t.Errorf("%s: changed the host", tt.want)
		}
	}
}

func TestUnhealthy(t *testing.T) {
	var ok atomic.Bool
	srv := healthServer(t, &ok)
	sys := newFake()
	out, err := install(t, t.TempDir(), sys, false, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, `"searxng":"down"`) {
		t.Errorf("output:\n%s", out)
	}
	n := len(sys.commands)
	if n < 2 || sys.commands[n-2] != "journalctl --no-pager -n 40 -u searxng.service" ||
		sys.commands[n-1] != "journalctl --no-pager -n 40 -u holocron.service" {
		t.Errorf("commands:\n%s", strings.Join(sys.commands, "\n"))
	}
}

func read(t *testing.T, root, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Brave's key on the host switches the unit to brave,searxng and is made
// 0600; removing it switches back.
func TestBraveKeySwitch(t *testing.T) {
	root := t.TempDir()
	var ok atomic.Bool
	ok.Store(true)
	srv := healthServer(t, &ok)
	sys := newFake()
	key := filepath.Join(root, BraveKeyPath)
	if err := os.MkdirAll(filepath.Dir(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("BSA-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := install(t, root, sys, true, srv.URL)
	if !strings.Contains(out, "provider: brave,searxng (/etc/holocron/brave.key is present)\n") || !strings.Contains(out, "would chmod 600 "+BraveKeyPath) {
		t.Errorf("dry run:\n%s", out)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o644 {
		t.Error("a dry run changed the key's mode")
	}

	out, err := install(t, root, sys, false, srv.URL)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %o", fi.Mode().Perm())
	}
	unit := read(t, root, unitPath)
	for _, want := range []string{
		"LoadCredential=brave-key:/etc/holocron/brave.key\n",
		"ExecStart=/usr/local/bin/holocron serve --listen 0.0.0.0:8080 --provider brave,searxng --brave-key-file ${CREDENTIALS_DIRECTORY}/brave-key\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "BSA-key") {
		t.Error("the unit holds the key")
	}

	os.Remove(key)
	out, _ = install(t, root, sys, false, srv.URL)
	unit = read(t, root, unitPath)
	if !strings.Contains(out, "provider: searxng (no /etc/holocron/brave.key)") || strings.Contains(unit, "brave") ||
		!strings.Contains(unit, "ExecStart=/usr/local/bin/holocron serve --listen 0.0.0.0:8080\n") {
		t.Errorf("without the key:\n%s\n%s", out, unit)
	}
}
