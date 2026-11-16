# holocron — SPEC

This is the single source of truth for holocron's behaviour, its
contract and how it is installed. Facts are marked with their source and
date; anything marked *(verify)* is read or measured before it becomes
design.

## 1. What holocron is

One static Go binary that gives AI agents (coding agents among them) two
things over HTTP JSON: **search the web** and **fetch a page as clean
Markdown text**.

- `/v1/search` asks a **search provider** and returns normalized results.
  The providers are the Parallel Search API and the Brave Search API,
  each with its own key, alone or chained (§5).
- `/v1/context` asks a provider for passages from several pages that
  answer a query, for an agent to read in one call (§3.5).
- `/v1/fetch` reads one public URL as text: through Parallel's Extract
  API when it has a key, else, or when Parallel cannot read the page,
  through holocron's own fetcher, behind an **address guard** (§6), with
  its own extraction (§7).
- `holocron install` sets a Linux host up: holocron's systemd unit with
  the keys it finds as systemd credentials (§9). `holocron key set`
  stores a key (§9.4).

holocron was made for [nuclis](https://github.com/tildaslashalef/nuclis)
(the agent in `nuclis chat`, and the hosted tools of `nuclis serve`); any
other client on a trusted network that speaks §3 can use it. Nothing in
holocron is specific to one deployment; the README describes the
recommended host.

### Non-goals

- Public exposure, accounts or authentication (v1 is for a trusted LAN or
  tailnet; a token is a v2 change).
- JavaScript rendering, crawling, PDFs and images in holocron's own
  fetcher (§5.3). An extraction provider reads what it reads: Parallel
  reads PDFs (§12).
- A persistent or shared cache: §8.1's is in memory, one per process.
- A general HTTP proxy: `GET` of pages for reading, nothing else.

## 2. Principles

- **The contract is the product.** Providers and extraction libraries can
  change behind `/v1/` without a client noticing.
- **Every limit is the server's.** A client may ask for less, never more; a
  larger request is clamped, not refused.
- **Failures are results.** Every failure a client can meet has a code in
  §3's table; nothing ends in a dropped connection.
- **The fetcher never reaches a non-public address**, whatever the URL,
  the DNS answer or the redirect says (§6).
- Few dependencies, each named in §11; the standard library otherwise.

## 3. The contract (v1) — authoritative

A breaking change is `/v2/`, served beside `/v1/` until clients move.
Adding a field to a response is not breaking; clients ignore fields they
do not know. (0.2.0 revised `/v1/` in place, before any client depended
on the fields it replaced: `engine`, `extra_snippets`, `engines_failed`
and the code `no_engines` are gone.)

### 3.1 General

- `GET` only; query parameters URL-encoded UTF-8; responses
  `application/json; charset=utf-8`, `Cache-Control: no-store`.
- Search, context and fetch answers may come from holocron's own cache
  (§8.1); the body is the same either way. Every such answer whose
  parameters passed validation carries `X-Search-Cache: hit`, `miss` or
  `stale`, and a `hit` or `stale` one `Age: <seconds>` since holocron
  stored it. A request with `Cache-Control: no-cache` skips the lookup
  and refreshes the entry. (`no-store` on the answer still tells clients
  and proxies not to keep it.)
- No authentication in v1. Clients send a `User-Agent` naming themselves
  (`nuclis/<version>`); holocron logs it.
- Errors are non-2xx with one body shape:

  ```json
  {"error": {"code": "blocked_address", "message": "example.test resolves to 192.168.1.20, not a public address"}}
  ```

  | Code | Status | When |
  | --- | --- | --- |
  | `bad_request` | 400 | a missing or invalid parameter (empty query, a URL that is not `http`/`https`, too long, a port other than 80/443, credentials in the URL, a fetch's `q` over 400 bytes), or a query the provider refuses (Brave: over 600 characters or 75 words) |
  | `not_found` | 404 | an unknown path (`/` and `/v1/stats` too, with `--dashboard=false`) |
  | `method_not_allowed` | 405 | any method but `GET` |
  | `not_available` | 501 | the endpoint's chain has no provider for it (`--search none`, or a host with no key, §9) |
  | `blocked_address` | 403 | the URL, or a redirect, resolves to a non-public address (§6) |
  | `unsupported_content` | 415 | the local fetcher got a type §3.3 does not extract |
  | `upstream_failed` | 502 | the remote site failed (DNS, connection, TLS, a 4xx/5xx — its status in `message`), or more than 5 redirects; from Parallel, a page that answered 404 or 410 |
  | `provider_unavailable` | 503 | every provider of the endpoint's chain failed: unreachable, held back after a refused key or used-up credit, rate-limited, an error answer, or for a fetch a page it could not read (§5.4) |
  | `busy` | 503 | every request slot, or every extraction slot of the local fetcher, stayed taken for 2 s (§8) |
  | `upstream_timeout` | 504 | the remote site or the provider exceeded the budget |
  | `internal` | 500 | a bug in holocron; the log line has the detail |

### 3.2 `GET /v1/search?q=<query>&n=<count>&freshness=<age>`

- `q`: 1–400 bytes after trimming. `n`: 1–10, default 8.
- `freshness` (optional): only pages from the last day (`pd`), week
  (`pw`), month (`pm`) or year (`py`), or a date range
  `YYYY-MM-DDtoYYYY-MM-DD` (from ≤ to); anything else is `bad_request`.
  Brave filters by the page's own date, ranges included. Parallel takes
  a start date only: holocron applies a range's end to each result's
  publish date, and keeps a result that has none (§5.1).
- Budget: 10 s for the whole request.
- `200`:

  ```json
  {
    "query": "zig std.http.Client proxy",
    "provider": "parallel",
    "results": [
      {
        "title": "std.http.Client — Zig documentation",
        "url": "https://ziglang.org/documentation/…",
        "snippet": "…",
        "snippets": ["…another passage…"],
        "published": "2026-09-01"
      }
    ],
    "altered_query": null,
    "providers_failed": []
  }
  ```

- `provider`: the provider that answered (§5).
- `results`: at most `n`, deduplicated by URL, in the provider's rank
  order; only `http`/`https` URLs; `title` ≤ 200 and `snippet` ≤ 500
  characters, whitespace collapsed, cut at a word with `…` appended;
  `snippets` more passages from the page, at most 5, each within
  `snippet`'s bounds and none repeating it, empty when the provider gives
  none; `published` an ISO date or `null`.
- `altered_query`: the query the provider searched when it corrected
  this one's spelling (Brave's `query.altered`), else `null`: how a client
  learns that its words were not the ones searched. Parallel gives none.
- Zero results is a `200` with an empty list.
- `providers_failed`: the providers of the chain (§5.4) that failed
  before the one that answered, in order, empty when none: how a client
  learns the answer is not from its first choice.

### 3.3 `GET /v1/fetch?url=<url>&q=<question>&start=<offset>&max_chars=<n>`

- `url`: `http` or `https`, ≤ 2,048 bytes, no credentials, ports 80 and
  443 only. `start`: a character offset into the extracted text, default 0.
  `max_chars`: 1–100,000, default 20,000.
- `q` (optional): a question, ≤ 400 bytes after trimming. A provider that
  can focus (Parallel) answers with the parts of the page relevant to it
  and `focused: true`; one that cannot (local) answers with the whole
  page and `focused: false`. `start` and `max_chars` page through either.
- The fetch chain (§5.4): Parallel's Extract API when it has a key, then
  holocron's own fetcher, `local` (§5.3). Parallel's failure to read a
  page moves on to `local`; a page that does not exist (404, 410) does
  not.
- Budget: 25 s for everything — for `local`, DNS, connect, every redirect,
  the body and the extraction. Parallel gets at most 60% of it while
  `local` follows (§5.4). `local` follows at most 5 redirects, each hop
  checked by the guard (§6), and reads the raw body to at most 5 MiB
  after decompression (more is cut, and `truncated` says so).
- Types `local` extracts: `text/html` and `application/xhtml+xml` (to
  Markdown, §7), `text/plain`, `text/markdown`, `application/json`,
  `text/xml` and `application/xml` (as text). A missing `Content-Type` is
  sniffed from the first 512 bytes. Anything else is
  `unsupported_content`, decided from the headers before the body is
  read. Parallel reads what it reads, PDFs among them.
- `local` asks like a browser (`Accept:
  text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8`): a
  site that answers `text/markdown` with its raw source (react.dev sends
  its MDX, components and all) sends the HTML §7 extracts instead.
- The extracted document is cached for 15 minutes, a fetch with `q` for
  30 (§8.1): `start` is how a client pages through a long document, and
  paging costs one upstream fetch.
- `200`:

  ```json
  {
    "url": "https://example.org/a",
    "final_url": "https://example.org/a/",
    "provider": "local",
    "providers_failed": [],
    "status": 200,
    "content_type": "text/html",
    "title": "…",
    "published": null,
    "focused": false,
    "text": "# Heading\n\nParagraph…",
    "start": 0,
    "chars": 48211,
    "truncated": true
  }
  ```

- `provider`, `providers_failed`: as §3.2.
- `status`, `content_type`: the final response's HTTP status and media
  type, or `null` when the provider does not say (Parallel does not).
  `published`: an ISO date or `null` (Parallel's publish date; `local`
  gives none).
- `chars`: the length of the whole extracted text. `truncated`: the
  returned text ends before `chars`, or the source was cut (`local`'s
  5 MiB, Parallel's 500,000 characters). Lengths and offsets count
  Unicode code points, not bytes. A `start` at or past `chars` returns
  empty `text`.
- `text` is Markdown: from `local`, headings `#`, list items `- `, fenced
  code blocks, tables, links `[text](absolute url)`, never HTML (§7);
  from Parallel, its own extraction. A page that renders its content with
  JavaScript yields little from `local`; fetch its source instead.
- `title`: the page's title, ≤ 200 characters; empty for plain types.
- A site behind a bot challenge answers `local` with `upstream_failed`,
  its message saying so: Cloudflare's (`cf-mitigated: challenge`) wants a
  browser to run its JavaScript, and a browser's `User-Agent` does not
  pass it either (Stack Overflow and AP News, measured 2026-10-16).
  holocron does not try to get past a bot check; the message points to
  `/v1/context`, whose provider has the page's text from its own index.
  Parallel fetches from its own network, and a page it reads never
  reaches `local`.

### 3.4 `GET /v1/health`

`200` always, while the process answers:

```json
{
  "version": "0.2.0",
  "providers": {"parallel": "ok", "brave": "ok", "local": "ok"},
  "routes": {
    "/v1/search": ["parallel", "brave"],
    "/v1/context": ["parallel", "brave"],
    "/v1/fetch": ["parallel", "local"]
  },
  "endpoints": ["/v1/search", "/v1/context", "/v1/fetch"],
  "billed": {"parallel": 37, "brave": 2},
  "cache": {"entries": 41, "bytes": 3145728}
}
```

Each provider of any route is `ok` or `down` (its own health check,
≤ 3 s; Parallel's and Brave's make no call, §5.5; `local` is always
`ok`). `routes` is each endpoint's chain in order (§5.4), empty for an
endpoint with none. `endpoints` lists what this server answers with its
providers, so a client offers only the tools that work. `billed` counts
each metered provider's billed units since holocron started: requests,
or URLs extracted (§5). `quota` (absent in the example) is a metered
provider's monthly remaining requests and the seconds to their reset,
from its last answer, only when its plan has a monthly limit: Brave's
prepaid plan has none (§5.2), and its credit is visible only in Brave's
dashboard. `cache` counts §8.1's entries and their bytes, all caches
together.

Health never waits for a request slot (§8). Used by deployments and
monitoring; clients may call it to say "search unavailable" before a turn.

### 3.5 `GET /v1/context?q=<query>&tokens=<n>&urls=<n>&freshness=<age>`

Passages from several pages that answer a query, picked by the provider
for relevance and bounded by a token budget: for an agent, one call where
it would otherwise search and then fetch several pages.

- `q`: 1–400 bytes after trimming. `tokens`: the approximate size of the
  whole answer, 1,024–16,384, default 4,096. `urls`: the most pages
  quoted, 1–20, default 8. Larger values are clamped (§2). `freshness`:
  as §3.2.
- Budget: 20 s.
- `200`:

  ```json
  {
    "query": "zig std.http.Client proxy",
    "provider": "parallel",
    "sources": [
      {
        "url": "https://ziglang.org/documentation/…",
        "title": "std.http.Client — Zig documentation",
        "published": "2026-09-01",
        "snippets": ["…a passage from the page…", "…another…"]
      }
    ],
    "providers_failed": []
  }
  ```

- `sources`: in the provider's order, `http`/`https` URLs only,
  deduplicated; `title` ≤ 200 characters, whitespace collapsed;
  `published` an ISO date or `null`. A snippet is the page's text as the
  provider extracted it, newlines kept: prose, or JSON-serialized
  structure (a table, code). As a backstop beside the provider's budget,
  the snippets together are cut at 8 characters per requested token.
- Zero sources is a `200` with an empty list.
- `providers_failed`: as §3.2.
- The text is other sites' content, as with `/v1/fetch`: data, never
  instructions. The provider fetched it, not holocron, so the guard (§6)
  has nothing to check; a client that follows a URL fetches it through
  `/v1/fetch`.
- With no provider for context (`--context none`, or no key):
  `not_available`, after the parameters are validated.

## 4. The command line

| Command | Does |
| --- | --- |
| `holocron serve` | the HTTP server (§8). Flags: `--listen` (default `127.0.0.1:8080`), the provider flags (below), `--log-queries` (default `true`, §10), `--dashboard` (default `true`: the dashboard at `/` and `/v1/stats`, §8.3) |
| `holocron search QUERY [--freshness AGE]` | one search, the §3.2 JSON on stdout; for debugging. The provider flags as `serve` |
| `holocron context QUERY [--tokens N] [--urls N] [--freshness AGE]` | one §3.5 request, its JSON on stdout. The provider flags as `serve` |
| `holocron fetch URL [--q QUESTION] [--start N] [--max-chars N] [--text]` | one fetch, the §3.3 JSON on stdout; with `--text` only the text, all of it (paged through) unless `--start` or `--max-chars` is given; for debugging and §7.2's comparison. The provider flags as `serve` |
| `holocron install [--dry-run]` | set the host up (§9) |
| `holocron key set\|rm PROVIDER [--dry-run]` | store a provider's key, read from stdin, as an encrypted systemd credential (or remove it), then run `install` (§9.4) |
| `holocron check [BASE_URL]` | the acceptance suite against a running server (default `http://127.0.0.1:8080`, §12) |
| `holocron version` | the version, the Go toolchain, the OS and architecture |

The provider flags (§5):

| Flag | Default | Is |
| --- | --- | --- |
| `--search LIST` | `auto` | `/v1/search`'s providers in order: `auto`, `none`, or a comma-separated list of `parallel`, `brave` |
| `--context LIST` | `auto` | `/v1/context`'s, as `--search` |
| `--fetch LIST` | `auto` | `/v1/fetch`'s, of `parallel`, `local` |
| `--parallel-key-file PATH` | the `parallel-key` credential | Parallel's key (§5.5) |
| `--brave-key-file PATH` | the `brave-key` credential | Brave's key (§5.5) |
| `--parallel-search-mode M` | `fast` | `turbo`, `fast`, `basic` or `advanced` (§5.1) |
| `--parallel-context-mode M` | `advanced` | as `--parallel-search-mode` |

`auto` is `parallel,brave` for search and context, each only when its key
is found, and `parallel,local` for fetch, Parallel only with its key. A
list that names a provider whose key is not found stops the command; an
unknown name, or one named twice, is a usage error. Each provider is built
once and shared by the routes that name it, so its bill and its holds
(§5.5) are one.

`search`, `context` and `fetch` answer through the same server code as
`serve`, in process, so the command line and the server answer the same
way. `main` only parses arguments and dispatches. Expected failures exit
non-zero with one line on stderr; nothing panics on input.

## 5. Providers

```go
type Provider interface {
    Name() string
    Health(ctx context.Context) error
}

// What a provider answers is the capability interfaces it implements:
type Searcher interface {
    Search(ctx context.Context, r SearchRequest) (Results, error) // query, n, freshness
}
type Contexter interface {
    Context(ctx context.Context, r ContextRequest) (Context, error) // query, tokens, urls, freshness
}
type Extractor interface {
    Extract(ctx context.Context, r ExtractRequest) (*Page, error) // url, focus
}
```

| Provider | Searcher | Contexter | Extractor | Key |
| --- | --- | --- | --- | --- |
| `parallel` (§5.1) | yes | yes | yes | yes |
| `brave` (§5.2) | yes | yes | no | yes |
| `local` (§5.3) | no | no | yes | no |

`Results`, `Context` and `Page` carry the normalized items; the HTTP
layer applies §3.2's, §3.3's and §3.5's bounds once, for every provider,
and the chain (§5.4) adds who answered and who failed. A provider's
failures are one of: unavailable (`provider_unavailable`, with §10's
reason; the chain moves on), a timeout (`upstream_timeout`), a query it
refuses (`bad_request`), or for an extractor a final answer about the
page (`upstream_failed`, `blocked_address`, `unsupported_content`,
`busy`). A metered provider also reports what it billed (§5.5) and,
when its plan has a monthly limit, its quota (§3.4).

The prices holocron counts its bill with (§5.5), in US dollars:

| Provider | Unit | Price |
| --- | --- | --- |
| `parallel` search, mode `turbo` or `fast` | a request, 10 results included | 1 per 1,000, and 0.001 per result beyond 10 |
| `parallel` search, mode `basic` or `advanced` | a request, 10 results included | 5 per 1,000, and 0.001 per result beyond 10 |
| `parallel` extract | a URL | 1 per 1,000 |
| `brave` web search or LLM context | a successful request | 5 per 1,000 |
| `local` | | nothing |

### 5.1 `parallel`

- **The plan** (Parallel's pricing page, read 2026-11-16): an API key from
  Parallel's platform; the Search API priced by mode and the Extract API
  per URL, as §5's table. §8.1's cache is what keeps the bill down: a
  repeated query within 30 minutes costs nothing.
- **Modes** of the Search API, by latency per request (measured
  2026-11-16): `turbo` about 0.5 s, `fast` 0.8 s, `basic` 1.3 s,
  `advanced` 3 s. `/v1/search` uses `fast` and `/v1/context` `advanced`
  by default (§4's flags): a search wants speed, a context answer the
  better passages.
- **Search**: `POST https://api.parallel.ai/v1/search` with a JSON body:
  `objective` the query, `search_queries` the query cut at a word to 200
  characters (Parallel's bound), `mode`, `max_chars_total` 3,000 per
  requested result, and `advanced_settings.max_results` `n` (holocron
  asks 10, §8.1); headers `x-api-key`, `Content-Type` and `Accept:
  application/json`, holocron's `User-Agent`. `results[].url` → `url`,
  `.title` → `title`, `.publish_date` → `published` (its date);
  `.excerpts[]` are split into paragraphs (at blank lines, a leading `#`
  dropped, `...` and `[...]` dropped, paragraphs under 40 characters
  dropped when longer ones exist): the first → `snippet`, the rest →
  `snippets` (§3.2 keeps 5). `altered_query` is always `null`.
- **Context** (§3.5): the same API in the context mode, `max_results`
  `urls`, `max_chars_total` 4 characters per requested token;
  `results[]` → `sources`, their `excerpts` → `snippets` as they are.
- **Freshness**: Parallel filters only from a date,
  `advanced_settings.source_policy.after_date`: `pd`, `pw`, `pm`, `py` send
  today (UTC) less a day, 7 days, a month, a year; a range sends its
  start, and holocron drops a result whose `publish_date` is after its
  end (a result without one is kept).
- **Extract** (§3.3): `POST https://api.parallel.ai/v1/extract` with
  `urls` the one URL, the same headers. The whole page:
  `advanced_settings.full_content.max_chars_per_result` 500,000;
  `results[0].full_content` → `text`, `truncated` when it reaches
  500,000 characters. With `q`: `objective` the question,
  `max_chars_total` 8,000, `full_content` `false`, excerpts of at most
  8,000 characters; the excerpts joined by blank lines → `text`, and
  `focused: true`. `results[0].url` → `final_url`, `.title`, `.publish_date`
  → `published`; `status` and `content_type` are `null`.
- **The guard before Extract**: Parallel fetches from its own network, so
  holocron would otherwise hand it any target. Before sending, the URL
  passes §6's step 1 again, and its host is resolved here and refused
  (`blocked_address`) unless every address is public (`guard.CheckHost`):
  a private target never leaves this host, and is refused as `local`
  refuses it. A failed lookup is `upstream_failed`. A redirect Parallel
  follows happens on Parallel's network, outside the guard; it cannot
  reach this host's private addresses (§12's redirect check).
- **Failures**:

  | Parallel answers | holocron answers |
  | --- | --- |
  | 429 with `Retry-After` ≤ 1 s | one retry after it (at least 200 ms), within the budget; then `provider_unavailable` (`rate_limited`) |
  | 402 | `provider_unavailable` "parallel: credit used up"; no call for an hour (`quota`) |
  | 401, 403 | `provider_unavailable` "parallel refused the key"; no call for 10 minutes (`key_refused`) |
  | another status, connection, TLS, an unreadable answer | `provider_unavailable` |
  | Extract: the URL in `errors[]` with `http_status_code` 404 or 410 | `upstream_failed`: the page does not exist, and the chain stops |
  | Extract: any other entry in `errors[]`, neither a result nor an error, or empty text | `provider_unavailable` (`unreadable`): the chain moves on to `local` |
  | the budget passes | `upstream_timeout` |

  An error's message appends Parallel's: its body is either
  `{"error": {"message", "detail"}}` (documented) or `{"code",
  "message"}` (a refused key, measured 2026-11-16).
- **Health makes no call**: every call is billed. Parallel is `ok`
  unless a refused key or used-up credit holds its calls back (§5.5).
- **The bill** (§5.5): a search is one request at its mode's price, plus
  each result beyond 10; an extraction is the URLs in the answer's
  `usage` (its `sku_extract` count), else the results returned. Only a
  200 is counted.
- **In a chain**: while another member follows it, Parallel gets at most
  60% of the remaining budget (§5.4).

### 5.2 `brave`

A second index with its own key: asked after Parallel when Parallel
cannot answer, or alone.

- **The plan** (Brave's dashboard, 2026-10-16): the Brave Search API
  needs an API key from `https://api-dashboard.search.brave.com`. Its
  *Search* plan is prepaid: US$5 per 1,000 requests, with US$5 of free
  credit every month (about 1,000 requests a month), capacity 50 requests
  a second; web search, LLM context, news, videos and images. §8.1's
  cache is what keeps the credit: a repeated query within 30 minutes
  costs nothing. The dashboard can also cap spend with a monthly usage
  limit; what Brave answers at the cap is not yet seen, and a 402 or a
  quota 429 holds Brave back so the chain answers without it (below).
- **Search**: `GET https://api.search.brave.com/res/v1/web/search` with
  `q`, `count` `n` (at most 20; holocron asks 10, §8.1), `safesearch=off`,
  `text_decorations=false` (no highlight markers), `result_filter=web`
  (no news, video or FAQ blocks), `extra_snippets=true`, and `freshness`
  when the request has one; headers `X-Subscription-Token`, `Accept:
  application/json`, holocron's `User-Agent`, and `Cache-Control:
  no-cache` when the client sent it (§8.1), since Brave otherwise answers
  from its own cache. `country` and `search_lang` are not sent: Brave's
  defaults (`US`, `en`) suit English technical queries; revisited if
  answers look too US-centred. `web.results[].title` → `title`, `.url` →
  `url`, `.description` → `snippet` (HTML entities decoded), `.page_age`
  (an ISO date-time) → `published` (its date), `.extra_snippets[]` →
  `snippets`; `query.altered`, when it differs from the query, →
  `altered_query`.
- **Context** (§3.5): `GET …/res/v1/llm/context` with `q`, `count=20`,
  `maximum_number_of_urls` and `maximum_number_of_tokens` from the
  request, `enable_local=false` (no location results), `freshness` when
  the request has one, the same headers.
  `grounding.generic[]` (`url`, `title`, `snippets[]`) → `sources`;
  `published` from `sources[url].age`, its `YYYY-MM-DD` position.
  `context_threshold_mode` is left to Brave's calibrated default.
- **Failures**:

  | Brave answers | holocron answers |
  | --- | --- |
  | 429 with a second still to wait (`X-RateLimit-Reset` first value ≤ 1) | one retry after it, within the budget; then `provider_unavailable` (`rate_limited`) |
  | 429 with the month's quota at 0, or 402 | `provider_unavailable` "brave: quota or credit used up"; no call until the reset Brave gave (1 h when it gave none) (`quota`) |
  | 401, 403, or 422 with `error.code` `SUBSCRIPTION_TOKEN_INVALID` (what a wrong key gets, measured 2026-10-16) | `provider_unavailable` "brave refused the key"; no call for 10 minutes (`key_refused`) |
  | 400 or 422 `VALIDATION` naming `q` (`meta.errors[].loc` `["query", "q"]`) | `bad_request` with Brave's messages |
  | 400 or 422 about another parameter (holocron's request) | `provider_unavailable` |
  | 5xx, connection, TLS, an unreadable answer | `provider_unavailable` |
  | the budget passes | `upstream_timeout` |

  Brave bills only successful requests, so a failure costs nothing. At
  50 requests a second against §8's 32 slots, holocron does not pace its
  requests.
- **Health makes no call**: every call spends credit, and monitoring
  polls `/v1/health`. Brave is `ok` unless a refused key or a used-up
  quota holds its calls back. Every answer's `X-RateLimit-Remaining` and
  `X-RateLimit-Reset` (`per-second, per-month` values) update §3.4's
  `quota` when `X-RateLimit-Limit`'s monthly value is above 0. A
  prepaid plan answers (measured 2026-10-16) `X-RateLimit-Limit: 50, 0`,
  `-Policy: 50;w=1, 0;w=2678400`, `-Remaining: 49, 0`, `-Reset: 1,
  1304852`: no monthly limit (0 is unlimited), so no `quota`, and no
  header carries the credit left. What holocron can count exactly is
  what Brave bills, its successful answers (readable or not): §3.4's
  `billed` and §10's `brave_billed`, at US$0.005 each.
- **The key**: §5.5's, sent only as `X-Subscription-Token`.
- **In a chain**: while another member follows it, Brave gets at most 60%
  of the remaining budget (§5.4).
- **Not pinned**: Brave's `Api-Version` header. Without it Brave answers
  with its latest version, which may change the answer's shape; the
  first deployment's `holocron check` (§12) is the test, and pinning a
  version is revisited if one breaks it.
- **Not adopted**, from the same API:

  | Endpoint | Why not |
  | --- | --- |
  | News search (`/v1/news/search`) | Later, as `/v1/search?kind=news`, when an agent needs current events: the same result shape, with `freshness` (`pd`, `pw`, `pm`, `py`). |
  | Place search (`/v1/local/place_search`) | Points of interest by coordinates or a place name, for a person's location, which holocron's agents do not have. |
  | Local POIs (`/v1/local/pois`), POI descriptions (`/v1/local/descriptions`) | They take ids from a web search's location results, valid 8 hours: only meaningful with place search. |
  | Rich search (`/v1/web/rich`) | Weather, stocks, sports, currency and calculator cards, through a callback key from a web search with `enable_rich_callback`; the plan includes it, but no agent here needs it. |

#### 5.2.1 Brave's API reference (read 2026-10-16)

Each page is also Markdown with `index.html.md` appended.

| Page | URL |
| --- | --- |
| LLM Context | https://api-dashboard.search.brave.com/api-reference/ai/llm_context/get/index.html.md |
| Web search | https://api-dashboard.search.brave.com/api-reference/web/search/get/index.html.md |
| News search | https://api-dashboard.search.brave.com/api-reference/news/news_search/get/index.html.md |
| Place search | https://api-dashboard.search.brave.com/api-reference/web/place_search/index.html.md |
| Local POIs | https://api-dashboard.search.brave.com/api-reference/web/local_pois/index.html.md |
| POI descriptions | https://api-dashboard.search.brave.com/api-reference/web/poi_descriptions/index.html.md |
| Rich search | https://api-dashboard.search.brave.com/api-reference/web/rich_search/index.html.md |
| LLM Context guide (budgets, threshold modes, changelog) | https://api-dashboard.search.brave.com/documentation/services/llm-context/index.html.md |
| Rate limiting | https://api-dashboard.search.brave.com/documentation/guides/rate-limiting/index.html.md |
| Versioning | https://api-dashboard.search.brave.com/documentation/guides/versioning/index.html.md |
| Index of all pages | https://api-dashboard.search.brave.com/llms.txt |

What this design relies on:

- Base URL `https://api.search.brave.com/res`; auth header
  `X-Subscription-Token`; errors `{"type", "error": {"id", "status",
  "code", "detail", "meta"}, "time"}`, statuses 400, 403, 404, 422 and
  429 across the endpoints. Every endpoint takes `Cache-Control:
  no-cache` ("best effort") and `Api-Version: YYYY-MM-DD` (latest when
  absent).
- Web search: `q` ≤ 600 characters and 75 words; `count` 1–20 (20),
  `offset` 0–9; `safesearch` `off`/`moderate`/`strict` (`moderate`);
  `text_decorations` (`true`); `result_filter` (`web`, `news`,
  `discussions`, `faq`, `infobox`, `videos`, `locations`, `query`,
  `summarizer`); `extra_snippets` (up to 5 more per result); `goggles`
  (custom re-ranking, up to 3); `freshness`; `spellcheck` (the altered
  query in `query.altered`); `operators`. `web.results[]` has `title`,
  `url`, `description`, `page_age`, `age`, `extra_snippets`, `language`
  and much structured data.
- LLM Context (launched 2026-02-06; a new extraction pipeline by default
  since 2026-07-31, `Api-Version: 2026-02-06` keeps the old one): GET or
  POST `/v1/llm/context`; `q` as web search; `count` 1–50 (20);
  `maximum_number_of_urls` 1–50 (20); `maximum_number_of_tokens`
  1,024–32,768 (8,192); `maximum_number_of_tokens_per_url` 512–8,192
  (4,096); the snippet counts bind only within the token budget;
  `context_threshold_mode`; `enable_local`; `enable_source_metadata`;
  `goggles`; `freshness`; `safesearch` (no filtering when unset). Answer:
  `grounding.generic[]` (`url`, `title`, `snippets[]`), `grounding.poi`,
  `grounding.map[]`, `sources{url: {title, hostname, age[4]}}`, `age`
  being a full date, `YYYY-MM-DD`, a relative age and an ISO timestamp.
  An empty `generic` means nothing relevant was found.
- News: `q` ≤ 400 characters and 50 words; `count` 1–50; `safesearch`
  default `strict`; results with `title`, `url`, `description`, `age`,
  `page_age`, `breaking`, `extra_snippets`.
- Rate limits: a 1-second sliding window and a monthly quota;
  `X-RateLimit-Limit`, `-Policy`, `-Remaining` and `-Reset` give both as
  `per-second, per-month`; a 429 past either; only successful requests
  count and are billed.

### 5.3 `local`

holocron's own fetcher as a provider of `/v1/fetch`: the guarded client
(§6) and the extraction (§7). It needs no key and costs nothing, so it is
the fetch chain's last member: what Parallel cannot read, `local` tries.

- **Extraction slots**: 4, each waited for at most 2 s, then `busy` ("all
  4 extraction slots stayed taken"). A slot is held until the extraction
  ends, which may be after a timed-out request was answered (§7.1): this
  bounds the memory extraction takes (§8).
- **No focus**: `q` is not used; the answer is the whole page with
  `focused: false`. `status` and `content_type` are the response's;
  `published` is `null` (§7.1 turns date extraction off).
- Its failures are final (`blocked_address`, `unsupported_content`,
  `upstream_failed`, `upstream_timeout`, `busy`): no provider follows it.
- **Health** is always `ok`: it needs nothing but the network.

### 5.4 Chaining

Each endpoint has its own chain, its providers in order
(`provider.Routes{Search, Context, Fetch}`, from §4's `--search`,
`--context` and `--fetch`). A chain asks only its members that answer
the endpoint (a `Searcher`, `Contexter` or `Extractor`); a chain with
none is `not_available`.

- **Falling back**: the next member is asked only when one is
  unavailable (`provider_unavailable`: a refused key, used-up credit,
  rate-limited, unreachable, an error answer, a page it could not read),
  or when it timed out within its own share of the budget, within the
  request's budget. Any other error is final: a query refused, a page that
  does not exist, an address refused, the request's own budget gone. No
  member can do better.
- **Budget shares**: a member with a share (Parallel's and Brave's are
  0.6) gets at most that share of the remaining budget while another
  member follows it, so the next is still asked in time; the last member
  gets all that remains. A member that exceeds its share is moved past
  with the reason `timeout`.
- When every member failed: `provider_unavailable`, their messages
  joined; `upstream_timeout` when the last timed out.
- `provider` in the answer names the member that answered;
  `providers_failed` lists those that failed before it, in order. Each
  member moved past is §10's `fallback`, with its reason. §8.1's keys
  hold the chain's name (`parallel,brave`), so a chain's entries are never
  mixed with another chain's.
- What a call billed is recorded per provider, with the fallbacks and
  the quotas its answers gave, for its log line (§10), the metrics (§8.2)
  and the dashboard (§8.3).

`holocron install` chooses the chains by the keys on the host (§9): with
both, `parallel,brave` for search and context, and `parallel,local` for
fetch.

### 5.5 Keys, holds and bills

What the metered providers (Parallel, Brave) share:

- **The key**: one file per provider, read once at startup: the file
  `--<name>-key-file` names, else `$CREDENTIALS_DIRECTORY/<name>-key`
  (the credential systemd delivered, §9.2), else none, and `auto` leaves
  the provider out (§4). One line, trimmed, not empty. A regular file
  that its group or others can read is refused ("chmod 600 it"), as ssh
  refuses a private key; a pipe (a shell's process substitution) is read
  as it is. In `$CREDENTIALS_DIRECTORY` only others are refused: systemd
  delivers a credential with mode 0440 in a directory only the service
  can open. On the host the key is an encrypted credential or a plain
  0600 file, put there by `holocron key set` (§9.4). The key is sent only
  in its provider's header (`x-api-key`, `X-Subscription-Token`); never in
  a flag value, the environment, a log line or an error message, which
  never quotes the file's content.
- **The API client**: no proxy from the environment, TLS 1.2 or later,
  and no redirect followed, so a key sent in a header never reaches
  another host: a 3xx is answered as a failure (`provider_unavailable`).
- **Holds**: after a failure more calls cannot fix (a refused key: 10
  minutes; used-up credit or quota: an hour, or Brave's reset), every call
  is answered with that failure without asking, and the provider's
  health is `down`. The chain moves past it meanwhile.
- **Bills**: each provider counts its billed units and their estimated
  cost in micro-dollars since start, from §5's price table: §3.4's
  `billed`, §8.2's `holocron_billed_total` and `holocron_cost_usd_total`,
  §10's `<provider>_billed` and `<provider>_cost_usd`, §8.3's costs. The
  cost is holocron's estimate, not the provider's invoice.

### 5.6 Later providers

Another API (Kagi, Exa, …) is a new implementation of the capabilities it
has, a name in §4's lists, and its key delivered as §5.5's are.

## 6. The address guard

The fetcher is an SSRF risk: the model chooses the URL, and a page it has
read can tell it which one. The guard keeps every fetch on the public
internet. The deployment's firewall is the fence; the guard is the first
line and the error message.

1. **Before any lookup**, for the URL and every redirect target, in this
   order: scheme `http`/`https`; no user info; the host not empty, not
   single-label (`nas`, `localhost`), and not under `.localhost`,
   `.local`, `.internal`, `.home.arpa`, `.lan` or `.ts.net`, and, when it
   is a literal address, outside the ranges of step 2; then port 80 or
   443. These are `bad_request` (scheme, credentials, port) or
   `blocked_address` (names, literal addresses). The host is checked before
   the port so a private target is named as one whatever its port
   (§12's `http://192.168.1.20:8443/`).
2. **At connect time**, in the dialer's `Control` hook (`net.Dialer`),
   which sees the exact address about to be connected for every attempt,
   including each address Go's dual-stack dialing tries and every redirect:
   the address, with IPv4-mapped IPv6 unmapped, must be global unicast and
   outside every range below; the port must be 80 or 443. A refusal ends
   that attempt with `blocked_address`, naming the host and the address.
   Checking the dialed address, not a separate lookup, closes DNS
   rebinding.

   | Refused | Ranges |
   | --- | --- |
   | loopback, unspecified | `127.0.0.0/8`, `0.0.0.0/8`, `::1`, `::` |
   | private | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7` |
   | CGNAT and tailnets | `100.64.0.0/10` |
   | link-local | `169.254.0.0/16`, `fe80::/10` |
   | special use | `192.0.0.0/24`, `192.0.2.0/24`, `198.18.0.0/15`, `198.51.100.0/24`, `203.0.113.0/24`, `240.0.0.0/4`, `255.255.255.255`, `2001:db8::/32` |
   | multicast | `224.0.0.0/4`, `ff00::/8` |
   | v4 embedded in v6 | `64:ff9b::/96` (NAT64), `2002::/16` (6to4): refused outright |

3. The transport has **no proxy** (`Proxy: nil`, never from the
   environment), its own resolver bounded by the request's deadline, and
   redirects followed by holocron's own `CheckRedirect` (≤ 5 hops, step 1
   on each target).

Literal addresses in any spelling (`0x7f000001`, `2130706433`, `127.1`,
`[::ffff:127.0.0.1]`) fail step 1 as single-label names or as the address
they are, fail to resolve (`upstream_failed`), or reach step 2 as the
address they mean and are refused there. Wildcard-DNS names (`192.168.1.1.nip.io`)
pass step 1 and are refused by step 2.

A URL handed to Parallel's Extract API (§5.1) is fetched from Parallel's
network, where step 2 cannot run. It passes step 1 again, and its host is
resolved here and refused unless every address is public
(`guard.CheckHost`), so a private target is never sent and is refused
with the same `blocked_address`. Redirects Parallel follows are its own.

## 7. Extraction

The local fetcher's (§5.3); Parallel extracts with its own.

### 7.1 The pipeline

1. **Bytes to UTF-8**: the charset from the `Content-Type` header, else
   the document's `<meta>`, else UTF-8 (`golang.org/x/net/html/charset`).
   An unknown or broken charset decodes as UTF-8 with replacement
   characters, never an error.
2. **Plain types** (§3.3) are returned as decoded text, title empty.
3. **HTML**: parsed once (`golang.org/x/net/html`, scripting off), then
   prepared: every `<a href>` resolved against the document's `<base
   href>`, else the **final URL including its path**, so a same-page anchor
   stays `…/page/#section` (go-trafilatura's own resolution joins paths as
   directories: `../howto/x.html` from `/3/library/ipaddress.html` lands
   under `/3/library/`); soft hyphens removed; each inline `<code>` outside
   `<pre>` flattened to its text (go-trafilatura, like Python trafilatura,
   moves an element nested in inline code, nginx's
   `<code><i>size</i></code>`, out of its place in the paragraph).
   `go-trafilatura` then finds the main content (links and tables kept,
   images dropped, its readability fallback enabled, tuned for recall,
   date extraction off). **Comments are kept** and appended after the main
   content, as Python trafilatura does by default: what looks like a
   comment thread is often content (PEP 8's `<section id="comments">`).
   When the result is shorter than go-trafilatura's minimum (250
   characters), it has replaced it with its paragraph-only baseline;
   Python 2.3 keeps the baseline only when it is longer (trafilatura#896),
   so such a page is extracted again without the baseline and the longer
   text kept (a short page keeps its lists and tables). `html-to-markdown`
   (base, CommonMark and table plugins) turns the nodes into Markdown;
   before that, each outermost `<code>` whose text spans lines becomes
   `<pre><code>`: go-trafilatura marks a code block as a bare `<code>`, and
   Python's Markdown writer fences it.
4. **Fallback**: when step 3 yields nothing, the visible text of `<body>`
   (without `script`, `style`, `noscript`, `template`, `svg`), one block
   per line.
5. **Title**: go-trafilatura's metadata, else `<title>`, clipped to 200.

Extraction runs under the request's deadline. If the deadline passes it
answers `upstream_timeout` and lets the extraction finish in the
background; its extraction slot (§5.3) is freed only when it does, so
abandoned work still counts against concurrency and memory.

### 7.2 The gate: compared against Python trafilatura

Before §7.1 is accepted, a one-off comparison of `holocron fetch --text`
against Python trafilatura 2.3.1 with the options a Python extraction
service would use (`output_format="markdown"`, `include_links`,
`include_tables`, `include_formatting`, `favor_recall`), on the same
downloaded bytes of 20 pages a coding agent reads:

| Page | URL |
| --- | --- |
| PEP 8 | https://peps.python.org/pep-0008/ |
| Python `ipaddress` | https://docs.python.org/3/library/ipaddress.html |
| MDN `Promise` | https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Promise |
| Go `net/http` | https://pkg.go.dev/net/http |
| GitHub README | https://github.com/ziglang/zig |
| Stack Overflow | https://stackoverflow.com/questions/11227809 |
| docs.rs | https://docs.rs/serde/latest/serde/ |
| The Rust book | https://doc.rust-lang.org/book/ch04-01-what-is-ownership.html |
| Arch wiki | https://wiki.archlinux.org/title/Systemd |
| man page | https://man7.org/linux/man-pages/man2/open.2.html |
| Wikipedia | https://en.wikipedia.org/wiki/Server-side_request_forgery |
| GitHub issue | https://github.com/rust-lang/rust/issues/57563 |
| Go blog | https://go.dev/blog/loopvar-preview |
| Kubernetes docs | https://kubernetes.io/docs/concepts/workloads/pods/ |
| Dockerfile reference | https://docs.docker.com/reference/dockerfile/ |
| PostgreSQL docs | https://www.postgresql.org/docs/current/sql-select.html |
| nginx docs | https://nginx.org/en/docs/http/ngx_http_proxy_module.html |
| React docs | https://react.dev/reference/react/useEffect |
| SQLite docs | https://www.sqlite.org/lang_select.html |
| Podman quadlet man page | https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html |

Per page: extracted characters, headings, fenced code blocks, links, and
milliseconds; then the two outputs read side by side. **Accepted** when on
every page holocron keeps the main content and every code block Python
keeps, and is not slower in total. Otherwise the gap is tuned or
§7.1 is reconsidered before implementation goes further. The pages are
not committed (they are others' content); the harness is throwaway, and
the result table lands in this section.

#### Result (2026-10-16): accepted

Pages downloaded 2026-10-16; Stack Overflow answers every non-browser
client from the measuring host with 403, so its bytes are the Wayback
Machine's unmodified copy (`web.archive.org/web/2026id_/…`, captured
2026-04-25). Both sides extracted the same bytes in one warm process each
(Python 3.14.8, trafilatura 2.3.1; Go 1.27.2, §7.1 as written above), on
an Apple-silicon Mac. Each cell is holocron / Python.

| Page | Characters | Headings | Code blocks | Links | ms |
| --- | --- | --- | --- | --- | --- |
| PEP 8 | 48,112 / 47,034 | 43 / 42 | 78 / 18 | 63 / 70 | 14 / 45 |
| Python `ipaddress` | 49,339 / 45,205 | 20 / 20 | 29 / 5 | 220 / 220 | 15 / 60 |
| MDN `Promise` | 35,780 / 34,575 | 23 / 23 | 17 / 17 | 83 / 83 | 8 / 18 |
| Go `net/http` | 178,488 / 191,884 | 182 / 179 | 135 / 69 | 686 / 1,165 | 55 / 137 |
| GitHub README | 3,403 / 3,399 | 0 / 0 | 0 / 0 | 0 / 0 | 21 / 113 |
| Stack Overflow | 3,822 / 3,807 | 1 / 1 | 2 / 2 | 3 / 3 | 26 / 60 |
| docs.rs | 6,065 / 5,795 | 8 / 8 | 0 / 0 | 43 / 43 | 2 / 5 |
| The Rust book | 23,613 / 23,123 | 12 / 12 | 10 / 10 | 18 / 18 | 4 / 8 |
| Arch wiki | 60,642 / 54,976 | 52 / 60 | 55 / 1 | 182 / 181 | 15 / 46 |
| man page | 65,746 / 68,446 | 0 / 0 | 14 / 14 | 186 / 350 | 8 / 21 |
| Wikipedia | 9,402 / 8,719 | 8 / 8 | 0 / 0 | 60 / 61 | 7 / 20 |
| GitHub issue | 12,993 / 12,757 | 21 / 21 | 0 / 0 | 60 / 58 | 18 / 46 |
| Go blog | 8,186 / 8,170 | 7 / 7 | 7 / 7 | 13 / 13 | 3 / 4 |
| Kubernetes docs | 25,890 / 25,678 | 0 / 0 | 4 / 4 | 66 / 66 | 16 / 31 |
| Dockerfile reference | 130,879 / 115,958 | 100 / 87 | 121 / 123 | 220 / 220 | 42 / 141 |
| PostgreSQL docs | 66,570 / 66,074 | 0 / 0 | 34 / 10 | 38 / 38 | 18 / 49 |
| nginx docs | 65,043 / 58,512 | 4 / 4 | 80 / 12 | 116 / 116 | 16 / 59 |
| React docs | 42,472 / 42,050 | 26 / 26 | 30 / 21 | 67 / 65 | 24 / 95 |
| SQLite docs | 36,328 / 36,783 | 0 / 0 | 0 / 0 | 76 / 76 | 41 / 73 |
| Podman quadlet man page | 134,573 / 112,422 | 251 / 251 | 30 / 30 | 276 / 276 | 29 / 118 |
| **Total** | | | | | **382 / 1,150** |

Read side by side, and checked line by line (every Python line of 30+
characters and every Python code block searched for in holocron's text,
Markdown punctuation ignored):

- **Main content and code blocks**: holocron keeps every code block
  Python keeps. The three it "misses" (Dockerfile 2, nginx 1) are Python
  fences that swallowed a heading or prose; their text is in holocron's
  output. Every Python line is found on 9 pages; the rest differ in
  formatting only: Python's `<sup>`/`<sub>` kept as HTML, list numbering
  (`3.` against `03.`), and Python's mangled inline code (Arch wiki,
  PostgreSQL and nginx lose or reorder words around `<code><i>…</i></code>`,
  which holocron keeps in place). Higher holocron code-block counts are
  real blocks Python left unfenced (PEP 8, Arch wiki's `# systemctl …`
  lines, which Python counts as headings) or split.
- **One loss, accepted**: PEP 8's four footnotes (`<aside
  role="doc-footnote">`) are dropped by go-trafilatura's cleaning.
- **Links**: lower counts on `net/http` and the man page are links inside
  code blocks (declarations, synopses), which a fence cannot carry.
  holocron's links resolve against the full URL
  (`https://peps.python.org/pep-0008/#references`); Python's against the
  host only (`https://peps.python.org#references`), and `../` links lose
  a path segment.
- **Speed**: 3× faster in total. A process's first extraction also loads
  go-py3langid's model, about 230 ms (once per `serve`, once per CLI
  `fetch`).

The gaps found on the way and closed in §7.1: code blocks rendered as
one-line inline code (the `<code>` → `<pre><code>` step), PEP 8's
Comments section dropped (comments kept), words moved out of inline code
(flattening), `../` links resolved wrongly (own resolution), lists and
tables lost on short pages (the baseline step).

## 8. Serving

- `net/http` server: `ReadHeaderTimeout` 5 s, `ReadTimeout` 10 s,
  `WriteTimeout` 35 s, `IdleTimeout` 60 s, `MaxHeaderBytes` 16 KiB. A slow
  or idle client cannot hold the server. On `SIGINT` or `SIGTERM`,
  in-flight requests get up to 25 s to finish.
- **32 request slots** for upstream calls, shared by search, context and
  fetch: one per call, which requests with the same key share (§8.1). A
  call waits at most 2 s for one, then `busy`. The local fetcher has
  **4 extraction slots** of its own (§5.3), so a fetch through it holds
  one of each. `/v1/health`, `/v1/metrics`, `/v1/stats` and the
  dashboard take none.
- Every request runs under a `context` with its budget (§3.2, §3.3,
  §3.5); outbound HTTP, DNS and extraction all observe it.
- A panic in a handler is recovered into `internal` with a log line;
  the process keeps serving.
- Memory: local extraction is what is large, and its 4 slots bound it.
  Measured 2026-10-16 (§7.2's pages): about 125 MiB live once the first
  extraction has run, 92 MiB of it go-py3langid's language model, which
  go-trafilatura loads on every extraction with no option to skip it;
  then about 13× the body live per extraction (SQLite's 1.9 MiB page:
  28 MiB; a 3.9 MiB page: 49 MiB), so a 5 MiB body holds about 75 MiB
  with its raw and decoded copies. Four at once: about 425 MiB. §8.1's
  cache adds at most 64 MiB of text: about 490 MiB. A provider's answer
  is read whole, at most 8 MiB (Parallel) or 4 MiB (Brave), and an
  extracted page's text is at most 500,000 characters; the rest of the
  headroom is theirs. `GOMEMLIMIT` (600 MiB) is set below the unit's
  `MemoryMax` (768 MiB, §9.2) so the runtime collects before the kernel
  kills.

### 8.1 Cache

Every metered call is billed (§5), agents repeat queries, and they page
through fetched documents (`start=2000`, `4000`, …). An in-memory cache
cuts the bill and makes paging one upstream fetch. It lives in the
process only: no disk, no Valkey; a restart empties it.

- **Behind validation.** A request's parameters, and for a fetch the
  URL's length and §6's step 1 (scheme, credentials, names, literal
  addresses, port), are checked before the lookup. An error answer is the
  same with or without the cache, and a hit never returns what a miss
  would refuse.
- **search** and **context** (§3.5): keyed by the endpoint's chain name
  (§5.4) and the query trimmed, its whitespace collapsed, lowercased. The
  providers are asked for 10 results whatever `n`, and the entry is
  §3.2's answer with up to 10 results and its `providers_failed`; an
  answer for any `n` is its first `n` results, with this request's own
  `query`. The key also holds `freshness`, and a context entry's `tokens`
  and `urls`. Fresh for **30 minutes**, or **5 minutes** when
  `providers_failed` is not empty.
- **fetch**: keyed by the fetch chain's name, the URL with its scheme and
  host lowercased and its fragment dropped (the query is kept), and `q`
  with its whitespace collapsed, lowercased (empty for the whole page).
  The entry is the extracted document (text, title, final URL, status,
  content type, published date, whether it is focused, whether the
  source was cut, who answered), not the raw body; `start` and
  `max_chars` are a window into it, and `url` is this request's own.
  Fresh for **15 minutes**; with `q`, **30 minutes**, like a search.
- **Failures**: a fetch's `upstream_failed` (a 404, a DNS failure) and
  `unsupported_content` are kept **1 minute**. `provider_unavailable`,
  `upstream_timeout`, `busy` and `internal` are never kept;
  `bad_request` and `blocked_address` never reach the cache (step 1
  answers before the lookup, and a refusal at connect time is not
  stored).
- **One upstream call per key**: concurrent requests with the same key
  share one call, and only that call takes a request slot (§8). It runs
  under its own budget, detached from any one request, so a client that
  leaves does not fail the others; it is cancelled when every request
  waiting for it has left. Each request waits within its own budget. A
  small standard-library implementation (no `x/sync`).
- **Stale if error**: a search or context entry is kept up to **24
  hours** after it was stored. When it would answer
  `provider_unavailable` (§3.1), an entry past its freshness but within
  those 24 hours is answered instead, marked `stale`. Never for another
  error, never for a fetch.
- **`Cache-Control: no-cache`** on a request skips the lookup (stale
  entries too), asks upstream, and replaces the entry with what it
  answers; it is also sent on to Brave (§5.2).
- **What it saves**: a hit or a stale answer counts the cost of the call
  that stored the entry as saved (§8.2's
  `holocron_cache_saved_usd_total`, §8.3's `saved_usd`).
- **Storage**: least-recently-used maps bounded by bytes (a mutex and
  `container/list`): **8 MiB** for searches, **8 MiB** for contexts,
  **48 MiB** for documents, 64 MiB in all. An entry's size is the bytes
  of its strings plus a fixed overhead; an entry larger than its map is
  not stored. An expired entry is dropped when a lookup meets it, or
  evicted with the least recently used.
- **The contract** (search, context and fetch): `X-Search-Cache` is
  `hit` (a fresh entry), `stale`,
  or `miss` (asked upstream, including a request that shared another's
  call and one with `no-cache`); `Age` on `hit` and `stale` only, in
  whole seconds since the entry was stored (§3.1). `/v1/health` counts
  the entries and their bytes (§3.4).

### 8.2 Metrics: `GET /v1/metrics`

For monitoring: a Prometheus-compatible
scraper on the host reads `127.0.0.1:8080/v1/metrics` every 15–60 s. It
is for operations, not part of the client contract (§3.4's `endpoints`
does not list it). Prometheus text format 0.0.4 (`Content-Type:
text/plain; version=0.0.4; charset=utf-8`), written with the standard
library; no client library.

- It takes no request slot, is not cached, and is neither logged nor
  counted per request. `GET` only, like every path.
- **No query, URL or host is a label value**: they are unbounded and
  private. `endpoint` is one of `/v1/search`, `/v1/context`, `/v1/fetch`,
  `/v1/health`, or `other` (an unknown path); the other labels take the
  §10 log line's values, empty when the line has none.

| Family | Type | Labels | Counts |
| --- | --- | --- | --- |
| `holocron_requests_total` | counter | `endpoint`, `code`, `cache`, `answered_by` | every request §10 logs; `code=""` on success |
| `holocron_request_duration_seconds` | histogram | `endpoint` | the logged duration; buckets 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30 s |
| `holocron_billed_total` | counter | `provider` | the units a metered provider billed (requests, or URLs extracted), counted as §10's `<provider>_billed`: once per call |
| `holocron_cost_usd_total` | counter | `provider` | their estimated cost in US dollars, from §5's price table, as §10's `<provider>_cost_usd` |
| `holocron_cache_saved_usd_total` | counter | | the estimated cost of the calls that hits and stale answers did not repeat (§8.1) |
| `holocron_fallbacks_total` | counter | `provider`, `reason` | per request, as §10's `fallback` |
| `holocron_providers_failed_total` | counter | `provider` | each name in an answer's `providers_failed`, once per upstream call (not on hits or stale answers) |
| `holocron_provider_up` | gauge | `provider` | 1 or 0, §3.4's state; a metered provider's check makes no call |
| `holocron_cache_entries`, `holocron_cache_bytes` | gauge | `kind` (`search`, `context`, `fetch`) | §8.1's maps |
| `holocron_cache_lookups_total` | counter | `kind`, `result` (`hit`, `miss`, `stale`) | each request that reached the cache |
| `holocron_slots_in_use`, `holocron_slots_max` | gauge | `pool` (`upstream`, `extract`) | §8's request slots, and the local fetcher's extraction slots when a route has it |
| `holocron_busy_total` | counter | | `busy` answers |
| `holocron_build_info` | gauge | `version`, `goversion` | always 1 |
| `go_goroutines`, `go_heap_objects_bytes`, `go_memory_total_bytes`, `go_gc_cycles_total` | gauge, gauge, gauge, counter | | from `runtime/metrics` |

Counters start at 0 when holocron starts; the time-series store handles
the reset.

### 8.3 The dashboard: `GET /` and `GET /v1/stats`

A page for the operator: each provider's state, traffic, latency, cost
and savings, the cache, the slots, memory and the last requests. On by
default; `serve --dashboard=false` turns both paths off (`not_found`).
Like `/v1/metrics`, neither takes a slot, is cached, logged or counted,
and neither is part of the client contract.

- **The page**: `/` (`index.html`), `/ui/app.js`, `/ui/app.css`,
  `/ui/icon.svg` and `/favicon.ico`, embedded in the binary (`go:embed`),
  so a release stays one file. Each is served with `Cache-Control:
  no-cache`, `X-Content-Type-Options: nosniff`, `Referrer-Policy:
  no-referrer`, `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy:
  same-origin`, and a strict policy that keeps the page to its own files
  (no inline script, no other host, no frames, no forms):

  ```text
  Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
  ```

  The script builds the page with DOM calls only: every value from the
  server, queries and URLs above all, goes in as text, never as markup.
  It reads `/v1/stats` every 5 s, not while the page is hidden, and once
  more when it is shown again.
- **Try the API**: a form on the page calls `/v1/search`, `/v1/context`
  or `/v1/fetch` from the browser, as a client would, and shows the
  answer rendered or as JSON, with its status, time, `X-Search-Cache`,
  the provider and `providers_failed`, a curl line to copy, and the next
  page of a fetch. These are ordinary requests: cached, logged, counted
  and billed like any other. A result's link is made only for an
  `http(s)` URL.
- **`/v1/stats`**: the page's data, in memory since the process started:
  the version, Go version, start time and uptime; whether queries are
  logged; the routes; per provider its state (with the reason when
  `down`), endpoints, answers, fallbacks by reason, billed units, cost
  since start and over the last 24 hours, and quota; totals since start
  and over the last 24 hours (requests, errors, cache hits, cost, saved);
  per endpoint its requests, errors, hits and the 50th and 95th
  percentile of its last 512 durations; the last 24 hours as a series of
  5-minute steps (requests per endpoint, errors, cost); error codes; per
  cache kind its entries, bytes and lookups; the slots by pool; `busy`
  answers; the runtime's heap, total memory, `GOMEMLIMIT` and goroutines;
  and the last 50 requests (time, endpoint, host, the provider that
  answered, fallbacks, cache, status, code, results, duration, cost).
  The last 24 hours are a ring of one-minute buckets. Only search,
  context and fetch are counted. A recent request carries its query or URL
  only while `--log-queries` is on (§10); otherwise its host alone.

## 9. Deployment: `holocron install`

Run as root on a systemd Linux host (the host's packages and firewall are
the deployment's job, not holocron's). The binary is already at
`/usr/local/bin/holocron`. Idempotent: run it again after copying a new
binary, or after a key changes. `--dry-run` prints each file it would
change and each command it would run, and changes nothing.

1. **Checks**: Linux, root, systemd as init, `systemctl` and `useradd` on
   `PATH`; a failed check names what is missing. A dry run reports a
   failed check and goes on, so it previews anywhere.
2. **User**: the system user and group `holocron` (no home, no shell).
3. **Keys**: for each provider with a key (`parallel`, then `brave`), the
   encrypted credential `/etc/credstore.encrypted/holocron.<name>-key`
   when it exists, else the plain file `/etc/holocron/<name>.key`, made
   0600 if it is not (systemd reads it as root, §9.2), with a hint to
   encrypt it (§9.4). The keys are the switch: the routes are
   `--search` and `--context` the providers with a key, in that order
   (`none` without any, and search and context answer `not_available`),
   and `--fetch` `parallel,local` with Parallel's key, else `local`.
   Adding or removing a key and running `install` again changes the
   routes, and an upgrade's `install` keeps what the host has. It prints
   the routes; a dry run too.
4. **SearXNG's removal** (§9.1), when an install before 0.2.0 left it.
5. **holocron**: `/etc/systemd/system/holocron.service` (§9.2).
6. **Start**: `systemctl daemon-reload`, `enable` and `restart`
   `holocron.service`.
7. **Wait** up to 60 s for `/v1/health` to report every provider `ok`,
   then print it and, with a search provider, one three-result search. On
   timeout, print the last 40 journal lines of `holocron.service` and exit
   non-zero.

The files are embedded in the binary (`go:embed`), so a holocron release
is its own complete install.

### 9.1 An older install's SearXNG

Before 0.2.0, `install` ran SearXNG as a Podman quadlet. When it finds
what that wrote (`/etc/containers/systemd/searxng.container`,
`/etc/searxng`, `/var/cache/searxng`), it reads the image from the
quadlet's `Image=` line, stops `searxng.service` (a failure is ignored:
the unit may be gone), removes the files, runs `systemctl daemon-reload`,
and, when `podman` is on `PATH`, `podman rmi` of that image. Podman itself
is the host's package and stays; `install` prints how to purge it. On a
host without them, the step does nothing.

### 9.2 holocron's unit

`User=holocron`, `Group=holocron`; for each key (§9 step 3)
`LoadCredentialEncrypted=<name>-key:/etc/credstore.encrypted/holocron.<name>-key`
or `LoadCredential=<name>-key:/etc/holocron/<name>.key`: systemd decrypts
or copies the key into a directory only the service reads, where `serve`
finds it as `$CREDENTIALS_DIRECTORY/<name>-key` (§5.5), so the `holocron`
user never reads `/etc/credstore.encrypted` or `/etc/holocron`;
`ExecStart=/usr/local/bin/holocron serve --listen 0.0.0.0:8080 --search
<routes> --context <routes> --fetch <routes>` (§9 step 3);
`Wants=`/`After=` `network-online.target`; `Restart=on-failure`,
`RestartSec=2s`; `MemoryMax=768M`, `Environment=GOMEMLIMIT=600MiB` (§8's
measurement), `TasksMax=96`; and the sandbox: `ProtectSystem=strict`,
`ProtectHome`, `PrivateTmp`, `PrivateDevices`, `NoNewPrivileges`, an empty
`CapabilityBoundingSet`, `ProtectKernelTunables`/`Modules`/`Logs`,
`ProtectControlGroups`, `ProtectClock`, `ProtectHostname`,
`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, `RestrictNamespaces`,
`RestrictRealtime`, `LockPersonality`, `SystemCallArchitectures=native`.
It listens on all addresses because the deployment's firewall decides who
reaches it.

### 9.3 The recommended host

What `install` does not do is the host: a small VM of its own (2 vCPU,
1 GiB, 20 GiB; with no container beside it, §8's memory fits), its
firewall enforced outside the guest (the hypervisor's per-VM firewall)
with outbound an allow-list: DNS to the resolver, TCP 80 and 443 to the
public internet (the providers' APIs among it), NTP, and every private
range dropped. Inbound only from the reverse proxy or the clients to
`:8080`, and SSH from an admin address. Never reachable from the public
internet: with no authentication it would be an open search and fetch
proxy, and its dashboard would show its traffic. The README walks
through it.

### 9.4 Keys: `holocron key set|rm PROVIDER`

`holocron key set parallel` (or `brave`) stores a key so that no one
types it into a file or a command line:

- It reads one line from stdin (at most 4 KiB, no spaces), with the
  terminal's echo off when stdin is one; the key is never printed.
- With `systemd-creds` on the host: `systemd-creds encrypt
  --name=<name>-key - /etc/credstore.encrypted/holocron.<name>-key`, the
  key on its stdin, never in an argument; the directory 0700, the file
  0600; a plain `/etc/holocron/<name>.key` left from before is removed.
  Without it: `/etc/holocron/<name>.key`, mode 0600, its directory 0700.
- Then it runs `install` (§9), which finds the key and changes the
  routes.

`holocron key rm <name>` removes both forms, then runs `install`.
`--dry-run` says what either would do, without reading the key, and runs
`install --dry-run`. Both need what `install` needs (§9 step 1).

## 10. Logging

One line per request on stdout (the journal, under systemd), `log/slog`
text format, its level by whose fault a failure is: `INFO` for success
and the client's own mistakes (`bad_request`, `not_found`,
`method_not_allowed`, `unsupported_content`, `not_available`); `WARN` for
the upstream's failures and holocron's capacity (`upstream_failed`,
`upstream_timeout`, `provider_unavailable`, `busy`) and for a refused
address (`blocked_address`: something asked for one); `ERROR` for
`internal`. An error line carries `code` and, while `--log-queries` is
on, `error`: the message the client got (it can quote the request). The
line has the endpoint, the status and error code, the duration (`ms`),
`provider` (the endpoint's chain, `parallel,brave`), the fetched host,
`cache=hit|miss|stale` (§8.1) for a request that reached the cache,
**the query (`q`, a fetch's question too) or the full URL (`url`)**, the
final URL after redirects (`final_url`), and the client's `User-Agent`
(`ua`). A search, context or fetch line also says how it was answered and
what it cost:

| Field | Value |
| --- | --- |
| `answered_by` | the provider that answered (on a hit, the one stored with the entry); absent on an error |
| `fallback` | `<provider>:<reason>` for each provider the chain moved past (§5.4), comma-separated: `quota` (used up, or a 402), `key_refused`, `rate_limited`, `timeout` (its budget share passed), `unreadable` (a page it could not read), `error` (anything else); only on a request that asked upstream |
| `results` | the results or sources returned (1 for a fetch) |
| `providers_failed` | the answer's `providers_failed`, comma-separated, when not empty |
| `<provider>_billed`, `<provider>_cost_usd` | the units this request's call was billed by that provider (`parallel_billed`, `brave_billed`) and their estimated cost in dollars (§5), on exactly one line per call: a shared call (§8.1) is counted on the first of its requests to be logged, so summing over the log counts the bill |
| `<provider>_remaining`, `<provider>_resets_in_s` | a provider's monthly remaining requests and the seconds to their reset, from the headers of its answer to this request (Brave, §5.2), only on a plan with a monthly limit (not a prepaid one) |

`--log-queries=false` leaves out the query and the URLs (keeping the
host), on the dashboard too (§8.3). Response bodies are never logged.
Startup logs the version, the Go version, the listen address, the three
routes, `log_queries`, `dashboard`, and with Parallel its two modes.
`/v1/metrics` (§8.2), `/v1/stats` and the dashboard (§8.3) are not
logged: a scrape every 15 s would drown the rest.

Queries are logged by default because holocron serves its operator's
own agents: the log is how a search the agent made is debugged.

## 11. Build, dependencies and releases

- **Go 1.27** (`go 1.27.2` in `go.mod`, the minimum toolchain); the exact
  toolchain is recorded by `holocron version`. Module path
  `github.com/tildaslashalef/holocron`.
- **Workflow**: commits go straight to `main` and are pushed; no branches or
  pull requests. `make check` passes before every commit.
- **Dependencies** (every addition is a SPEC edit):

  | Module | Version | For |
  | --- | --- | --- |
  | `github.com/markusmobius/go-trafilatura/v2` | v2.2.6 (2026-09-29, proxy.golang.org) | main-content extraction, metadata; Apache-2.0; a port of Python trafilatura 2.2.0's core, without its jusText fallback |
  | `github.com/JohannesKaufmann/html-to-markdown/v2` | v2.5.2 (2026-06-07, proxy.golang.org) | the content node to Markdown |
  | `golang.org/x/net` | as required by the above | `html` parsing, `html/charset` |

  Their own dependencies are pinned by `go.sum`; `go mod verify` runs in
  the checks.
- **Layout**:

  ```text
  Makefile             make help lists the targets
  .github/workflows/   ci.yml (push to main), release.yml (a vX.Y.Z tag)
  cmd/holocron/        main: flags and dispatch only
  internal/api/        the contract's types, error codes, handlers, slots, §8.3's stats
  internal/api/ui/     §8.3: the dashboard's page, embedded
  internal/cache/      §8.1: the byte-bounded LRU and the shared upstream call
  internal/metrics/    §8.2: counters, a histogram and the text exposition
  internal/guard/      §6: URL checks and the dial-time address check
  internal/fetch/      the guarded client: redirects, body cap, content types
  internal/extract/    §7: charset, HTML to Markdown, fallback, title
  internal/provider/   the interfaces, chaining (§5.4), keys, holds and bills (§5.5)
  internal/provider/*/ parallel/ §5.1, brave/ §5.2, local/ §5.3
  internal/install/    §9 and §9.4, and the embedded unit
  internal/check/      §12's acceptance suite
  ```

- **Checks** (`make check`, before every commit, and in CI on every push
  to `main`): `gofmt -l` empty,
  `go vet ./...`, `go mod verify`, `go test ./...`. Tests are offline: a
  fake Parallel, a fake Brave and fake sites on `httptest`, the guard tested through an
  injected resolver and dialer (every range of §6, the literal spellings,
  a redirect to a private address, a name resolving to public and private
  addresses at once), extraction against small fixtures written for the
  tests, `install` against a temporary root with `--dry-run`, the cache
  (§8.1) against an injected clock.
- **Releases are built by GitHub, never on a workstation.** Semantic
  Versioning. `make release V=X.Y.Z` refuses unless on a clean `main`
  equal to `origin/main` with no such tag, runs `make check`, then creates
  the annotated tag `vX.Y.Z` (signed when git is configured to sign) and
  pushes it. The tag triggers `.github/workflows/release.yml`: it refuses a
  tag that is not an annotated `vX.Y.Z`, runs `make check`, then `make
  dist`, and publishes a GitHub Release with `holocron-linux-amd64`,
  `holocron-linux-arm64` and `SHA256SUMS`, its notes the tag's message and
  the commit subjects since the previous tag. `make dist` is the same build
  locally: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X
  main.version=X.Y.Z"` per platform. Deployments pin the version and the
  binary's SHA-256. Actions are pinned by commit SHA. The contract's
  version (`/v1/`) and the binary's are separate axes.

## 12. Acceptance: `holocron check [BASE_URL]`

Against a running server, one line per check, non-zero exit if any fails.
Which checks run follows `/v1/health`'s `endpoints` and the fetch
route's first provider, so their count varies:

1. `/v1/health` → at least one provider, every provider `ok`.
2. `/v1/search?q=zig std.http.Client` → when health lists `/v1/search`,
   ≥ 3 results, each field within its bound (`snippets` at most 5);
   `n=20` clamped to 10; `q=` empty → `bad_request`. Otherwise
   `not_available`.
3. `/v1/context?q=zig std.http.Client` → when health lists
   `/v1/context`, ≥ 1 source with snippets, each field within its
   bound; otherwise `not_available`.
4. `/v1/fetch` of PEP 8 with `max_chars=2000` → 2,000 characters, a
   `provider`, `chars` > 2,000, `truncated`, and when `local` fetches
   first, starting with a heading; `start=2000` pages on (the two
   windows are `max_chars=4000`'s).
5. Guard, each → `blocked_address`: `http://192.168.1.20:8443/`,
   `http://nas.home.arpa:8443/`, `http://127.0.0.1:8888/`,
   `http://192.168.1.1.nip.io/`. A public redirect to
   `http://192.168.1.1/` (`https://postman-echo.com/redirect-to?url=…`)
   → `blocked_address` when `local` fetches first; when Parallel does, it
   follows the redirect on its own network (§5.1), and the check is only
   that the answer is an error, not a page.
6. When `local` fetches first: a PDF → `unsupported_content`. When
   Parallel does: the PDF (RFC 9110's) → ≥ 1,000 characters of text, and
   PEP 8 with `q=maximum line length` → `focused`, its text mentioning 79.
7. A 404 → `upstream_failed`; port 8443 → `bad_request`.

Items 2–7 use public sites, and with a metered provider most of them
are billed; a failure there names the site, so an outage elsewhere is told apart from
a holocron bug.
