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
  The providers are a self-hosted SearXNG and the Brave Search API, alone
  or chained (§5).
- `/v1/context` asks a provider that has one (Brave) for passages from
  several pages that answer a query, for an agent to read in one call
  (§3.5).
- `/v1/fetch` fetches one public URL through an **address guard** (§6) and
  extracts its readable text (§7).
- `holocron install` sets a Linux host up: holocron's systemd unit and
  SearXNG as a Podman quadlet (§9).

holocron was made for [nuclis](https://github.com/tildaslashalef/nuclis)
(the agent in `nuclis chat`, and the hosted tools of `nuclis serve`); any
other client on a trusted network that speaks §3 can use it. Nothing in
holocron is specific to one deployment; the README describes the
recommended host.

### Non-goals

- Public exposure, accounts or authentication (v1 is for a trusted LAN or
  tailnet; a token is a v2 change).
- JavaScript rendering, crawling, PDFs and images.
- A persistent or shared cache: §8.1's is in memory, one per process.
- A general HTTP proxy: `GET` of pages for reading, nothing else.

## 2. Principles

- **The contract is the product.** Providers, extraction libraries and
  SearXNG can all change behind `/v1/` without a client noticing.
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
do not know.

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
  | `bad_request` | 400 | a missing or invalid parameter (empty query, a URL that is not `http`/`https`, too long, a port other than 80/443, credentials in the URL), or a query the provider refuses (Brave: over 600 characters or 75 words) |
  | `not_found` | 404 | an unknown path |
  | `method_not_allowed` | 405 | any method but `GET` |
  | `not_available` | 501 | the configured providers have no such endpoint (`/v1/context` with SearXNG alone) |
  | `blocked_address` | 403 | the URL, or a redirect, resolves to a non-public address (§6) |
  | `unsupported_content` | 415 | the response's type is not one §3.3 extracts |
  | `upstream_failed` | 502 | the remote site failed (DNS, connection, TLS, a 4xx/5xx — its status in `message`), or more than 5 redirects |
  | `no_engines` | 503 | every engine of the provider failed or was rate-limited, or the provider is unreachable |
  | `busy` | 503 | every request slot stayed taken for 2 s (§8) |
  | `upstream_timeout` | 504 | the remote site or the provider exceeded the budget |
  | `internal` | 500 | a bug in holocron; the log line has the detail |

### 3.2 `GET /v1/search?q=<query>&n=<count>&freshness=<age>`

- `q`: 1–400 bytes after trimming. `n`: 1–10, default 8.
- `freshness` (optional): only pages from the last day (`pd`), week
  (`pw`), month (`pm`) or year (`py`), or a date range
  `YYYY-MM-DDtoYYYY-MM-DD` (from ≤ to); anything else is `bad_request`.
  Brave filters by the page's own date; SearXNG has a `time_range` of a
  day, week, month or year and no ranges, so it ignores a date range
  (§5.1).
- Budget: 10 s for the whole request.
- `200`:

  ```json
  {
    "query": "zig std.http.Client proxy",
    "provider": "searxng",
    "results": [
      {
        "title": "std.http.Client — Zig documentation",
        "url": "https://ziglang.org/documentation/…",
        "snippet": "…",
        "engine": "duckduckgo",
        "published": "2026-09-01",
        "extra_snippets": ["…another passage…"]
      }
    ],
    "altered_query": null,
    "engines_failed": ["mojeek"]
  }
  ```

- `provider`: the provider that answered (§5).
- `results`: at most `n`, deduplicated by URL, in the provider's rank
  order; only `http`/`https` URLs; `title` ≤ 200 and `snippet` ≤ 500
  characters, whitespace collapsed, cut at a word with `…` appended;
  `engine` the engine inside the provider (the provider's own name when it
  has one engine); `published` an ISO date or `null`; `extra_snippets`
  more passages from the page (Brave: up to 5), each within `snippet`'s
  bounds, empty when the provider gives none (SearXNG).
- `altered_query`: the query the provider searched when it corrected
  this one's spelling (Brave's `query.altered`), else `null`: how a client
  learns that its words were not the ones searched.
