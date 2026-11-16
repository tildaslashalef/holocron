package install

import (
	"bufio"
	"bytes"
	"context"
	"embed"
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
	"slices"
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
	// RunInput is Run with input on the command's stdin.
	RunInput(ctx context.Context, w io.Writer, input []byte, name string, args ...string) error
	// Terminal reports whether f is a terminal; NoEcho turns its echo off
	// and returns what turns it back on.
	Terminal(f *os.File) bool
	NoEcho(f *os.File) (restore func())
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
	unitPath = "/etc/systemd/system/holocron.service"
	// KeyDir holds plain key files, <provider>.key (§9 step 3).
	KeyDir = "/etc/holocron"
	// CredStore holds encrypted keys, holocron.<provider>-key, where
	// systemd-creds puts them (§9 step 3).
	CredStore = "/etc/credstore.encrypted"

	// What an older install wrote for SearXNG, removed by this one.
	legacyContainer = "/etc/containers/systemd/searxng.container"
	legacySettings  = "/etc/searxng"
	legacyCache     = "/var/cache/searxng"
)

// KeyProviders are the providers that take a key, in their default order.
var KeyProviders = []string{"parallel", "brave"}

// PlainKey is where a provider's plain key file is.
func PlainKey(name string) string { return KeyDir + "/" + name + ".key" }

// EncryptedKey is where a provider's encrypted key is.
func EncryptedKey(name string) string { return CredStore + "/holocron." + name + "-key" }

// Key is one provider's key on the host.
type Key struct {
	Name      string
	Path      string
	Encrypted bool
}

func (o *Options) defaults() {
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
}

