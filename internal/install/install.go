package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"text/template"
	"time"
)

//go:embed files
var files embed.FS

// System is what Run needs of the host; tests replace it.
type System interface {
	GOOS() string
	Euid() int
	LookPath(file string) (string, error)
	UserExists(name string) bool
	// Booted reports whether systemd is the running init.
	Booted() bool
	// Run runs a command with its output to w.
	Run(ctx context.Context, w io.Writer, name string, args ...string) error
}

// Options configure Run.
type Options struct {
	Root       string        // where files are written; "/" on a host
	DryRun     bool          // print what would change, change nothing
	Out        io.Writer     // progress and the final report
	System     System        // nil: this host
	HealthBase string        // holocron's address; "" is http://127.0.0.1:8080
	Wait       time.Duration // how long to wait for health; 0 is 60 s
}

const (
	settingsPath  = "/etc/searxng/settings.yml"
	containerPath = "/etc/containers/systemd/searxng.container"
	unitPath      = "/etc/systemd/system/holocron.service"
	cacheDir      = "/var/cache/searxng"
	// BraveKeyPath is where the deployment puts Brave's key; its presence
	// makes the unit run brave,searxng (§9 step 3).
	BraveKeyPath = "/etc/holocron/brave.key"
)

// Run sets the host up (§9): checks, user, SearXNG's files and image,
// holocron's unit, the restarts, and the wait for health. It is
// idempotent.
func Run(ctx context.Context, o Options) error {
	if o.System == nil {
		o.System = hostSystem{}
	}
	if o.Root == "" {
		o.Root = "/"
	}
	if o.HealthBase == "" {
		o.HealthBase = "http://127.0.0.1:8080"
	}
	if o.Wait == 0 {
		o.Wait = 60 * time.Second
	}
	in := &installer{Options: o}
	if o.DryRun {
		in.say("dry run: nothing is changed")
	}

	if missing := in.check(); len(missing) > 0 {
		msg := "this host lacks: " + strings.Join(missing, "; ")
		if !o.DryRun {
			return errors.New(msg)
		}
		in.say("check: %s (continuing: dry run)", msg)
	}

	if !o.System.UserExists("holocron") {
		if err := in.run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
			"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "holocron"); err != nil {
			return err
		}
	}

	secret, err := in.secret()
	if err != nil {
		return err
	}
	settings, err := render("files/settings.yml.tmpl", map[string]string{"SecretKey": secret})
	if err != nil {
		return err
	}
	container, err := render("files/searxng.container", map[string]string{"Image": SearXNGImage})
	if err != nil {
		return err
	}
	brave, err := in.braveKey()
	if err != nil {
		return err
	}
	unit, err := render("files/holocron.service.tmpl", map[string]any{"Brave": brave, "KeyPath": BraveKeyPath})
	if err != nil {
		return err
	}
	for _, dir := range []string{"/etc/searxng", cacheDir, filepath.Dir(containerPath), filepath.Dir(unitPath)} {
		if err := in.mkdir(dir); err != nil {
			return err
		}
	}
	if err := in.write(settingsPath, settings, 0o600); err != nil {
		return err
	}
	if err := in.write(containerPath, container, 0o644); err != nil {
		return err
	}
	if err := in.run(ctx, "podman", "pull", SearXNGImage); err != nil {
		return err
	}
	if err := in.write(unitPath, unit, 0o644); err != nil {
		return err
	}
	for _, cmd := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "restart", "searxng.service"},
		{"systemctl", "enable", "holocron.service"},
		{"systemctl", "restart", "holocron.service"},
	} {
		if err := in.run(ctx, cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return in.waitHealthy(ctx)
}

type installer struct {
	Options
}

func (in *installer) say(format string, args ...any) {
	fmt.Fprintf(in.Out, format+"\n", args...)
}

// braveKey reports whether Brave's key is on the host, and makes it 0600
// if it is not (§9 step 3). systemd reads it as root, whoever owns it.
func (in *installer) braveKey() (bool, error) {
	fi, err := os.Lstat(in.path(BraveKeyPath))
	if errors.Is(err, fs.ErrNotExist) {
		in.say("provider: searxng (no %s)", BraveKeyPath)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", BraveKeyPath)
	}
	in.say("provider: brave,searxng (%s is present)", BraveKeyPath)
	if fi.Mode().Perm() != 0o600 {
		if in.DryRun {
			in.say("would chmod 600 %s", BraveKeyPath)
		} else {
			in.say("chmod 600 %s", BraveKeyPath)
			if err := os.Chmod(in.path(BraveKeyPath), 0o600); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// check returns what the host lacks, by name.
func (in *installer) check() []string {
	var missing []string
	if g := in.System.GOOS(); g != "linux" {
		missing = append(missing, "Linux (this is "+g+")")
	}
	if in.System.Euid() != 0 {
		missing = append(missing, "root (run with sudo)")
	}
	if !in.System.Booted() {
		missing = append(missing, "systemd as init")
	}
	for _, tool := range []string{"systemctl", "podman", "useradd"} {
		if _, err := in.System.LookPath(tool); err != nil {
			missing = append(missing, tool+" on PATH")
		}
	}
	return missing
}

func (in *installer) run(ctx context.Context, name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	if in.DryRun {
		in.say("would run: %s", line)
		return nil
	}
	in.say("run: %s", line)
	if err := in.System.Run(ctx, in.Out, name, args...); err != nil {
		return fmt.Errorf("%s: %w", line, err)
	}
	return nil
}

func (in *installer) path(p string) string {
	return filepath.Join(in.Root, p)
}

func (in *installer) mkdir(dir string) error {
	if fi, err := os.Stat(in.path(dir)); err == nil && fi.IsDir() {
		return nil
	}
	if in.DryRun {
		in.say("would create %s/", dir)
		return nil
	}
	in.say("create %s/", dir)
	return os.MkdirAll(in.path(dir), 0o755)
}

// write replaces path with data when they differ, atomically, keeping the
// owner of the file it replaces (SearXNG's entrypoint owns /etc/searxng).
func (in *installer) write(path string, data []byte, mode fs.FileMode) error {
	full := in.path(path)
	old, err := os.ReadFile(full)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if exists && bytes.Equal(old, data) {
		if fi, err := os.Stat(full); err == nil && fi.Mode().Perm() != mode {
			if in.DryRun {
				in.say("would chmod %o %s", mode, path)
				return nil
			}
			in.say("chmod %o %s", mode, path)
			return os.Chmod(full, mode)
		}
		in.say("unchanged %s", path)
		return nil
	}
	verb := "create"
	if exists {
		verb = "update"
	}
	if in.DryRun {
		in.say("would %s %s (mode %o)", verb, path, mode)
		return nil
	}
	in.say("%s %s", verb, path)
	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if fi, err := os.Stat(full); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && in.System.Euid() == 0 {
			if err := os.Chown(tmp.Name(), int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp.Name(), full)
}

var secretLine = regexp.MustCompile(`(?m)^\s*secret_key:\s*"([0-9a-f]{64})"\s*$`)

// secret returns the secret_key of an existing settings.yml, else a new
// one from crypto/rand.
func (in *installer) secret() (string, error) {
	if old, err := os.ReadFile(in.path(settingsPath)); err == nil {
		if m := secretLine.FindSubmatch(old); m != nil {
			return string(m[1]), nil
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func render(name string, data any) ([]byte, error) {
	t, err := template.ParseFS(files, name)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// waitHealthy waits for /v1/health to report every provider ok, then
// prints it and a three-result search. On timeout it prints the units'
// last journal lines and fails.
func (in *installer) waitHealthy(ctx context.Context) error {
	healthURL := in.HealthBase + "/v1/health"
	if in.DryRun {
		in.say("would wait up to %s for %s", in.Wait, healthURL)
		return nil
	}
	in.say("wait up to %s for %s", in.Wait, healthURL)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	deadline := time.Now().Add(in.Wait)
	var last string
	for {
		body, ok := healthy(ctx, client, healthURL)
		if ok {
			in.say("health: %s", body)
			break
		}
		last = body
		if time.Now().After(deadline) {
			in.say("holocron is not healthy after %s (last: %s); the journal:", in.Wait, last)
			for _, unit := range []string{"searxng.service", "holocron.service"} {
				in.System.Run(ctx, in.Out, "journalctl", "--no-pager", "-n", "40", "-u", unit)
			}
			return fmt.Errorf("holocron did not become healthy within %s", in.Wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	searchURL := in.HealthBase + "/v1/search?n=3&q=" + url.QueryEscape("SearXNG metasearch engine")
	resp, err := client.Get(searchURL)
	if err != nil {
		in.say("search: %v", err)
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	in.say("search (%s): %s", resp.Status, strings.TrimSpace(string(b)))
	return nil
}

// healthy reports whether every provider in /v1/health is ok, with the
// body (or the error) to print.
func healthy(ctx context.Context, client *http.Client, u string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err.Error(), false
	}
	resp, err := client.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	body := strings.TrimSpace(string(b))
	var h struct {
		Providers map[string]string `json:"providers"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &h) != nil || len(h.Providers) == 0 {
		return body, false
	}
	for _, state := range h.Providers {
		if state != "ok" {
			return body, false
		}
	}
	return body, true
}

// hostSystem is this machine.
type hostSystem struct{}

func (hostSystem) GOOS() string                         { return runtime.GOOS }
func (hostSystem) Euid() int                            { return os.Geteuid() }
func (hostSystem) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (hostSystem) UserExists(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}

func (hostSystem) Booted() bool {
	fi, err := os.Stat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

func (hostSystem) Run(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}
