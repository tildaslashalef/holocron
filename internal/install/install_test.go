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
	input    map[string]string // what RunInput was given, by command
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
func (f *fakeSystem) RunInput(_ context.Context, w io.Writer, input []byte, name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	f.commands = append(f.commands, line)
	if f.input == nil {
		f.input = map[string]string{}
	}
	f.input[line] = string(input)
	// systemd-creds encrypt NAME - PATH: write a stand-in for the blob.
	if name == "systemd-creds" {
		return os.WriteFile(args[len(args)-1], []byte("encrypted:"+string(input)), 0o644)
	}
	return nil
}
func (f *fakeSystem) Terminal(*os.File) bool { return false }
func (f *fakeSystem) NoEcho(*os.File) func() { return func() {} }

func healthServer(t *testing.T, ok *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			state := "down"
			if ok.Load() {
				state = "ok"
			}
			fmt.Fprintf(w, `{"version":"0.2.0","providers":{"parallel":%q,"local":"ok"}}`, state)
		case "/v1/search":
			if r.URL.Query().Get("n") != "3" {
				http.Error(w, "n", 400)
				return
			}
			fmt.Fprint(w, `{"query":"x","provider":"parallel","results":[],"providers_failed":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func install(t *testing.T, root string, sys System, dry bool, base string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), opts(root, sys, dry, base, &out))
	return out.String(), err
}

func opts(root string, sys System, dry bool, base string, out io.Writer) Options {
	return Options{Root: root, DryRun: dry, Out: out, System: sys, HealthBase: base, Wait: 300 * time.Millisecond}
}

func put(t *testing.T, root, p, content string, mode fs.FileMode) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func exists(root, p string) bool {
	_, err := os.Lstat(filepath.Join(root, p))
	return err == nil
}

func read(t *testing.T, root, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDryRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	sys := newFake()
	sys.goos, sys.euid, sys.booted = "darwin", 501, false
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
		"check: this host lacks: Linux (this is darwin); root (run with sudo); systemd as init (continuing: dry run)",
		"would run: useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin holocron",
		"routes: /v1/search none, /v1/context none, /v1/fetch local",
		"would create /etc/systemd/system/holocron.service (mode 644)",
		"would run: systemctl daemon-reload",
		"would run: systemctl enable holocron.service",
		"would run: systemctl restart holocron.service",
		"would wait up to 300ms for http://127.0.0.1:8080/v1/health",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "podman") || strings.Contains(out, "searxng") {
		t.Errorf("a fresh host has nothing of SearXNG:\n%s", out)
	}
}

func TestInstallAndRerun(t *testing.T) {
	root := t.TempDir()
	var ok atomic.Bool
	ok.Store(true)
	srv := healthServer(t, &ok)
	sys := newFake()
	put(t, root, EncryptedKey("parallel"), "blob", 0o600)
	put(t, root, PlainKey("brave"), "BSA-key\n", 0o644)

	out, err := install(t, root, sys, false, srv.URL)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantCommands := []string{
		"useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin holocron",
		"systemctl daemon-reload",
		"systemctl enable holocron.service",
		"systemctl restart holocron.service",
	}
	if !slices.Equal(sys.commands, wantCommands) {
		t.Errorf("commands:\n%s", strings.Join(sys.commands, "\n"))
	}
	for _, want := range []string{
		"key: parallel, encrypted (/etc/credstore.encrypted/holocron.parallel-key)",
		"key: brave, a plain file (/etc/holocron/brave.key); encrypt it with: holocron key set brave < /etc/holocron/brave.key",
		"chmod 600 /etc/holocron/brave.key",
		"routes: /v1/search parallel,brave, /v1/context parallel,brave, /v1/fetch parallel,local",
		`health: {"version":"0.2.0","providers":{"parallel":"ok","local":"ok"}}`,
		"search (200 OK):",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if fi, _ := os.Stat(filepath.Join(root, PlainKey("brave"))); fi.Mode().Perm() != 0o600 {
		t.Errorf("brave key mode %o", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(root, unitPath)); fi.Mode().Perm() != 0o644 {
		t.Errorf("unit mode %o", fi.Mode().Perm())
	}
	unit := read(t, root, unitPath)
	for _, want := range []string{
		"User=holocron",
		"LoadCredentialEncrypted=parallel-key:/etc/credstore.encrypted/holocron.parallel-key\n",
		"LoadCredential=brave-key:/etc/holocron/brave.key\n",
		"ExecStart=/usr/local/bin/holocron serve --listen 0.0.0.0:8080 --search parallel,brave --context parallel,brave --fetch parallel,local\n",
		"MemoryMax=768M", "Environment=GOMEMLIMIT=600MiB", "TasksMax=96",
		"CapabilityBoundingSet=\n", "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX", "After=network-online.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("holocron.service lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "BSA-key") || strings.Contains(unit, "searxng") {
		t.Errorf("unit:\n%s", unit)
	}

	// Again: nothing changes, no user is added.
	sys.commands = nil
	out, err = install(t, root, sys, false, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unchanged "+unitPath) || !slices.Equal(sys.commands, wantCommands[1:]) {
		t.Errorf("second run:\n%s\n%s", out, strings.Join(sys.commands, "\n"))
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

// An older install's SearXNG goes: its unit stops, its files go, and its
// image is removed with the image the quadlet named.
func TestRemovesSearXNG(t *testing.T) {
	root := t.TempDir()
	var ok atomic.Bool
	ok.Store(true)
	srv := healthServer(t, &ok)
	sys := newFake()
	sys.hasUser = true
	put(t, root, legacyContainer, "[Container]\nImage=docker.io/searxng/searxng@sha256:abc\nNetwork=host\n", 0o644)
	put(t, root, legacySettings+"/settings.yml", "secret", 0o600)
	put(t, root, legacyCache+"/x", "", 0o644)

	out, _ := install(t, root, sys, true, srv.URL)
	if !strings.Contains(out, "would remove "+legacyContainer) || !exists(root, legacyContainer) || len(sys.commands) != 0 {
		t.Errorf("dry run:\n%s", out)
	}
	out, err := install(t, root, sys, false, srv.URL)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, p := range []string{legacyContainer, legacySettings, legacyCache} {
		if exists(root, p) {
			t.Errorf("%s is still there", p)
		}
	}
	want := []string{
		"systemctl stop searxng.service",
		"systemctl daemon-reload",
		"podman rmi docker.io/searxng/searxng@sha256:abc",
		"systemctl daemon-reload",
		"systemctl enable holocron.service",
		"systemctl restart holocron.service",
	}
	if !slices.Equal(sys.commands, want) {
		t.Errorf("commands:\n%s", strings.Join(sys.commands, "\n"))
	}
	if !strings.Contains(out, "apt purge podman") {
		t.Errorf("output:\n%s", out)
	}
	// Without podman on the host, the image is not asked for.
	put(t, root, legacyContainer, "Image=x\n", 0o644)
	sys.commands, sys.missing = nil, []string{"podman"}
	if _, err := install(t, root, sys, false, srv.URL); err != nil || slices.ContainsFunc(sys.commands, func(c string) bool { return strings.HasPrefix(c, "podman") }) {
		t.Errorf("without podman: %v %v", err, sys.commands)
	}
}

// key set encrypts with systemd-creds, the key on stdin, and drops the
// plain file; without systemd-creds it writes a 0600 file. key rm removes
// both.
func TestKeys(t *testing.T) {
	var ok atomic.Bool
	ok.Store(true)
	srv := healthServer(t, &ok)

	root := t.TempDir()
	sys := newFake()
	put(t, root, PlainKey("parallel"), "old\n", 0o600)
	var out bytes.Buffer
	if err := SetKey(context.Background(), opts(root, sys, false, srv.URL, &out), "parallel", strings.NewReader("  sk-new  \nignored\n")); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	line := "systemd-creds encrypt --name=parallel-key - " + filepath.Join(root, EncryptedKey("parallel"))
	if sys.input[line] != "sk-new" || exists(root, PlainKey("parallel")) {
		t.Errorf("encrypt: %q, plain kept %v; commands %v", sys.input[line], exists(root, PlainKey("parallel")), sys.commands)
	}
	if fi, _ := os.Stat(filepath.Join(root, EncryptedKey("parallel"))); fi.Mode().Perm() != 0o600 {
		t.Errorf("blob mode %o", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(root, CredStore)); fi.Mode().Perm() != 0o700 {
		t.Errorf("credstore mode %o", fi.Mode().Perm())
	}
	if strings.Contains(out.String(), "sk-new") || strings.Contains(read(t, root, unitPath), "sk-new") {
		t.Error("the key was printed or put in the unit")
	}
	if !strings.Contains(read(t, root, unitPath), "LoadCredentialEncrypted=parallel-key:") {
		t.Error("unit lacks the encrypted credential")
	}

	root = t.TempDir()
	sys = newFake()
	sys.missing = []string{"systemd-creds"}
	out.Reset()
	if err := SetKey(context.Background(), opts(root, sys, false, srv.URL, &out), "brave", strings.NewReader("BSA-1")); err != nil {
		t.Fatal(err)
	}
	if read(t, root, PlainKey("brave")) != "BSA-1\n" {
		t.Error("plain key")
	}
	if fi, _ := os.Stat(filepath.Join(root, PlainKey("brave"))); fi.Mode().Perm() != 0o600 {
		t.Errorf("plain key mode %o", fi.Mode().Perm())
	}
	for _, bad := range []string{"", "  \n", "two words\n"} {
		if err := SetKey(context.Background(), opts(root, sys, false, srv.URL, io.Discard), "brave", strings.NewReader(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}

	put(t, root, EncryptedKey("brave"), "blob", 0o600)
	out.Reset()
	if err := RemoveKey(context.Background(), opts(root, sys, false, srv.URL, &out), "brave"); err != nil {
		t.Fatal(err)
	}
	if exists(root, PlainKey("brave")) || exists(root, EncryptedKey("brave")) || strings.Contains(read(t, root, unitPath), "brave") {
		t.Errorf("rm left the key:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "routes: /v1/search none") {
		t.Errorf("output:\n%s", out.String())
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
	// Podman is no longer needed.
	sys := newFake()
	sys.missing = []string{"podman"}
	if _, err := install(t, t.TempDir(), sys, true, ""); err != nil {
		t.Error(err)
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
	if !strings.Contains(out, `"parallel":"down"`) {
		t.Errorf("output:\n%s", out)
	}
	if n := len(sys.commands); n < 1 || sys.commands[n-1] != "journalctl --no-pager -n 40 -u holocron.service" {
		t.Errorf("commands:\n%s", strings.Join(sys.commands, "\n"))
	}
}