// Run sets the host up (§9): checks, user, the keys, SearXNG's removal
// when an older install left it, holocron's unit, the restart, and the
// wait for health. It is idempotent.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	in := &installer{Options: o}
	if o.DryRun {
		in.say("dry run: nothing is changed")
	}
	if err := in.checkHost(); err != nil {
		return err
	}

	if !o.System.UserExists("holocron") {
		if err := in.run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
			"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "holocron"); err != nil {
			return err
		}
	}

	keys, err := in.keys()
	if err != nil {
		return err
	}
	var withKey []string
	for _, k := range keys {
		withKey = append(withKey, k.Name)
	}
	search := "none"
	if len(withKey) > 0 {
		search = strings.Join(withKey, ",")
	}
	fetch := "local"
	if slices.Contains(withKey, "parallel") {
		fetch = "parallel,local"
	}
	in.say("routes: /v1/search %s, /v1/context %s, /v1/fetch %s", search, search, fetch)
	if len(keys) == 0 {
		in.say("no provider key on this host: search and context answer not_available; add one with: holocron key set parallel")
	}

	if err := in.removeSearXNG(ctx); err != nil {
		return err
	}

	unit, err := render("files/holocron.service.tmpl", map[string]any{"Keys": keys, "Search": search, "Context": search, "Fetch": fetch})
	if err != nil {
		return err
	}
	if err := in.mkdir(filepath.Dir(unitPath)); err != nil {
		return err
	}
	if err := in.write(unitPath, unit, 0o644); err != nil {
		return err
	}
	for _, cmd := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "holocron.service"},
		{"systemctl", "restart", "holocron.service"},
	} {
		if err := in.run(ctx, cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return in.waitHealthy(ctx, len(keys) > 0)
}

// checkHost fails on a host that lacks what install needs; a dry run
// reports it and goes on.
func (in *installer) checkHost() error {
	missing := in.check()
	if len(missing) == 0 {
		return nil
	}
	msg := "this host lacks: " + strings.Join(missing, "; ")
	if !in.DryRun {
		return errors.New(msg)
	}
	in.say("check: %s (continuing: dry run)", msg)
	return nil
}

// keys finds each provider's key: encrypted first, else a plain file,
// made 0600 if it is not (systemd reads it as root, whoever owns it).
func (in *installer) keys() ([]Key, error) {
	var out []Key
	for _, name := range KeyProviders {
		if fi, err := os.Lstat(in.path(EncryptedKey(name))); err == nil && fi.Mode().IsRegular() {
			in.say("key: %s, encrypted (%s)", name, EncryptedKey(name))
			out = append(out, Key{name, EncryptedKey(name), true})
			continue
		}
		plain := PlainKey(name)
		fi, err := os.Lstat(in.path(plain))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", plain)
		}
		in.say("key: %s, a plain file (%s); encrypt it with: holocron key set %s < %s", name, plain, name, plain)
		if fi.Mode().Perm() != 0o600 {
			if in.DryRun {
				in.say("would chmod 600 %s", plain)
			} else {
				in.say("chmod 600 %s", plain)
				if err := os.Chmod(in.path(plain), 0o600); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, Key{name, plain, false})
	}
	return out, nil
}

var imageLine = regexp.MustCompile(`(?m)^\s*Image=(\S+)\s*$`)

// removeSearXNG removes what an install before 0.2.0 wrote for SearXNG
// (§9 step 4): its unit, its settings and cache, and its image. Podman
// itself is the host's package, and stays.
func (in *installer) removeSearXNG(ctx context.Context) error {
	var found []string
	for _, p := range []string{legacyContainer, legacySettings, legacyCache} {
		if _, err := os.Lstat(in.path(p)); err == nil {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return nil
	}
	in.say("SearXNG from an older install: removing %s", strings.Join(found, ", "))
	var image string
	if b, err := os.ReadFile(in.path(legacyContainer)); err == nil {
		if m := imageLine.FindSubmatch(b); m != nil {
			image = string(m[1])
		}
	}
	// A quadlet's unit is generated: stopping it and removing its file is
	// how it goes.
	in.runQuiet(ctx, "systemctl", "stop", "searxng.service")
	for _, p := range found {
		if err := in.remove(p); err != nil {
			return err
		}
	}
	if err := in.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if image != "" {
		if _, err := in.System.LookPath("podman"); err == nil {
			in.runQuiet(ctx, "podman", "rmi", image)
		}
	}
	in.say("SearXNG is removed; Podman is the host's package: sudo apt purge podman && sudo apt autoremove --purge")
	return nil
}

// SetKey stores a provider's key read from r (§9.4): encrypted with
// systemd-creds when the host has it, else a plain 0600 file; then Run.
// The key goes to systemd-creds on its stdin, never in an argument.
func SetKey(ctx context.Context, o Options, name string, r io.Reader) error {
	o.defaults()
	in := &installer{Options: o}
	if err := in.checkHost(); err != nil {
		return err
	}
	_, credsErr := o.System.LookPath("systemd-creds")
	if o.DryRun {
		if credsErr == nil {
			in.say("would read %s's key from stdin and run: systemd-creds encrypt --name=%s-key - %s", name, name, EncryptedKey(name))
		} else {
			in.say("would read %s's key from stdin and write %s (mode 600): this host has no systemd-creds", name, PlainKey(name))
		}
		return Run(ctx, o)
	}
	key, err := readKey(o.System, r, o.Out, name)
	if err != nil {
		return err
	}
	if credsErr == nil {
		if err := in.mkdirMode(CredStore, 0o700); err != nil {
			return err
		}
		line := fmt.Sprintf("systemd-creds encrypt --name=%s-key - %s", name, EncryptedKey(name))
		in.say("run: %s", line)
		if err := o.System.RunInput(ctx, o.Out, []byte(key), "systemd-creds", "encrypt", "--name="+name+"-key", "-", in.path(EncryptedKey(name))); err != nil {
			return fmt.Errorf("%s: %w", line, err)
		}
		if err := os.Chmod(in.path(EncryptedKey(name)), 0o600); err != nil {
			return err
		}
		if _, err := os.Lstat(in.path(PlainKey(name))); err == nil {
			if err := in.remove(PlainKey(name)); err != nil {
				return err
			}
		}
	} else {
		in.say("this host has no systemd-creds: the key is a plain file, readable by root only")
		if err := in.mkdirMode(KeyDir, 0o700); err != nil {
			return err
		}
		if err := in.write(PlainKey(name), []byte(key+"\n"), 0o600); err != nil {
			return err
		}
	}
	return Run(ctx, o)
}

// RemoveKey removes a provider's key, encrypted and plain, then Run.
func RemoveKey(ctx context.Context, o Options, name string) error {
	o.defaults()
	in := &installer{Options: o}
	if err := in.checkHost(); err != nil {
		return err
	}
	removed := false
	for _, p := range []string{EncryptedKey(name), PlainKey(name)} {
		if _, err := os.Lstat(in.path(p)); err == nil {
			if err := in.remove(p); err != nil {
				return err
			}
			removed = true
		}
	}
	if !removed {
		in.say("%s has no key on this host", name)
	}
	return Run(ctx, o)
}

// readKey reads one line from r, with the terminal's echo off when r is
// one. The key is never printed.
func readKey(sys System, r io.Reader, out io.Writer, name string) (string, error) {
	if f, ok := r.(*os.File); ok && sys.Terminal(f) {
		fmt.Fprintf(out, "%s key (not shown), then Enter: ", name)
		restore := sys.NoEcho(f)
		defer func() { restore(); fmt.Fprintln(out) }()
	}
	// One line: a terminal sends it on Enter, a file or pipe may end
	// without a newline.
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	key := strings.TrimSpace(line)
	if key == "" || strings.ContainsAny(key, " \t\r") {
		return "", fmt.Errorf("%s: no key on stdin (one line, no spaces)", name)
	}
	return key, nil
}

type installer struct {
	Options
}

// runQuiet runs a command whose failure does not matter (a unit already
// gone), saying so.
func (in *installer) runQuiet(ctx context.Context, name string, args ...string) {
	if err := in.run(ctx, name, args...); err != nil && !in.DryRun {
		in.say("(ignored: %v)", err)
	}
}

// remove deletes a file or a directory with its contents.
func (in *installer) remove(p string) error {
	if in.DryRun {
		in.say("would remove %s", p)
		return nil
	}
	in.say("remove %s", p)
	return os.RemoveAll(in.path(p))
}

func (in *installer) say(format string, args ...any) {
	fmt.Fprintf(in.Out, format+"\n", args...)
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
	for _, tool := range []string{"systemctl", "useradd"} {
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

func (in *installer) mkdir(dir string) error { return in.mkdirMode(dir, 0o755) }

func (in *installer) mkdirMode(dir string, mode fs.FileMode) error {
	if fi, err := os.Stat(in.path(dir)); err == nil && fi.IsDir() {
		return nil
	}
	if in.DryRun {
		in.say("would create %s/", dir)
		return nil
	}
	in.say("create %s/", dir)
	return os.MkdirAll(in.path(dir), mode)
}

// write replaces path with data when they differ, atomically, keeping the
// owner of the file it replaces.
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
// prints it and, with a search provider, a three-result search. On timeout it prints the unit's
// last journal lines and fails.
func (in *installer) waitHealthy(ctx context.Context, searches bool) error {
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
			in.System.Run(ctx, in.Out, "journalctl", "--no-pager", "-n", "40", "-u", "holocron.service")
			return fmt.Errorf("holocron did not become healthy within %s", in.Wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !searches {
		return nil
	}
	searchURL := in.HealthBase + "/v1/search?n=3&q=" + url.QueryEscape("holocron web search for AI agents")
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

func (hostSystem) RunInput(ctx context.Context, w io.Writer, input []byte, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), w, w
	return cmd.Run()
}

func (hostSystem) Terminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (hostSystem) NoEcho(f *os.File) func() {
	stty := func(arg string) {
		cmd := exec.Command("stty", arg)
		cmd.Stdin = f
		cmd.Run()
	}
	stty("-echo")
	return func() { stty("echo") }
}
