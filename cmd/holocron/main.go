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
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/tildaslashalef/holocron/internal/api"
	"github.com/tildaslashalef/holocron/internal/check"
	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/install"
	"github.com/tildaslashalef/holocron/internal/provider"
	"github.com/tildaslashalef/holocron/internal/provider/brave"
	"github.com/tildaslashalef/holocron/internal/provider/searxng"
)

// version is set at build time: -ldflags "-X main.version=X.Y.Z".
var version = "dev"

const usage = `usage: holocron <command> [arguments]

commands:
  serve [--listen ADDR] [PROVIDER FLAGS] [--log-queries=BOOL]
             the HTTP server
  search QUERY [--freshness AGE] [PROVIDER FLAGS]
             one search, the /v1/search JSON on stdout
  context QUERY [--tokens N] [--urls N] [--freshness AGE] [PROVIDER FLAGS]
             one context request, the /v1/context JSON on stdout
  fetch URL [--start N] [--max-chars N] [--text]
             one fetch, the /v1/fetch JSON on stdout; --text prints only the
             text (all of it, unless --start or --max-chars is given)
  install [--dry-run]
             set up this host: holocron's unit and SearXNG
  check [BASE_URL]
             the acceptance suite against a running server
  version    the version, toolchain and pinned SearXNG image

AGE is pd, pw, pm or py (the last day, week, month or year), or a date
range YYYY-MM-DDtoYYYY-MM-DD.

provider flags:
  --provider LIST        a provider or a chain, e.g. brave,searxng (default searxng)
  --searxng URL          SearXNG's base URL (default http://127.0.0.1:8888)
  --brave-key-file PATH  the file holding Brave's key, for brave
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one command and returns the process's exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	var err error
	switch cmd {
	case "version", "--version":
		fmt.Fprintf(stdout, "holocron %s (%s, %s/%s)\nsearxng %s (%s)\n",
			version, runtime.Version(), runtime.GOOS, runtime.GOARCH, install.SearXNGImage, install.SearXNGTag)
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
// provider, or the chain (§5.3), they name.
func providerFlags(fs *flag.FlagSet) func() (provider.Provider, error) {
	names := fs.String("provider", "searxng", "")
	base := fs.String("searxng", "http://127.0.0.1:8888", "")
	keyFile := fs.String("brave-key-file", "", "")
	return func() (provider.Provider, error) {
		var list []provider.Provider
		seen := map[string]bool{}
		for name := range strings.SplitSeq(*names, ",") {
			name = strings.TrimSpace(name)
			if seen[name] {
				return nil, usageError(fmt.Sprintf("provider %q is named twice", name))
			}
			seen[name] = true
			var p provider.Provider
			var err error
			switch name {
			case "searxng":
				p, err = searxng.New(*base)
			case "brave":
				p, err = brave.New(*keyFile, userAgent())
			default:
				return nil, usageError(fmt.Sprintf("unknown provider %q (known: searxng, brave)", name))
			}
			if err != nil {
				return nil, err
			}
			list = append(list, p)
		}
		return provider.NewChain(list...), nil
	}
}

func userAgent() string { return "holocron/" + version }

func serve(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "")
	logQueries := fs.Bool("log-queries", true, "")
	newProvider := providerFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("serve takes no arguments")
	}
	p, err := newProvider()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stdout, nil))
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	log.Info("holocron starting", "version", version, "go", runtime.Version(), "listen", ln.Addr().String(),
		"providers", p.Name(), "searxng_image", install.SearXNGImage, "log_queries", *logQueries)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	h := api.NewServer(version, p, fetch.Guarded(userAgent()), log, *logQueries)
	h.SetSearXNGImage(install.SearXNGImage)
	err = api.Serve(ctx, ln, api.NewHTTPServer(h, log))
	log.Info("holocron stopped")
	return err
}

func search(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	freshness := fs.String("freshness", "", "")
	newProvider := providerFlags(fs)
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
	p, err := newProvider()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), api.SearchBudget)
	defer cancel()
	res, err := p.Search(ctx, provider.SearchRequest{Query: query, N: api.DefaultN, Freshness: *freshness})
	if err != nil {
		return classified(err)
	}
	out, err := api.NewSearch(query, p.Name(), res, api.DefaultN)
	if err != nil {
		return classified(err)
	}
	return printJSON(stdout, out)
}

func contextCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	tokens := fs.Int("tokens", api.DefaultTokens, "")
	urls := fs.Int("urls", api.DefaultURLs, "")
	freshness := fs.String("freshness", "", "")
	newProvider := providerFlags(fs)
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
	p, err := newProvider()
	if err != nil {
		return err
	}
	req := provider.ContextRequest{Query: query, Tokens: min(*tokens, api.MaxTokens), URLs: min(*urls, api.MaxURLs), Freshness: *freshness}
	ctx, cancel := context.WithTimeout(context.Background(), api.ContextBudget)
	defer cancel()
	res, err := provider.ContextOf(ctx, p, req)
	if err != nil {
		return classified(err)
	}
	out, err := api.NewContext(query, p.Name(), res, req.Tokens)
	if err != nil {
		return classified(err)
	}
	return printJSON(stdout, out)
}

func fetchCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	start := fs.Int("start", 0, "")
	maxChars := fs.Int("max-chars", api.DefaultMaxChars, "")
	textOnly := fs.Bool("text", false, "")
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
	ctx, cancel := context.WithTimeout(context.Background(), fetch.Budget)
	defer cancel()
	page, err := fetch.Guarded(userAgent()).Fetch(ctx, pos[0], nil)
	if err != nil {
		return classified(err)
	}
	if *textOnly {
		n := len(page.Text) // bytes ≥ characters: the whole text
		if set["start"] || set["max-chars"] {
			n = *maxChars
		}
		_, err := io.WriteString(stdout, api.NewFetch(page, *start, n).Text+"\n")
		return err
	}
	return printJSON(stdout, api.NewFetch(page, *start, min(*maxChars, api.MaxMaxChars)))
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

// classified names an error by its contract code, as the server would.
func classified(err error) error {
	e := api.Classify(err)
	if e.Code == "internal" {
		return fmt.Errorf("internal: %v", err)
	}
	return e
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