- Zero results is a `200` with an empty list. No results **while** engines
  failed is `no_engines`, not an empty `200`.
- `engines_failed`: engines that errored or were rate-limited for this
  query, empty when none: how a client learns results are partial.

### 3.3 `GET /v1/fetch?url=<url>&start=<offset>&max_chars=<n>`

- `url`: `http` or `https`, ≤ 2,048 bytes, no credentials, ports 80 and
  443 only. `start`: a character offset into the extracted text, default 0.
  `max_chars`: 1–100,000, default 20,000.
- Budget: 20 s for everything — DNS, connect, every redirect, the body and
  the extraction. At most 5 redirects, each hop checked by the guard (§6).
  The raw body is read to at most 5 MiB after decompression (more is cut,
  and `truncated` says so).
- Extracted types: `text/html` and `application/xhtml+xml` (to Markdown,
  §7), `text/plain`, `text/markdown`, `application/json`, `text/xml` and
  `application/xml` (as text). A missing `Content-Type` is sniffed from the
  first 512 bytes. Anything else is `unsupported_content`, decided from the
  headers before the body is read.
- Requests ask like a browser (`Accept:
  text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8`): a
  site that answers `text/markdown` with its raw source (react.dev sends
  its MDX, components and all) sends the HTML §7 extracts instead.
- The extracted document is cached for 15 minutes (§8.1): `start` is how
  a client pages through a long document, and paging costs one upstream
  fetch.
- `200`:

  ```json
  {
    "url": "https://example.org/a",
    "final_url": "https://example.org/a/",
    "status": 200,
    "content_type": "text/html",
    "title": "…",
    "text": "# Heading\n\nParagraph…",
    "start": 0,
    "chars": 48211,
    "truncated": true
  }
  ```

- `chars`: the length of the whole extracted text. `truncated`: the
  returned text ends before `chars`, or the raw body was cut at 5 MiB.
  Lengths and offsets count Unicode code points, not bytes. A `start` at
  or past `chars` returns empty `text`.
- `text` is Markdown: headings `#`, list items `- `, fenced code blocks,
  tables, links `[text](absolute url)`. Never HTML. A page that renders its
  content with JavaScript yields little; fetch its source instead.
- `title`: the page's title, ≤ 200 characters; empty for plain types.
- A site behind a bot challenge answers `upstream_failed`, its message
  saying so: Cloudflare's (`cf-mitigated: challenge`) wants a browser to
  run its JavaScript, and a browser's `User-Agent` does not pass it
  either (Stack Overflow and AP News, measured 2026-10-16). holocron does
  not try to get past a bot check; the message points to `/v1/context`,
  whose provider has the page's text from its own index.

### 3.4 `GET /v1/health`

`200` always, while the process answers:

```json
{
  "version": "0.1.0",
  "providers": {"brave": "ok", "searxng": "ok"},
  "endpoints": ["/v1/search", "/v1/context", "/v1/fetch"],
  "billed": {"brave": 37},
  "cache": {"entries": 41, "bytes": 3145728}
}
```

