# holocron

Web search and page fetch for AI agents (coding agents among them): one
static Go binary with a small HTTP JSON contract, meant for a trusted LAN
or tailnet. It was made for [nuclis](https://github.com/tildaslashalef/nuclis),
and any client that speaks the contract can use it.

The contract is the same whichever provider answers. Each endpoint has
its own chain of providers, tried in order, and the answer names the one
that answered:

| Endpoint | Does | Default chain |
| --- | --- | --- |
| `GET /v1/search?q=…&n=…&freshness=…` | ranked results, each with a snippet and up to five passages | Parallel Search, then Brave |
| `GET /v1/context?q=…&tokens=…&urls=…&freshness=…` | passages from several pages that answer a query, in one call, within a token budget | Parallel Search, then Brave's LLM Context |
| `GET /v1/fetch?url=…&q=…&start=…&max_chars=…` | one public page (HTML or PDF) as clean Markdown, paged by character offset; with `q`, only the parts that answer it | Parallel Extract, then the local fetcher |
| `GET /v1/health` | the version, each provider's state, the routes, what was billed, Brave's quota | |
| `GET /v1/metrics` | Prometheus text; no queries or URLs in it | |
| `GET /` and `GET /v1/stats` | a dashboard: traffic, latency, cost per provider, cache, recent requests | |

A provider joins a chain only when it has a key, so with no key at all
holocron still fetches pages locally, and search and context answer
`not_available`. An address guard keeps every fetch off private, loopback
and link-local addresses: at every redirect for the local fetcher, and
before a URL is handed to a remote extractor.

Answers are cached in memory for minutes, so a repeated query or the next
page of a document costs no upstream call; `Cache-Control: no-cache`
refreshes ([SPEC.md § 8.1](SPEC.md#81-cache)).

The design, the contract and what `install` does are in
[SPEC.md](SPEC.md), the single source of truth.

## Providers

| Provider | Serves | Price | Key |
| --- | --- | --- | --- |
| [Parallel](https://platform.parallel.ai) | search, context, fetch | Search US$1 per 1,000 (fast, the default) or US$5 (advanced, used for context); Extract US$1 per 1,000 URLs | `parallel` |
| [Brave Search API](https://api-dashboard.search.brave.com) | search, context | US$5 per 1,000 requests, with US$5 of free credit a month | `brave` |
| local | fetch | free; HTML and plain text only, on the VM's own connection | none |

A provider that is out of credit, refuses its key, is rate-limited or
too slow for its share of the request's time is skipped, and the next
one answers; `providers_failed` in the answer lists the ones skipped. The
dashboard and `/v1/metrics` show what each provider billed and its
estimated cost.

## Using it from an agent's tools

- **Search, then fetch.** `/v1/search` with a small `n` (5 is plenty)
  finds pages; `/v1/fetch` reads the one worth reading. Each result's
  `snippets` are often enough to answer without a fetch.
- **Ask a fetch a question.** `q` on `/v1/fetch` returns only the parts
  of the page that answer it (`"focused": true`), a few thousand
  characters instead of a whole manual. Without `q`, page with `start`
  and `max_chars` while `truncated` is true; the next pages come from the
  cache.
- **Use `/v1/context` for a research question** that wants several
  sources at once, instead of a search and several fetches.
- **Read the error code, not the message.** `blocked_address`,
  `bad_request` and `unsupported_content` will not change on a retry;
  `busy`, `upstream_timeout` and `provider_unavailable` may.
- **Expect nulls.** `status` and `content_type` on a fetch are null when
  a remote extractor does not say.

## Commands

| Command | Does |
| --- | --- |
| `holocron serve` | the HTTP server, with the dashboard at `/` |
| `holocron search QUERY` / `holocron context QUERY` / `holocron fetch URL [--q QUESTION]` | one request without the server, JSON on stdout |
| `holocron key set\|rm PROVIDER` | store a provider's key, encrypted with systemd-creds, or remove it |
| `holocron install` | on a systemd host: holocron's user, unit and keys |
| `holocron check [BASE_URL]` | the acceptance suite against a running server |
| `holocron version` | the version and toolchain |

`--search`, `--context` and `--fetch` set each endpoint's chain (for
example `--search brave` or `--fetch local`); `holocron help` lists every
flag.

## Deploying

The recommended setup is holocron on a **small VM of its own**:

```text
agent (LAN or tailnet)
  └─ https://search.example.com      your reverse proxy, TLS, LAN-only name
       └─ <vm>:8080   holocron.service   /v1/search /v1/context /v1/fetch /v1/health /
            ├─ api.parallel.ai, api.search.brave.com
            └─ local fetch → the public internet only
```

Why a VM and not the workstation the agent runs on:

- **The fetcher is an SSRF risk.** The model chooses the URL, and a page
  it has just read can tell it which one. On a flat home network an
  unfenced fetcher could reach the hypervisor's UI, the router and every
  unauthenticated service, and hand what it read back to the model.
  holocron's address guard refuses non-public addresses; the VM's
  firewall is the fence behind it.
- **Egress in one place**, from a guest whose firewall you control, not
  from the machine holding the agent's workspace and credentials.
- **Hostile HTML is parsed on a disposable guest**, and the provider keys
  live there, not in the agent's environment.

### 1. The VM

1 vCPU, 1 GiB of memory and 10 GiB of disk are plenty. Any systemd Linux
distribution will do; systemd 250 or later encrypts the keys
(`systemd-creds`). holocron is one static binary with no runtime
dependencies.

### 2. The firewall, before anything listens

Enforce it **outside the guest**, on the hypervisor's per-VM firewall
(Proxmox VE, libvirt's nwfilter, a cloud security group), so a
compromised guest cannot turn it off. Default policy `DROP` both ways,
then:

| Direction | Allow | For |
| --- | --- | --- |
| in | TCP 8080 from the reverse proxy (or your clients) | holocron |
| in | TCP 22 from your admin machine | SSH |
| out | UDP/TCP 53 to your DNS resolver | lookups |
| out | **drop** `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16`, `fc00::/7`, `fe80::/10` | nothing else on the private network |
| out | TCP 80, 443 | providers, fetches, packages |
| out | UDP 123 | NTP |

The order matters: the private ranges are dropped before 80/443 is
allowed. From the guest, a `curl` to another machine on your network
should now time out, and `curl https://example.com` should answer.

### 3. The binary

Download a release on your workstation, check it, and copy it over:

```bash
gh release download vX.Y.Z -R tildaslashalef/holocron -p holocron-linux-amd64 -p SHA256SUMS
```

```bash
shasum -a 256 -c --ignore-missing SHA256SUMS
```

```bash
scp holocron-linux-amd64 SHA256SUMS <user>@<vm>:
```

Then on the VM:

```bash
sha256sum -c --ignore-missing SHA256SUMS && sudo install -m 0755 -o root -g root holocron-linux-amd64 /usr/local/bin/holocron
```

Use `holocron-linux-arm64` on an arm64 guest.

### 4. The keys

Each key is typed at a hidden prompt on the VM, never on a command line
or in the environment:

```bash
sudo holocron key set parallel
```

```bash
sudo holocron key set brave
```

`key set` encrypts the key with `systemd-creds` into
`/etc/credstore.encrypted/holocron.<provider>-key`, readable by root only,
and systemd decrypts it for the service alone (`LoadCredentialEncrypted=`).
On a host without `systemd-creds` it writes `/etc/holocron/<provider>.key`
with mode 0600 instead. Either way it then runs `install`. `holocron key rm
PROVIDER` takes a provider out of every chain
([SPEC.md § 5.5](SPEC.md#55-keys-holds-and-bills)).

### 5. Install

```bash
sudo holocron install --dry-run
```

```bash
sudo holocron install
```

It creates the `holocron` system user, finds the keys, sets each chain
from them, writes and starts `holocron.service`, and ends on a healthy
`/v1/health` and one search. It is idempotent. On a host that ran 0.1,
it also removes SearXNG: its quadlet, settings, cache and image. Podman
is the host's package and stays; remove it yourself when nothing else
uses it.

### 6. Expose it on your network only

Put a reverse proxy with TLS in front of `<vm>:8080` under a name your
LAN or tailnet resolves. **Never route it to the public internet:**
holocron has no authentication, so it would be an open search and fetch
proxy that spends your provider credit.

### 7. Check it

```bash
holocron check https://search.example.com
```

Releases carry Linux binaries only; elsewhere, run `check` from a build
of the same tag (`make build`).

### Upgrading

Steps 3 and 5 again with the new release.

### Monitoring and logs

- The dashboard at `https://search.example.com/` shows each provider's
  state, traffic, latency, cost and the last requests; `--dashboard=false`
  turns it off ([SPEC.md § 8.3](SPEC.md#83-the-dashboard-get--and-get-v1stats)).
- Scrape `http://127.0.0.1:8080/v1/metrics` from the VM
  ([SPEC.md § 8.2](SPEC.md#82-metrics-get-v1metrics)), and alert on a
  failed `holocron.service` or on `holocron_cost_usd_total` rising faster
  than you expect.
- holocron logs one line per request to the journal, **including the
  query or the fetched URL**, the client's `User-Agent`, the provider
  that answered and what it cost; `--log-queries=false` leaves queries
  and URLs out of the log and the dashboard
  ([SPEC.md § 10](SPEC.md#10-logging)).

## Building

Go 1.27. `make help` lists the targets.

```sh
make check              # gofmt, go vet, go mod verify, go test
make build              # bin/holocron for this machine
make dist               # dist/: static linux amd64 and arm64 binaries, SHA256SUMS
make release V=X.Y.Z    # tag vX.Y.Z and push it; GitHub builds and publishes
```

Commits go straight to `main`. A release is only ever built by GitHub
Actions from the tag ([SPEC.md § 11](SPEC.md#11-build-dependencies-and-releases)).

## License

[MIT](LICENSE).
