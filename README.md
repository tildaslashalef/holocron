# holocron

Web search and page fetch for AI agents (coding agents among them): one
static Go binary with a small HTTP JSON contract, meant for a trusted LAN
or tailnet. It was made for [nuclis](https://github.com/tildaslashalef/nuclis),
and any client that speaks the contract can use it.

- `GET /v1/search?q=…&n=…&freshness=…` — normalized results from a search provider:
  a self-hosted SearXNG, the Brave Search API, or Brave with SearXNG as
  its fallback.
- `GET /v1/context?q=…&tokens=…&urls=…&freshness=…` — passages from several pages
  that answer a query, in one call (Brave's LLM Context); `not_available`
  with SearXNG alone.
- `GET /v1/fetch?url=…&start=…&max_chars=…` — one public page as clean
  Markdown, paged by character offset. An address guard keeps every fetch
  off private, loopback and link-local addresses, at every redirect.
- `GET /v1/health` — the version, each provider's state, the endpoints
  this server answers and Brave's remaining quota.
- `GET /v1/metrics` — Prometheus text for monitoring; no queries or URLs in it.

Answers are cached in memory for minutes, so a repeated query or the next
page of a document costs no upstream call; `Cache-Control: no-cache`
refreshes ([SPEC.md § 8.1](SPEC.md#81-cache)).

The design, the contract and what `install` does are in
[SPEC.md](SPEC.md), the single source of truth.

## Commands

| Command | Does |
| --- | --- |
| `holocron serve` | the HTTP server |
| `holocron search QUERY` / `holocron context QUERY` / `holocron fetch URL` | one request without the server, JSON on stdout |
| `holocron install` | on a systemd host with Podman: holocron's unit and SearXNG as a quadlet |
| `holocron check [BASE_URL]` | the acceptance suite against a running server |
| `holocron version` | the version, toolchain and pinned SearXNG image |

## Deploying

The recommended setup is holocron and SearXNG on a **small VM of their
own**, SearXNG running under **Podman** as a systemd quadlet, which
`holocron install` writes for you:

```text
agent (LAN or tailnet)
  └─ https://search.example.com      your reverse proxy, TLS, LAN-only name
       └─ <vm>:8080   holocron.service   /v1/search /v1/context /v1/fetch /v1/health
            ├─ 127.0.0.1:8888   searxng.service (Podman quadlet) → search engines
            └─ fetch → the public internet only
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
- **Hostile HTML is parsed on a disposable guest.**

### 1. The VM

2 vCPU, 2 GiB of memory and 20 GiB of disk are plenty. Any systemd Linux
distribution with Podman will do; on Debian or Ubuntu:

```bash
sudo apt-get install -y --no-install-recommends podman
```

holocron itself is one static binary with no runtime dependencies.

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
| out | TCP 80, 443 | engines, fetches, the image pull, packages |
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

### 4. Install

```bash
sudo holocron install --dry-run
```

```bash
sudo holocron install
```

It creates the `holocron` system user, writes SearXNG's settings (with a
secret generated on the host) and quadlet, pulls the SearXNG image by the
digest pinned in this release, writes and starts `holocron.service`, and
ends on a healthy `/v1/health` and one search. It is idempotent.

### 5. Expose it on your network only

Put a reverse proxy with TLS in front of `<vm>:8080` under a name your
LAN or tailnet resolves. **Never route it to the public internet:**
holocron has no authentication, so it would be an open search and fetch
proxy.

### 6. Check it

```bash
holocron check https://search.example.com
```

Releases carry Linux binaries only; elsewhere, run `check` from a build
of the same tag (`make build`).

### Upgrading

Steps 3 and 4 again with the new release. A new SearXNG image arrives as
a new holocron release.

### Brave Search

SearXNG scrapes other engines, and from a home connection (often behind
carrier-grade NAT, shared with other customers) many of them rate-limit
or challenge it. The [Brave Search API](https://api-dashboard.search.brave.com)
has its own key and is not limited by your address, and it adds
`/v1/context`.

It needs a **Brave Search API key**. The *Search* plan is prepaid at
US$5 per 1,000 requests and includes **US$5 of free credit every month**,
about 1,000 requests; holocron's cache keeps repeated queries free, and
the dashboard can cap the monthly spend.

Put the key on the VM, never in a command line or the environment:

```bash
sudo install -d -m 0700 /etc/holocron && sudo bash -c 'umask 077; read -rsp "Brave key: " k; echo; printf "%s\n" "$k" > /etc/holocron/brave.key'
```

```bash
sudo holocron install
```

The key file is the switch: with it, the unit runs the chain
`brave,searxng` (SearXNG answers when Brave's credit runs out) and serves
`/v1/context`; remove the file and run `install` again to go back to
SearXNG alone. systemd hands the key to the service with
`LoadCredential=` ([SPEC.md § 9.2](SPEC.md#92-holocrons-unit)).

### Monitoring and logs

- Scrape `http://127.0.0.1:8080/v1/metrics` from the VM
  ([SPEC.md § 8.2](SPEC.md#82-metrics-get-v1metrics)), and alert on a
  failed `holocron.service` or `searxng.service`.
- holocron logs one line per request to the journal, **including the
  query or the fetched URL** and the client's `User-Agent`;
  `--log-queries=false` leaves them out
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