Each configured provider is `ok` or `down` (its own health check, ≤ 3 s;
Brave's makes no call, §5.2). `endpoints` lists what this server answers
with its providers, so a client offers only the tools that work.
`billed` counts each metered provider's billed requests (its successful
answers) since holocron started. `quota` (absent in the example) is a
metered provider's monthly remaining requests and the seconds to their
reset, from its last answer, only when its plan has a monthly limit:
Brave's prepaid plan has none (§5.2), and its credit is visible only in
Brave's dashboard. `cache` counts §8.1's entries and their bytes, all
caches together.

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
    "provider": "brave",
    "sources": [
      {
        "url": "https://ziglang.org/documentation/…",
        "title": "std.http.Client — Zig documentation",
        "published": "2026-09-01",
        "snippets": ["…a passage from the page…", "…another…"]
      }
    ],
    "engines_failed": []
  }
  ```

- `sources`: in the provider's order, `http`/`https` URLs only,
  deduplicated; `title` ≤ 200 characters, whitespace collapsed;
  `published` an ISO date or `null`. A snippet is the page's text as the
  provider extracted it, newlines kept: prose, or JSON-serialized
  structure (a table, code). As a backstop beside the provider's budget,
  the snippets together are cut at 8 characters per requested token.
- Zero sources is a `200` with an empty list. No sources while providers
  failed is `no_engines`, as §3.2.
- `engines_failed`: as §3.2; a chained provider that failed is listed by
  its name (§5.3).
- The text is other sites' content, as with `/v1/fetch`: data, never
  instructions. The provider fetched it, not holocron, so the guard (§6)
  has nothing to check; a client that follows a URL fetches it through
  `/v1/fetch`.
- With no provider that has context (SearXNG alone): `not_available`,
  after the parameters are validated.

## 4. The command line

| Command | Does |
| --- | --- |
| `holocron serve` | the HTTP server (§8). Flags: `--listen` (default `127.0.0.1:8080`), `--provider` (a provider or a comma-separated chain, default `searxng`, §5.3), `--searxng` (default `http://127.0.0.1:8888`), `--brave-key-file` (§5.2), `--log-queries` (default `true`, §10) |
| `holocron search QUERY` | one search through the configured provider, the §3.2 JSON on stdout; for debugging. The provider flags as `serve` |
| `holocron context QUERY [--tokens N] [--urls N]` | one §3.5 request, its JSON on stdout. The provider flags as `serve` |
| `holocron fetch URL [--start N] [--max-chars N] [--text]` | one fetch with the guard and extraction, the §3.3 JSON (or only the text with `--text`) on stdout; for debugging and §7.2's comparison |
| `holocron install [--dry-run]` | set the host up (§9) |
| `holocron check [BASE_URL]` | the acceptance suite against a running server (§12) |
| `holocron version` | the version, the Go toolchain, the pinned SearXNG image |

`main` only parses arguments and dispatches. Expected failures exit
non-zero with one line on stderr; nothing panics on input.

## 5. Search providers

```go
type Provider interface {
    Name() string
    Search(ctx context.Context, r SearchRequest) (Results, error) // query, n, freshness
    Health(ctx context.Context) error
}

// A provider with context (§3.5) also implements:
type Contexter interface {
    Context(ctx context.Context, r ContextRequest) (Context, error) // query, tokens, urls, freshness
}
```

`Results` and `Context` carry the normalized items, the provider that
answered and `engines_failed`; the HTTP layer applies §3.2's and §3.5's
bounds once, for every provider. A provider's failures map to
`no_engines`, `upstream_timeout` or, for a query it refuses,
`bad_request`. A metered provider also reports its quota (§3.4).

### 5.1 `searxng` (v1)

- Reads SearXNG's JSON API: `GET /search?q=…&format=json&categories=general,it&safesearch=0`
  on the configured base URL, with `time_range=day|week|month|year` for
  §3.2's `freshness` `pd`, `pw`, `pm`, `py` (a date range is not sent:
  SearXNG has none); `results[].url/title/content/engine/publishedDate`
  become §3.2's fields; `unresponsive_engines[][0]` becomes
  `engines_failed`.
- Health is SearXNG's `GET /healthz` → 200.
- The SearXNG instance is holocron's own, installed by §9: only holocron
  talks to it.

### 5.2 `brave`

SearXNG scrapes other engines, and from a shared address (a home
connection behind carrier-grade NAT) most of them rate-limit or challenge
it: §9.1's measurement saw Brave and Wikipedia answer 429, DuckDuckGo a
CAPTCHA on 25 of 30 queries, Mojeek and GitHub 403. An API with its own
key is not limited by the address.

