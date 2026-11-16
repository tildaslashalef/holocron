// Command holocron serves web search and page fetch for AI agents
// (SPEC.md). main only parses arguments and dispatches; the work lives in
// internal/.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/tildaslashalef/holocron/internal/api"
	"github.com/tildaslashalef/holocron/internal/check"
	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/install"
	"github.com/tildaslashalef/holocron/internal/provider"
	"github.com/tildaslashalef/holocron/internal/provider/brave"
	"github.com/tildaslashalef/holocron/internal/provider/local"
	"github.com/tildaslashalef/holocron/internal/provider/parallel"
)

// version is set at build time: -ldflags "-X main.version=X.Y.Z".
var version = "dev"

const usage = `usage: holocron <command> [arguments]

commands:
  serve [--listen ADDR] [PROVIDER FLAGS] [--log-queries=BOOL] [--dashboard=BOOL]
             the HTTP server, with its dashboard at /
  search QUERY [--freshness AGE] [PROVIDER FLAGS]
             one search, the /v1/search JSON on stdout
  context QUERY [--tokens N] [--urls N] [--freshness AGE] [PROVIDER FLAGS]
             one context request, the /v1/context JSON on stdout
  fetch URL [--q QUESTION] [--start N] [--max-chars N] [--text] [PROVIDER FLAGS]
             one fetch, the /v1/fetch JSON on stdout; --text prints only the
             text (all of it, unless --start or --max-chars is given)
  install [--dry-run]
             set up this host: holocron's unit, its keys, SearXNG's removal
  key set|rm PROVIDER [--dry-run]
             store a provider's key, read from stdin, as an encrypted systemd
             credential (or remove it), then run install
  check [BASE_URL]
             the acceptance suite against a running server
  version    the version and toolchain

AGE is pd, pw, pm or py (the last day, week, month or year), or a date
range YYYY-MM-DDtoYYYY-MM-DD.

provider flags (SPEC.md § 5):
  --search LIST             /v1/search's providers in order (default auto: parallel,brave
                            with a key)
  --context LIST            /v1/context's providers in order (default auto, as --search)
  --fetch LIST              /v1/fetch's providers in order (default auto: parallel with a
                            key, then local)
  --parallel-key-file PATH  Parallel's key (default: the parallel-key credential)
  --brave-key-file PATH     Brave's key (default: the brave-key credential)
  --parallel-search-mode M  turbo, fast, basic or advanced (default fast)
  --parallel-context-mode M as --parallel-search-mode (default advanced)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches one command and returns the process's exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	var err error
	switch cmd {
	case "version", "--version":
		fmt.Fprintf(stdout, "holocron %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve":
		err = serve(args, stdout)
	case "search":
		err = search(args, stdout)
	case "context":
		err = contextCmd(args, stdout)
	case "fetch":
		err = fetchCmd(args, stdout)
	case "install":
		err = installCmd(args, stdout)
	case "key":
		err = keyCmd(args, stdin, stdout)
	case "check":
		err = checkCmd(args, stdout)
	default:
		fmt.Fprintf(stderr, "holocron: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return 0
	case errors.As(err, new(usageError)):
		fmt.Fprintf(stderr, "holocron %s: %v\n", cmd, err)
		return 2
	}
	fmt.Fprintf(stderr, "holocron %s: %v\n", cmd, err)
	return 1
}

type usageError string

func (e usageError) Error() string { return string(e) }

// parse parses flags wherever they come among the arguments and returns
// the positional ones.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError(err.Error())
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}

// providerFlags adds the provider flags (§4) and returns what builds the
// routes they name (§5.4). Each provider is built once and shared by the
// routes that name it, so its meter and holds are one.
func providerFlags(fs *flag.FlagSet) func() (provider.Routes, error) {
	searchList := fs.String("search", "auto", "")
	contextList := fs.String("context", "auto", "")
	fetchList := fs.String("fetch", "auto", "")
	parallelKey := fs.String("parallel-key-file", "", "")
	braveKey := fs.String("brave-key-file", "", "")
	searchMode := fs.String("parallel-search-mode", parallel.ModeFast, "")
	contextMode := fs.String("parallel-context-mode", parallel.ModeAdvanced, "")
	return func() (provider.Routes, error) {
		for flagName, m := range map[string]string{"parallel-search-mode": *searchMode, "parallel-context-mode": *contextMode} {
			if !slices.Contains(parallel.Modes, m) {
				return provider.Routes{}, usageError(fmt.Sprintf("--%s must be one of %s", flagName, strings.Join(parallel.Modes, ", ")))
			}
		}
		keys := map[string]string{
			"parallel": provider.KeyFile("parallel", *parallelKey),
			"brave":    provider.KeyFile("brave", *braveKey),
		}
		built := map[string]provider.Provider{}
		get := func(name string) (provider.Provider, error) {
			if p, ok := built[name]; ok {
				return p, nil
			}
			var p provider.Provider
			var err error
			switch name {
			case "parallel":
				p, err = parallel.New(keys["parallel"], parallel.Options{SearchMode: *searchMode, ContextMode: *contextMode, UserAgent: userAgent()})
			case "brave":
				p, err = brave.New(keys["brave"], userAgent())
			case "local":
				p = local.New(fetch.Guarded(userAgent()), api.ExtractSlots, api.SlotWait)
			}
			if err != nil {
				return nil, err
			}
			built[name] = p
			return p, nil
		}
		chain := func(flagName, list string, known []string, auto []string) (*provider.Chain, error) {
			var names []string
			if list == "auto" {
				for _, n := range auto {
					if n == "local" || keys[n] != "" {
						names = append(names, n)
					}
				}
			} else if strings.TrimSpace(list) != "" && list != "none" {
				for n := range strings.SplitSeq(list, ",") {
					n = strings.TrimSpace(n)
					if !slices.Contains(known, n) {
						return nil, usageError(fmt.Sprintf("--%s: unknown provider %q (known: %s)", flagName, n, strings.Join(known, ", ")))
					}
					if slices.Contains(names, n) {
						return nil, usageError(fmt.Sprintf("--%s: provider %q is named twice", flagName, n))
					}
					names = append(names, n)
				}
			}
			var ps []provider.Provider
			for _, n := range names {
				p, err := get(n)
				if err != nil {
					return nil, err
				}
				ps = append(ps, p)
			}
			return provider.NewChain(ps...), nil
		}
		var r provider.Routes
		var err error
		if r.Search, err = chain("search", *searchList, []string{"parallel", "brave"}, []string{"parallel", "brave"}); err != nil {
			return r, err
		}
		if r.Context, err = chain("context", *contextList, []string{"parallel", "brave"}, []string{"parallel", "brave"}); err != nil {
			return r, err
		}
		if r.Fetch, err = chain("fetch", *fetchList, []string{"parallel", "local"}, []string{"parallel", "local"}); err != nil {
			return r, err
		}
		return r, nil
	}
}

func userAgent() string { return "holocron/" + version }

func serve(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "")
	logQueries := fs.Bool("log-queries", true, "")
	dashboard := fs.Bool("dashboard", true, "")
	newRoutes := providerFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("serve takes no arguments")
	}
	routes, err := newRoutes()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stdout, nil))
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	attrs := []any{"version", version, "go", runtime.Version(), "listen", ln.Addr().String(),
		"search", routes.Search.Name(), "context", routes.Context.Name(), "fetch", routes.Fetch.Name(),
		"log_queries", *logQueries, "dashboard", *dashboard}
	for _, p := range routes.Providers() {
		if pp, ok := p.(*parallel.Provider); ok {
			sm, cm := pp.Modes()
			attrs = append(attrs, "parallel_search_mode", sm, "parallel_context_mode", cm)
		}
	}
	log.Info("holocron starting", attrs...)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	h := api.NewServer(api.Options{Version: version, Routes: routes, Log: log, LogQueries: *logQueries, Dashboard: *dashboard})
	err = api.Serve(ctx, ln, api.NewHTTPServer(h, log))
	log.Info("holocron stopped")
	return err
}

// one runs one request against a server built from the flags, as a client
// would, and prints its JSON: the command line and the server answer the
// same way.
func one(routes provider.Routes, target string, stdout io.Writer) error {
	return oneOn(api.NewServer(api.Options{Version: version, Routes: routes, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}), target, stdout)
}

// oneOn is one against srv, whose cache pages a document from one fetch.
func oneOn(srv *api.Server, target string, stdout io.Writer) error {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("User-Agent", userAgent()+" (command line)")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		var eb api.ErrorBody
		if json.Unmarshal(rec.Body.Bytes(), &eb) == nil && eb.Error.Code != "" {
			return fmt.Errorf("%s: %s", eb.Error.Code, eb.Error.Message)
		}
		return fmt.Errorf("status %d", rec.Code)
	}
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		return err
	}
	return printJSON(stdout, v)
}

func search(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	freshness := fs.String("freshness", "", "")
	newRoutes := providerFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" {
		return usageError("search needs a QUERY")
	}
	if *freshness != "" && !api.ValidFreshness(*freshness) {
		return usageError("--freshness must be pd, pw, pm, py or YYYY-MM-DDtoYYYY-MM-DD")
	}
	routes, err := newRoutes()
	if err != nil {
		return err
	}
	return one(routes, "/v1/search?"+values("q", query, "freshness", *freshness), stdout)
}

func contextCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	tokens := fs.Int("tokens", api.DefaultTokens, "")
	urls := fs.Int("urls", api.DefaultURLs, "")
	freshness := fs.String("freshness", "", "")
	newRoutes := providerFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" {
		return usageError("context needs a QUERY")
	}
	if *tokens < api.MinTokens || *urls < 1 {
		return usageError(fmt.Sprintf("--tokens must be %d or more and --urls 1 or more", api.MinTokens))
	}
	if *freshness != "" && !api.ValidFreshness(*freshness) {
		return usageError("--freshness must be pd, pw, pm, py or YYYY-MM-DDtoYYYY-MM-DD")
	}
	routes, err := newRoutes()
	if err != nil {
		return err
	}
	return one(routes, "/v1/context?"+values("q", query, "tokens", fmt.Sprint(*tokens), "urls", fmt.Sprint(*urls), "freshness", *freshness), stdout)
}

func fetchCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	start := fs.Int("start", 0, "")
	maxChars := fs.Int("max-chars", api.DefaultMaxChars, "")
	focus := fs.String("q", "", "")
	textOnly := fs.Bool("text", false, "")
	newRoutes := providerFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("fetch needs one URL")
	}
	if *start < 0 || *maxChars < 1 {
		return usageError("--start must be 0 or more and --max-chars 1 or more")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	routes, err := newRoutes()
	if err != nil {
		return err
	}
	if !*textOnly {
		return one(routes, "/v1/fetch?"+values("url", pos[0], "q", *focus, "start", fmt.Sprint(*start), "max_chars", fmt.Sprint(*maxChars)), stdout)
	}
	// --text pages through the whole text unless a window was asked for.
	n := api.MaxMaxChars
	if set["max-chars"] {
		n = min(*maxChars, api.MaxMaxChars)
	}
	srv := api.NewServer(api.Options{Version: version, Routes: routes, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	at := *start
	for {
		var buf strings.Builder
		if err := oneOn(srv, "/v1/fetch?"+values("url", pos[0], "q", *focus, "start", fmt.Sprint(at), "max_chars", fmt.Sprint(n)), &buf); err != nil {
			return err
		}
		var f api.Fetch
		if err := json.Unmarshal([]byte(buf.String()), &f); err != nil {
			return err
		}
		if _, err := io.WriteString(stdout, f.Text); err != nil {
			return err
		}
		at += len([]rune(f.Text))
		if set["start"] || set["max-chars"] || f.Text == "" || at >= f.Chars {
			_, err := io.WriteString(stdout, "\n")
			return err
		}
	}
}

func installCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("install takes no arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return install.Run(ctx, install.Options{DryRun: *dryRun, Out: stdout})
}

func keyCmd(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || (pos[0] != "set" && pos[0] != "rm") {
		return usageError("key needs set or rm, and a provider: holocron key set parallel")
	}
	if !slices.Contains(install.KeyProviders, pos[1]) {
		return usageError(fmt.Sprintf("unknown provider %q (keys: %s)", pos[1], strings.Join(install.KeyProviders, ", ")))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	o := install.Options{DryRun: *dryRun, Out: stdout}
	if pos[0] == "rm" {
		return install.RemoveKey(ctx, o, pos[1])
	}
	return install.SetKey(ctx, o, pos[1], stdin)
}

func checkCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	base := "http://127.0.0.1:8080"
	switch len(pos) {
	case 0:
	case 1:
		base = pos[0]
	default:
		return usageError("check takes at most one BASE_URL")
	}
	return check.Run(context.Background(), base, "holocron-check/"+version, stdout)
}

// values encodes name, value pairs, leaving out empty values.
func values(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	return v.Encode()
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