- **The plan** (Brave's dashboard, 2026-10-16): the Brave Search API
  needs an API key from `https://api-dashboard.search.brave.com`. Its
  *Search* plan is prepaid: US$5 per 1,000 requests, with US$5 of free
  credit every month (about 1,000 requests a month), capacity 50 requests
  a second; web search, LLM context, news, videos and images. §8.1's
  cache is what keeps the credit: a repeated query within 30 minutes
  costs nothing. The dashboard can also cap spend with a monthly usage
  limit; what Brave answers at the cap is not yet seen, and a 402 or a
  quota 429 holds Brave back so the chain answers from SearXNG (below).
- **Search**: `GET https://api.search.brave.com/res/v1/web/search` with
  `q`, `count=10`, `safesearch=off`, `text_decorations=false` (no
  highlight markers), `result_filter=web` (no news, video or FAQ
  blocks), `extra_snippets=true`, and `freshness` when the request has
  one; headers `X-Subscription-Token`, `Accept: application/json`,
  holocron's `User-Agent`, and `Cache-Control: no-cache` when the client
  sent it (§8.1), since Brave otherwise answers from its own cache.
  `country` and `search_lang` are not sent: Brave's defaults (`US`, `en`)
  suit English technical queries; revisited if answers look too
  US-centred. `web.results[].title` → `title`, `.url` → `url`,
  `.description` → `snippet` (HTML entities decoded), `.page_age` (an
  ISO date-time) → `published` (its date), `.extra_snippets[]` →
  `extra_snippets`, `engine` `brave`; `query.altered`, when it differs
  from the query, → `altered_query`.
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
  | 429 with a second still to wait (`X-RateLimit-Reset` first value ≤ 1) | one retry after it, within the budget; then `no_engines` |
  | 429 with the month's quota at 0, or 402 | `no_engines` "brave: quota or credit used up"; no call until the reset Brave gave (1 h when it gave none) |
  | 401, 403, or 422 with `error.code` `SUBSCRIPTION_TOKEN_INVALID` (what a wrong key gets, measured 2026-10-16) | `no_engines` "brave refused the key"; no call for 10 minutes |
  | 400 or 422 `VALIDATION` naming `q` (`meta.errors[].loc` `["query", "q"]`) | `bad_request` with Brave's messages |
  | 400 or 422 about another parameter (holocron's request) | `no_engines` |
  | 5xx, connection, TLS, an unreadable answer | `no_engines` |
  | the budget passes | `upstream_timeout` |

  Brave bills only successful requests, so a failure costs nothing. At
  50 requests a second against §8's 8 slots, holocron does not pace its
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
  what Brave bills, its successful answers: §3.4's `billed` and §10's
  `brave_billed`.
- **The key**: read once at startup from the file named by
  `--brave-key-file` (one line, trimmed, not empty; a missing or empty
  file stops `serve`), and sent only as `X-Subscription-Token`. Never in
  a flag value, the environment, a log line or an error message. On the
  host the file is `/etc/holocron/brave.key`, delivered to the service by
  systemd's `LoadCredential=` (§9.2).
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

### 5.3 Chaining

`--provider` takes an ordered list, e.g. `brave,searxng`. The next
provider is asked only when one answers `no_engines` (a refused key, a
used-up quota, Brave down; for SearXNG, no results while engines failed),
within the same budget. `provider` in the answer names the one that
answered, and each provider that failed before it is listed in
`engines_failed` by its name, ahead of the answering provider's own
failed engines. For `/v1/context` only the providers with context are
asked, in the same order. §8.1's key is the list's name, so a chain's
entries are never mixed with a single provider's.

A host with Brave's key runs `brave,searxng`: from a shared address
SearXNG rarely helps (§9.1), but when the credit runs out a partial answer
beats `no_engines`. `holocron install` chooses the chain by whether the key is
on the host (§9).

### 5.4 Later providers

Another paid API (Kagi, …) is a new implementation and a `--provider`
name, its key delivered as Brave's is.

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

## 7. Extraction

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
background; its request slot (§8) is freed only when it does, so abandoned
work still counts against concurrency and memory.

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
  or idle client cannot hold the server.
- **8 request slots** shared by search and fetch. A request waits at most
  2 s for one, then `busy`. `/v1/health` takes no slot.
- Every request runs under a `context` with its budget (§3.2, §3.3);
  outbound HTTP, DNS and extraction all observe it.
- A panic in a handler is recovered into `internal` with a log line;
  the process keeps serving.
- Memory: at most 8 bodies of 5 MiB in flight plus their parse trees;
  `GOMEMLIMIT` is set below the unit's `MemoryMax` (§9) so the runtime
  collects before the kernel kills. Measured 2026-10-16 (§7.2's pages):
  about 125 MiB live once the first extraction has run, 92 MiB of it
  go-py3langid's language model, which go-trafilatura loads on every
  extraction with no option to skip it; then about 13× the body live per
  extraction (SQLite's 1.9 MiB page: 28 MiB; a 3.9 MiB page: 49 MiB), so
  a 5 MiB body holds about 75 MiB with its raw and decoded copies. Eight
  at once: about 725 MiB, hence §9.2's limits. §8.1's cache adds at most
  64 MiB of text: about 790 MiB in the worst case, under `GOMEMLIMIT`
  (800 MiB), with `MemoryMax` (1 GiB) above both.

### 8.1 Cache

A shared address is rate-limited or challenged by most engines (§9.1's measurement), agents
repeat queries, and they page through fetched documents (`start=2000`,
`4000`, …). An in-memory cache cuts the traffic to the engines and makes
paging one upstream fetch. It lives in the process only: no disk, no
Valkey; a restart empties it.

- **Behind validation.** A request's parameters, and for a fetch the
  URL's length and §6's step 1 (scheme, credentials, names, literal
  addresses, port), are checked before the lookup. An error answer is the
  same with or without the cache, and a hit never returns what a miss
  would refuse.
- **search** and **context** (§3.5): keyed by the provider's name (a
  chain's, §5.3) and the query trimmed, its
  whitespace collapsed, lowercased. The provider is asked for 10 results
  whatever `n`, and the entry is §3.2's answer with up to 10 results and
  its `engines_failed`; an answer for any `n` is its first `n` results,
  with this request's own `query`. The key also holds `freshness`, and a
  context entry's `tokens` and `urls`. Fresh for **30 minutes**, or **5 minutes** when
  `engines_failed` is not empty.
- **fetch**: keyed by the URL with its scheme and host lowercased and its
  fragment dropped; the query is kept. The entry is the extracted
  document (text, title, final URL, status, content type, the 5 MiB cut),
  not the raw body; `start` and `max_chars` are a window into it, and
  `url` is this request's own. Fresh for **15 minutes**.
- **Failures**: a fetch's `upstream_failed` (a 404, a DNS failure) and
  `unsupported_content` are kept **1 minute**. `no_engines`,
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
  hours** after it was stored. When it would answer `no_engines` (§3.1),
  an entry
  past its freshness but within those 24 hours is answered instead,
  marked `stale`. Never for another error, never for a fetch.
- **`Cache-Control: no-cache`** on a request skips the lookup (stale
  entries too), asks upstream, and replaces the entry with what it
  answers; it is also sent on to Brave (§5.2). §9.1's engine
  measurement sends it.
- **Storage**: least-recently-used maps bounded by bytes (a mutex and
  `container/list`): **8 MiB** for searches, **8 MiB** for contexts,
  **48 MiB** for documents, 64 MiB in all. An entry's size is the bytes of its strings plus a fixed
  overhead; an entry larger than its map is not stored. An expired entry
  is dropped when a lookup meets it, or evicted with the least recently
  used.
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
| `holocron_brave_billed_total` | counter | | Brave's billed answers, counted as §10's `brave_billed`: once per call |
| `holocron_fallbacks_total` | counter | `provider`, `reason` | per request, as §10's `fallback` |
| `holocron_engines_failed_total` | counter | `engine` | each name in an answer's `engines_failed`, once per upstream call (not on hits); a chained provider that failed is listed by its name (§5.3) |
| `holocron_provider_up` | gauge | `provider` | 1 or 0, §3.4's state; Brave's check makes no call |
| `holocron_cache_entries`, `holocron_cache_bytes` | gauge | `kind` (`search`, `context`, `fetch`) | §8.1's maps |
| `holocron_cache_lookups_total` | counter | `kind`, `result` (`hit`, `miss`, `stale`) | each request that reached the cache |
| `holocron_slots_in_use`, `holocron_slots_max` | gauge | | §8's request slots |
| `holocron_busy_total` | counter | | `busy` answers |
| `holocron_build_info` | gauge | `version`, `goversion`, `searxng_image` | always 1 |
| `go_goroutines`, `go_heap_objects_bytes`, `go_memory_total_bytes`, `go_gc_cycles_total` | gauge, gauge, gauge, counter | | from `runtime/metrics` |

Counters start at 0 when holocron starts; the time-series store handles
the reset.

## 9. Deployment: `holocron install`

Run as root on a systemd Linux host with Podman installed (the host's
packages and firewall are the deployment's job, not holocron's). The
binary is already at `/usr/local/bin/holocron`. Idempotent: run it again
after copying a new binary. `--dry-run` prints each file it would change
and each command it would run, and changes nothing.

1. **Checks**: Linux, root, systemd as init, `systemctl`, `podman` and
   `useradd` on `PATH`; a failed check names what is missing. A dry run
   reports a failed check and goes on, so it previews anywhere.
2. **User**: the system user and group `holocron` (no home, no shell).
3. **Providers**: when `/etc/holocron/brave.key` exists (put there by
   the deployment, never by holocron), the unit runs the chain
   `brave,searxng` (§5.3) and the file is made 0600 if it is not (systemd
   reads it as root, §9.2); otherwise `searxng` alone. The key file is the switch: adding or
   removing it and running `install` again changes the provider, and an
   upgrade's `install` keeps what the host has. A dry run says which.
   **SearXNG** (§9.1): `/etc/searxng/settings.yml` (mode 0600) with a
   secret generated once from `crypto/rand` and kept across re-runs;
   `/etc/containers/systemd/searxng.container`; `podman pull` of the
   image by the digest compiled into this release.
4. **holocron**: `/etc/systemd/system/holocron.service` (§9.2).
5. **Start**: `systemctl daemon-reload`, then restart `searxng.service`,
   enable and restart `holocron.service`.
6. **Wait** up to 60 s for `/v1/health` to report every provider `ok`,
   then print it and one three-result search. On timeout, print the last
   40 journal lines of both units and exit non-zero.

The files are embedded in the binary (`go:embed`), so a holocron release
is its own complete install.

### 9.1 SearXNG under Podman

- **Why Podman**: SearXNG publishes no versioned releases and no
  packages; the upstream path is the container image
  `docker.io/searxng/searxng`, tagged daily by date and commit, which can
  be pinned by digest. A quadlet is a plain systemd unit, no daemon, and
  removing SearXNG later (a paid provider only) is deleting a unit and an
  image. Docker's own iptables rules would fight the host's firewall.
- **Image**: pinned by digest; the pin in this release is
  `docker.io/searxng/searxng@sha256:ad273460b12390ab1ad9c6ea630dafed39045ab0b9096a85ca31f5246517c670`
  (tag `2026.10.15-b00212a83`, Docker Hub, read 2026-10-16). SearXNG's
  engine scrapers break and are fixed often, so the pin is refreshed at
  least monthly, and whenever `engines_failed` climbs; a bump is a
  holocron release.
- **Quadlet**: `Network=host` so the host firewall sees SearXNG's egress
  like any process's (no container bridge); `GRANIAN_HOST=127.0.0.1`,
  `SEARXNG_PORT=8888`, `SEARXNG_BASE_URL=http://127.0.0.1:8888/`;
  `/etc/searxng` and `/var/cache/searxng` mounted; `NoNewPrivileges=true`;
  `Restart=always`. The image's entrypoint owns `/etc/searxng` as its
  `searxng` user (uid 977).
- **Settings**, merged over SearXNG's defaults (`use_default_settings`):
  - `search.formats: [html, json]`; `safe_search: 0`; no autocomplete;
  - `server.limiter: false` (no Valkey: holocron is the only client),
    `public_instance: false`, `image_proxy: false`, the generated
    `secret_key`;
  - `outgoing.request_timeout` 4 s, `max_request_timeout` 8 s, below the
    10 s search budget;
  - engines (`keep_only`; names from SearXNG's `settings.yml`, read
    2026-10-16): general web from Brave, DuckDuckGo, Mojeek, Startpage
    (the last two turned on: inactive by default) and Wikipedia; for code
    GitHub, Stack Overflow, Ask Ubuntu, Super User, MDN, PyPI, crates.io,
    npm, pkg.go.dev (the last three turned on), Docker Hub and the Arch
    wiki. Google and Bing stay off (disabled by SearXNG's defaults).
- **Measure before accepting the engine list**: 30 representative queries
  (coding, error messages, general) from the deployed host, counting
  results and `engines_failed` per engine, each sent with
  `Cache-Control: no-cache` so it reaches the engines (§8.1). An engine
  that fails often is dropped, and if too few remain that is the case for
  a paid provider (§5.2).
- **Measured** (2026-10-16, a host behind carrier-grade NAT, 30 queries
  3 s apart): every query returned 10 results, but from few engines.
  Queries with a result per engine: MDN 19, Stack Overflow 19, Docker
  Hub 17, Startpage 12, Super User 10, Mojeek 8, crates.io 7, Ask Ubuntu
  6, DuckDuckGo 5, pkg.go.dev 2, GitHub 1, Brave 0, Wikipedia 0. Brave
  and Wikipedia answered 429 from the first request, DuckDuckGo a CAPTCHA
  on 25 of 30, Mojeek and GitHub 403; SearXNG suspends a failing engine
  for 180 s. General web search rested on Startpage alone: the case for
  Brave (§5.2) on such a host.

### 9.2 holocron's unit

`User=holocron`; `ExecStart=/usr/local/bin/holocron serve --listen 0.0.0.0:8080`,
with Brave (§9 step 3) `--provider brave,searxng --brave-key-file
${CREDENTIALS_DIRECTORY}/brave-key` and
`LoadCredential=brave-key:/etc/holocron/brave.key`: systemd copies the
key into a directory only the service reads, so the `holocron` user never
reads `/etc/holocron`;
`Wants=`/`After=` `network-online.target searxng.service`;
`Restart=on-failure`; `MemoryMax=1G`, `Environment=GOMEMLIMIT=800MiB`
(§8's measurement),
`TasksMax=64`; and the sandbox: `ProtectSystem=strict`, `ProtectHome`,
`PrivateTmp`, `PrivateDevices`, `NoNewPrivileges`, an empty
`CapabilityBoundingSet`, `ProtectKernelTunables`/`Modules`/`Logs`,
`ProtectControlGroups`, `ProtectClock`, `ProtectHostname`,
`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, `RestrictNamespaces`,
`RestrictRealtime`, `LockPersonality`, `SystemCallArchitectures=native`.
It listens on all addresses because the deployment's firewall decides who
reaches it.

### 9.3 The recommended host

What `install` does not do is the host: a small VM of its own (2 vCPU,
2 GiB, 20 GiB) with Podman from the distribution, its firewall enforced
outside the guest (the hypervisor's per-VM firewall) with outbound an
allow-list: DNS to the resolver, TCP 80 and 443 to the public internet,
NTP, and every private range dropped. Inbound only from the reverse proxy
or the clients to `:8080`, and SSH from an admin address. Never reachable
from the public internet: with no authentication it would be an open
search and fetch proxy. The README walks through it.

## 10. Logging

One line per request on stdout (the journal, under systemd), `log/slog`
text format, its level by whose fault a failure is: `INFO` for success
and the client's own mistakes (`bad_request`, `not_found`,
`method_not_allowed`, `unsupported_content`, `not_available`); `WARN` for
the upstream's failures and holocron's capacity (`upstream_failed`,
`upstream_timeout`, `no_engines`, `busy`) and for a refused address
(`blocked_address`: something asked for one); `ERROR` for `internal`.
An error line carries `code` and, while `--log-queries` is on, `error`:
the message the client got (it can quote the request). The line has
the endpoint, the status and error code, the duration, the provider or
the fetched host, **the query or the full URL**, the final URL
after redirects, `cache=hit|miss|stale` (§8.1) for a search or fetch that
reached the cache, and the client's `User-Agent`. `provider` is the
configured provider or chain (`brave,searxng`). A search or context line
also says how it was answered, so Brave's use is told from SearXNG's:

| Field | Value |
| --- | --- |
| `answered_by` | the provider whose results were returned (on a hit, the one stored with the entry); absent on an error |
| `fallback` | `<provider>:<reason>` for each provider the chain moved past (§5.3), comma-separated: `quota` (used up, or a 402), `key_refused`, `rate_limited`, `no_results` (none while its engines failed), `error` (anything else); only on a request that asked upstream |
| `results` | the results or sources returned |
| `engines_failed` | the answer's `engines_failed`, comma-separated, when not empty |
| `brave_billed` | the successful (billed) Brave answers this request's call got, on exactly one line per call: a shared call (§8.1) is counted on the first of its requests to be logged, so summing `brave_billed` over the log counts Brave's bill |
| `brave_remaining`, `brave_resets_in_s` | Brave's monthly remaining requests and the seconds to their reset, from the headers of Brave's answer to this request (§5.2), only on a plan with a monthly limit (not a prepaid one) |

`--log-queries=false` leaves out the query and the URLs (keeping the host). Response bodies are
never logged. Startup logs the version, the listen address, the
providers and the SearXNG pin. `/v1/metrics` (§8.2) is not logged: a
scrape every 15 s would drown the rest.

Queries are logged by default because holocron serves its operator's
own agents: the log is how a search the agent made is debugged and how §9.1's
measurement is read.

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
  internal/api/        the contract's types, error codes, handlers, slots
  internal/cache/      §8.1: the byte-bounded LRU and the shared upstream call
  internal/metrics/    §8.2: counters, a histogram and the text exposition
  internal/guard/      §6: URL checks and the dial-time address check
  internal/fetch/      the guarded client: redirects, body cap, content types
  internal/extract/    §7: charset, HTML to Markdown, fallback, title
  internal/provider/   the Provider interface and chaining (§5.3); searxng/, brave/
  internal/install/    §9 and its embedded files
  internal/check/      §12's acceptance suite
  ```

- **Checks** (`make check`, before every commit, and in CI on every push
  to `main`): `gofmt -l` empty,
  `go vet ./...`, `go mod verify`, `go test ./...`. Tests are offline: a
  fake SearXNG, a fake Brave and fake sites on `httptest`, the guard tested through an
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

Against a running server, one line per check, non-zero exit if any fails:

1. `/v1/health` → every provider `ok`.
2. `/v1/search?q=zig std.http.Client` → ≥ 3 results, each field within
   its bound; `n=20` clamped to 10; `q=` empty → `bad_request`.
3. `/v1/fetch` of PEP 8 with `max_chars=2000` → 2,000 characters starting
   with a heading, `chars` > 2,000, `truncated`; `start=2000` pages on.
4. Guard, each → `blocked_address`: `http://192.168.1.20:8443/`,
   `http://nas.home.arpa:8443/`, `http://127.0.0.1:8888/`,
   `http://192.168.1.1.nip.io/`, and a public redirect to
   `http://192.168.1.1/` (`https://httpbin.org/redirect-to?url=…`).
5. A PDF → `unsupported_content`; a 404 → `upstream_failed`; port 8443 →
   `bad_request`.
6. `/v1/context?q=zig std.http.Client` → when health lists
   `/v1/context`, ≥ 1 source with snippets, each field within its
   bound; otherwise `not_available`.

Items 2–6 use public sites (and item 6 one Brave request); a failure
there names the site, so an outage elsewhere is told apart from a
holocron bug.
