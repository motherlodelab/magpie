# magpie

[![Go 1.26+](https://img.shields.io/badge/go-1.26+-blue.svg)](go.mod)
[![CGO-free](https://img.shields.io/badge/CGO-free-green.svg)](spec.md)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](#license)

A fast, single-binary web scraper for humans and agents: fetch a page,
strip the boilerplate, and get clean Markdown — plus optional structured
extraction through any LLM provider. Pure Go, zero CGO, no runtime
dependencies.

```
fetch (static → challenge detect → optional real browser)
  → clean (boilerplate removal → GFM markdown + JSON-LD sidecar)
    → extract (schema-driven, provider-agnostic, cost-capped)
```

**Highlights:** SSRF-guarded fetching · TLS impersonation · proxy pool
with failover · bot-challenge detection · PDF support · sitemap mapping ·
scoped crawling · zero-LLM verticals · 12-tool MCP server · WASM plugins.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Usage](#usage)
  - [Command reference](#command-reference)
  - [Global flags](#global-flags)
  - [Environment variables](#environment-variables)
  - [Exit codes](#exit-codes)
  - [Providers](#providers)
  - [Page formats, PDF & screenshots](#page-formats-pdf--screenshots)
  - [Network & security](#network--security)
  - [TLS impersonation](#tls-impersonation)
  - [Search](#search)
  - [Bot challenges](#bot-challenges)
- [MCP server](#mcp-server)
- [Custom builds & WASM plugins](#custom-builds--wasm-plugins)
- [Development](#development)
- [Project layout](#project-layout)
- [Contributing](#contributing)
- [Credits](#credits)
- [License](#license)

## Prerequisites

- **Go 1.26+** (single static binary, no CGO, no runtime dependencies)
- **Chrome/Chromium** — only for `--render browser`, screenshots, and
  the `browser` test tag; everything else is pure Go
- **API key** — only for LLM extraction or BYOK search providers;
  cleaning, mapping, crawling, verticals, and brand detection need none

## Installation

1. Clone the repository:

   ```bash
   git clone https://github.com/motherlodelab/magpie.git
   cd magpie
   ```

2. Build:

   ```bash
   go build -o magpie ./cmd/magpie
   ```

3. (Optional) Verify the hermetic test suite passes with no network,
   browser, or keys:

   ```bash
   go test ./...
   ```

Cross-compile static binaries (Windows / Linux / macOS) with
`CGO_ENABLED=0` — see [Development](#development).

## Quick start

```bash
# Cleaned markdown only (no LLM call)
./magpie scrape https://example.com/article --render static

# Structured extraction
./magpie scrape https://example.com/product --schema testdata/extract/price.yaml \
  --provider openai --model gpt-4o-mini

# From stdin, no fetch
echo "$HTML" | ./magpie extract --schema testdata/extract/price.yaml --content-type html

# Config (keys redacted)
MAGPIE_EXTRACT_PROVIDER=ollama ./magpie config show
./magpie config set-key anthropic
```

API keys resolve as: `--api-key` flag > `MAGPIE_<PROVIDER>_API_KEY` env >
OS keyring > config file.
(Dashes become underscores: `opencode-go` → `MAGPIE_OPENCODE_GO_API_KEY`.)

**Common use-cases:**

- `scrape` a news article to Markdown for archiving or summarising
- `scrape --schema` a product page to typed JSON for a price tracker
- `batch` 100 URLs to JSONL overnight with bounded concurrency
- `crawl` your docs site into an LLM-ready corpus (`--exporter-cmd`)
- `serve` the whole pipeline to an AI agent over MCP

## Usage

### Command reference

Every command prints to stdout by default; pass `--out file` to write
to a file instead. Run `magpie [command] --help` for the authoritative
flag list.

#### `magpie scrape <url>` — fetch → clean → extract one URL

```bash
magpie scrape https://example.com/article --render static
magpie scrape https://example.com/product --schema price.yaml --provider openai --model gpt-4o-mini
magpie scrape https://example.com/page --page-format screenshot --viewport 1280x800 --out shot.png
magpie scrape https://example.com/docs --include '.main,.content' --exclude 'nav,footer'
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--schema` | yaml/json file | Extract structured data with this JSON Schema |
| `--render` | `auto\|static\|browser` | Fetch mode (`browser` launches real Chrome) |
| `--page-format` | `markdown\|llm\|text\|json\|html\|raw\|screenshot` | Page output shape (default markdown) |
| `--browser` | `chrome\|firefox\|safari\|edge\|ios\|chrome_android\|random` | TLS-impersonating fingerprint (static fetch only) |
| `--header-profile` | `default\|chrome\|firefox\|safari\|edge\|ios\|chrome_android` | Request header bundle (overrides the fingerprint's headers) |
| `--cookies` | `"a=b; c=d"` | Raw Cookie header value |
| `--include` / `--exclude` | CSS selectors (repeatable/comma-separated) | Scrape only matching subtrees / drop matching nodes |
| `--only-main-content` | flag | Main-content only (trafilatura already does this) |
| `--vertical` | `auto` or a name (`magpie vertical --list`) | Zero-LLM typed extractor instead of LLM/generic path |
| `--viewport` | `WxH`, e.g. `1280x800` | Screenshot page size (page-format screenshot only) |
| `--action` | DSL line (repeatable, see [Actions DSL](#actions-dsl)) | Browser interaction executed by rod before capture |
| `--actions` | path | Action file: one action per line, `#` comments (file lines run first) |
| `--capture-xhr` | Go regexp (repeatable) | Capture XHR/fetch response bodies the page loads into the `xhr` envelope array (caps: 50 × 64 KiB, truncated past cap; browser rendering only) |
| `--cdp-url` | `ws://`, `wss://`, `http(s)://` endpoint | Drive a remote/already-running browser over CDP instead of launching one (no local download; overrides `MAGPIE_CDP_URL`; userinfo is redacted from errors) |
| `--lang` | e.g. `fr-CA,fr;q=0.9` | Accept-Language header value (overrides the profile bundle; no control characters) |
| `--provider` / `--model` | see [Providers](#providers) | LLM provider + model for `--schema` extraction |
| `--format` | `json\|jsonl` (default json) | Envelope format |
| `--no-cache` | flag | Bypass the learned selector cache |
| `--out` | path (default stdout) | Write output to a file |

#### `magpie extract` — structured extraction from stdin/file, no fetch

```bash
echo "$HTML" | magpie extract --schema price.yaml --content-type html
cat page.md | magpie extract --schema price.yaml --content-type markdown
magpie extract --prompt "list every price mentioned" < page.md
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--schema` | yaml/json file | JSON Schema to fill (mutually exclusive with `--prompt`) |
| `--prompt` | free text | Plain-text instruction; returns text, no schema |
| `--content-type` | `html\|markdown` (default html) | Input markup flavour |
| `--provider` / `--model` | `…\|auto` | LLM provider + model (`auto` tries keyed providers) |
| `--out` | path (default stdout) | Write output to a file |

#### `magpie batch [urls...]` — scrape ≤100 URLs, zero LLM

One ok/error record per URL; a bad URL yields
`{"ok":false,"error":…}` — never a whole-batch failure.
Markdown only.

```bash
magpie batch https://a.com https://b.com --format jsonl --out pages.jsonl
magpie batch --file urls.txt --concurrency 4
cat urls.txt | magpie batch --file - --render static
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--file` | path (`-` = stdin) | URL list file, one per line |
| `--concurrency` | int (default 8) | Max parallel scrapes |
| `--format` | `jsonl\|json` (default jsonl) | Output shape |
| `--render` | `auto\|static\|browser` | Fetch mode for all URLs |
| `--browser` | `chrome\|firefox\|random` | TLS-impersonating fingerprint |
| `--profile` | `default\|chrome\|firefox` | Request header bundle |
| `--cookies` | `"a=b; c=d"` | Raw Cookie header value |
| `--lang` | e.g. `fr-CA,fr;q=0.9` | Accept-Language header value |
| `--include` / `--exclude` | CSS selectors | Scope every page the same way |
| `--only-main-content` | flag | Main-content only |

#### `magpie map <site>` — list sitemap-derived URLs, zero LLM

```bash
magpie map https://example.com            # one URL per line
magpie map https://example.com --format json  # {site, urls, truncated}
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--format` | `lines\|json` (default lines) | Output shape; over-cap listings set `truncated: true` |

BFS to depth 5, gzip + entity aware; partial results on dead children
or a 25 s budget, flagged `truncated`.

#### `magpie summarize <url>` — summarize one URL in ≤N sentences

```bash
magpie summarize https://example.com/article
magpie summarize https://example.com/article --max-sentences 1 --provider openai
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--max-sentences` | 1–20 (default 3) | Hard cap, enforced even when the model rambles |
| `--provider` / `--model` | `…\|auto` | LLM provider + model |
| `--out` | path (default stdout) | Write output to a file |

#### `magpie diff <url> --against <file>` — word-level page diff, zero LLM

```bash
magpie scrape https://example.com --out snapshot.md
magpie diff https://example.com --against snapshot.md   # -old +new word pairs
cat snapshot.md | magpie diff https://example.com --against -
```

Identical input prints nothing and exits 0. `--against` is required;
`-` reads the snapshot from stdin.

#### `magpie watch <url>` — monitor a page for changes, zero LLM

```bash
magpie watch https://example.com/pricing --every 1h                 # loop, Ctrl-C exits 0
magpie watch https://example.com/pricing --once                     # single check (cron mode)
magpie watch https://example.com/pricing --every 5m --webhook https://hooks.local/magpie
```

Every check stores a snapshot in the cache DB; on change the word-diff
prints (`changed=true old=… new=…`) and `--webhook` receives exactly one
`POST {url, changed, old_hash, new_hash, diff}`. Silence on no-change,
exit 0 on change or not. `--once` is the cron/systemd-timer mode.

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--every` | duration, e.g. `5m`, `1h` (**required**, floor 30s) | Check interval |
| `--once` | flag | Run one check and exit (loop skipped) |
| `--webhook` | URL | POSTed the JSON change payload (operator-chosen endpoint) |
| `--render` | `auto\|static\|browser` | Fetch mode |
| `--lang` | e.g. `fr-CA,fr;q=0.9` | Accept-Language override |

#### Actions DSL — drive the browser before capture

`--action`/`--actions` (scrape) run rod interactions in order inside the
fetch session, then capture the final DOM. Actions force browser
rendering — `--render static` is rejected. One action per line; `type`,
`screenshot`, and `eval-js` take the rest of the line (spaces need no
quoting); `#` comments and blank lines are skipped; a single `wait` caps
at 30000 ms (use repeated `wait` lines). Malformed lines exit 2 naming
the verb and line number, before any I/O.

| Verb | Example | Action |
| :-- | :-- | :-- |
| `click` | `click #consent-accept` | Click the first matching element |
| `type` | `type #search unicode scraper` | Select-all + type text (replace semantics) |
| `scroll` | `scroll bottom` / `scroll 500` | To top/bottom, or by N pixels |
| `wait` | `wait 1200` | Fixed pause (≤ 30000 ms) |
| `wait-for` | `wait-for .row:nth-of-type(30)` | Auto-wait until the selector exists (bounded by the fetch budget) |
| `screenshot` | `screenshot /tmp/shot.png` | Viewport PNG mid-flow (**CLI-only** — rejected on MCP `scrape_url`) |
| `eval-js` | `eval-js window.scrollTo(0, document.body.scrollHeight)` | Run arbitrary JS, result discarded |

```bash
magpie scrape https://example.com/list --action 'click .load-more' \
  --action 'wait-for .row:nth-of-type(30)'
magpie scrape https://example.com/form --actions flow.txt --page-format screenshot --viewport 1280x800
```

Slow pages: the fetch keeps a fixed settle — no implicit network-idle
wait; add explicit `wait`/`wait-for` lines. Actions need the browser
(rod launches Chrome on first use).

#### `magpie brand <url>` — brand surface as JSON, zero LLM

```bash
magpie brand https://example.com --out brand.json
```

Returns colors, fonts, logo, and favicon. Blocked pages fail loudly,
never empty. Only flag is `--out`.

#### `magpie vertical` — zero-LLM typed extraction

```bash
magpie vertical --list                                   # all 15 extractors
magpie vertical https://arxiv.org/abs/1706.03762         # strict auto-dispatch
magpie vertical https://shop.myshop.com/products/hoodie --name shopify_product
magpie vertical https://example.com/anything --name og   # generic OG/meta, explicit only
```

| Flag | Action |
| :-- | :-- |
| `--list` | Print all registered extractors as JSON |
| `--name` | Extractor name (default `auto` = strict dispatch, no guessing) |

Built-in extractors: `arxiv`, `shopify_product`, `ecommerce_product`,
`github_repo`, `pypi`, `npm`, `crates_io`, `reddit` (permalink comment
trees via .json, depth 10 / 200 comments), `hackernews`, `youtube`,
`stackoverflow`, `trustpilot`, `dockerhub`, `huggingface`, `og`.

`og` is **OptIn**: it matches every URL, so it only fires with an
explicit `--name og` — auto-dispatch never selects it, keeping the
default scrape output shape stable.

#### `magpie crawl <url>` — BFS crawl + extract a site

```bash
magpie crawl https://example.com --schema page.yaml --out pages.jsonl
magpie crawl https://example.com/docs --schema page.yaml --path-prefix /docs \
  --include '**/docs/**' --exclude '**/api/**' --max-pages 50 --max-depth 3
magpie crawl https://example.com --schema page.yaml --exporter-cmd ./embed.sh
magpie crawl https://example.com --schema page.yaml --resume <run_id>
magpie crawl https://docs.example.com --corpus --max-pages 500 --out corpus.jsonl
magpie crawl --status <run_id>
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--schema` | yaml/json file (**required unless `--corpus`**) | Schema applied to every page |
| `--corpus` | flag | Schema-less corpus mode: one `{url,title,depth,markdown}` JSONL record per page with cleaned main-content text; zero LLM calls, no API key (jsonl only; mutually exclusive with `--schema`) |
| `--format` | `jsonl\|json\|csv\|sqlite` (default jsonl) | Output shape |
| `--out` | path (default stdout) | Write records to a file |
| `--max-pages` | int (default 100) | Max pages to claim/fetch |
| `--max-depth` | int (default 3) | Max link depth from the seed |
| `--concurrency` | int (default 8) | Fetch workers |
| `--rate` | float (default 1) | Per-host requests/sec |
| `--same-host` | bool (default true) | Follow only same-host links |
| `--allow-subdomains` | flag | Also follow subdomains of the seed host |
| `--path-prefix` | e.g. `/docs` | Only follow links under this prefix |
| `--include` / `--exclude` | URL globs (`**` crosses `/`, `*` stays in one segment) | Scope filter; exclude wins |
| `--no-sitemap` | flag | Skip sitemap seed expansion |
| `--sitemap-only` | flag | Frontier = sitemap URLs only: no seed enqueue, no link-following; zero in-scope URLs exits 3; contradicts `--no-sitemap` (exit 2) |
| `--auto-throttle` | flag | Adaptive per-host pacing: delay ×2 on 429/5xx (cap 60s, `Retry-After` wins up to 5m), decay ×¾ on success toward the configured rate / crawl-delay floor (static fetch path only) |
| `--ignore-robots` | flag | Fetch despite robots.txt (prints a warning) |
| `--browser` | fingerprint (see scrape) | TLS impersonation for all fetches |
| `--lang` | e.g. `fr-CA,fr;q=0.9` | Accept-Language header value for all fetches |
| `--status` | `run_id` | Print a run's status row + `pending/inflight/done/errors` counts instead of crawling (unknown id exits 4) |
| `--provider` / `--model` | see [Providers](#providers) | LLM provider + model |
| `--exporter-cmd` | program | Pipe each record as JSONL to its stdin, in addition to `--out` |
| `--resume` | `run_id` | Resume a checkpointed run |

Binary assets (pdf/images/video/fonts/archives) never enqueue; sitemap
expansion is scope-filtered and best-effort.

#### `magpie search <query>` — SERP search, optional scrape-through

```bash
magpie search "vector databases"                          # duckduckgo, zero-key
magpie search "pricing" --provider brave --limit 5
magpie search "launch" --scrape-top 3 --out results.jsonl  # scrape first 3 hits
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--provider` | `brave\|serper\|serpapi\|searxng\|exa\|duckduckgo` (default duckduckgo) | SERP backend |
| `--limit` | int (default 10) | Max hits |
| `--scrape-top` | int (default 0) | Scrape the first N hits through the normal pipeline (page records embedded) |
| `--out` | path (default stdout) | Write hits to a file |

BYOK keys: `MAGPIE_BRAVE_API_KEY`, `MAGPIE_SERPER_API_KEY`,
`MAGPIE_SERPAPI_API_KEY`, `MAGPIE_EXA_API_KEY`; searxng needs only
`MAGPIE_SEARXNG_URL`. Missing keys exit 7 with the set-key hint.

#### `magpie serve` — serve the pipeline over MCP

```bash
magpie serve                                   # stdio (Claude Desktop)
magpie serve --transport http --addr 127.0.0.1:8089
```

| Flag | Values / default | Action |
| :-- | :-- | :-- |
| `--transport` | `stdio\|http` (default stdio) | MCP transport (HTTP = stateless Streamable HTTP) |
| `--addr` | addr (default `:8080`) | HTTP listen address |

`crawl_site` runs synchronously to completion (no background jobs):
keep MaxPages bounded or use HTTP mode with generous client timeouts.
See [MCP server](#mcp-server) for the 12-tool list.

#### `magpie build` — compile a custom binary with extra modules

```bash
magpie build --with example.com/rodfetcher@v1.2.0 --output magpie-custom
```

| Flag | Action |
| :-- | :-- |
| `--with` | Extra `module@version` (repeatable; bare paths rejected — they'd resolve to `latest`) |
| `--output` | Output binary path (**required**) |

Remote modules need network; the build is otherwise hermetic.

#### `magpie cache` — inspect and manage learned selectors

```bash
magpie cache inspect --domain example.com
magpie cache clear --domain example.com
magpie cache heal --domain example.com --schema page.yaml --seed-url https://example.com
```

| Subcommand | Flags | Action |
| :-- | :-- | :-- |
| `inspect` | `--domain`, `--schema-hash` | Show cached selectors + null rates |
| `clear` | `--domain` (default all), `--schema-hash` | Evict cached selectors |
| `heal` | `--domain` + `--schema` + `--seed-url` (all required), `--provider`, `--model` | Force re-synthesis for a domain |

#### `magpie config` — manage configuration and API keys

```bash
magpie config set-key anthropic   # prompts, stores in OS keyring
magpie config show                 # resolved config, keys redacted
```

| Subcommand | Action |
| :-- | :-- |
| `set-key <provider>` | Prompt and store the provider key in the OS keyring |
| `show` | Print resolved config (keys redacted) |

### Global flags

Available on **every** command:

| Flag | Action |
| :-- | :-- |
| `--api-key` | Provider API key (overrides env/keyring) |
| `--max-cost` | USD cost ceiling — abort before exceeding (flat-rate `codex`, `opencode-go` exempt) |
| `--proxy-file` | Proxy pool file (sugar over `MAGPIE_PROXY_FILE`; overrides `MAGPIE_PROXY`) |
| `--config` | Config file path |
| `--cache-db` | SQLite cache DB path |

Shell completion: `magpie completion bash|zsh|fish|powershell`.

### Environment variables

| Variable | Action |
| :-- | :-- |
| `MAGPIE_<PROVIDER>_API_KEY` | Key per provider, e.g. `MAGPIE_OPENAI_API_KEY`, `MAGPIE_ANTHROPIC_API_KEY`, `MAGPIE_OPENROUTER_API_KEY`, `MAGPIE_BRAVE_API_KEY`, `MAGPIE_SERPER_API_KEY`, `MAGPIE_SERPAPI_API_KEY`, `MAGPIE_EXA_API_KEY`, `MAGPIE_OPENCODE_GO_API_KEY`, `MAGPIE_OPENCODE_ZEN_API_KEY` (dashes → underscores) |
| `MAGPIE_EXTRACT_PROVIDER` / `MAGPIE_PROVIDER` | Default LLM provider |
| `MAGPIE_MODEL` | Default model |
| `MAGPIE_MAX_COST` | Default USD cost ceiling |
| `MAGPIE_SCHEMA` / `MAGPIE_RENDER` / `MAGPIE_FORMAT` / `MAGPIE_OUT` | Defaults for the matching flags |
| `MAGPIE_CACHE_DB` | Default SQLite cache path |
| `MAGPIE_SERVE_TRANSPORT` / `MAGPIE_SERVE_ADDR` | Defaults for `serve` |
| `MAGPIE_EXPORTER_CMD` | Default for crawl `--exporter-cmd` |
| `MAGPIE_BASE_URL` | Override provider base URL (e.g. local Ollama) |
| `MAGPIE_PROXY` | Single proxy `http(s)://host:port` (becomes a 1-entry pool) |
| `MAGPIE_PROXY_FILE` | Proxy pool file (wins over `MAGPIE_PROXY`) |
| `MAGPIE_PROXY_STRATEGY` | `round-robin` (default) or `sticky-host` |
| `MAGPIE_SEARXNG_URL` | Self-hosted SearXNG instance (localhost/LAN allowed) |
| `MAGPIE_CDP_URL` | Remote browser CDP endpoint (`ws://`, `wss://`, `http(s)://`); `--cdp-url` wins when both are set |
| `MAGPIE_ALLOW_FILE=1` | Allow `file://` URLs (otherwise exit 2) |
| `MAGPIE_STRICT_SSRF` | Stricter SSRF posture when set |
| Standard `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | Honored unless the MAGPIE proxy settings override/bypass |

API keys resolve as: `--api-key` flag > `MAGPIE_<PROVIDER>_API_KEY`
env > OS keyring > config file.

### Exit codes

| Code | Meaning |
| :-- | :-- |
| 0 | ok |
| 1 | runtime error |
| 2 | usage error — incl. non-public/SSRF-rejected URLs and proxy-pool config errors |
| 3 | all-failed (batch/crawl) |
| 4 | partial success — incl. `crawl --status` with an unknown run_id |
| 5 | robots-blocked |
| 6 | cost ceiling reached |
| 7 | credentials missing (with the set-key hint) |
| 8 | quality-blocked — incl. a bot challenge that survives the warmup retry + browser escalation; the typed error names the vendor, e.g. `fetch: bot challenge (cloudflare) on … [status 403]` |

### Providers

`--provider` takes
`anthropic|openai|ollama|openrouter|codex|opencode-go|opencode-zen`.

| Provider | Auth | Billing | Notes |
| :-- | :-- | :-- | :-- |
| `anthropic`, `openai` | API key | Metered, price table | Native structured output |
| `ollama` | none (local) | Free | Base-URL switch on the OpenAI adapter |
| `openrouter` | `MAGPIE_OPENROUTER_API_KEY` | Metered, costed from `usage.cost` | Sends `provider.require_parameters` + Referer/Title so the schema is enforced, not a hint |
| `codex` | none — uses your logged-in Codex CLI | Your subscription | Shells out to `codex exec` (`--output-schema --ephemeral --ignore-user-config`); needs a current CLI, no API key, exempt from `--max-cost` |
| `opencode-go` | `MAGPIE_OPENCODE_GO_API_KEY` | Flat plan, exempt from `--max-cost` | Flat-plan traffic is monitored for abuse — extraction is tiny, but if in doubt use Zen |
| `opencode-zen` | `MAGPIE_OPENCODE_ZEN_API_KEY` | Pay-as-you-go credits (metered) | Same endpoints under `/zen/v1`; model prefix picks `/chat/completions` vs `/messages`; calls carry `x-opencode-session` + magpie UA |

Claude models are reached via API key, OpenRouter, or Zen only — reusing
a Claude Pro/Max subscription token outside Claude Code is banned by
Anthropic. Zen `/responses`-only models are unsupported (different API
shape).

### Page formats, PDF & screenshots

`--page-format html|raw|screenshot`:

- `html` emits the cleaned, scope-applied document (PDFs: their markdown
  in a minimal `<article>` wrapper).
- `raw` emits the decoded response body untouched — the quality gate
  still runs, so a challenge page is a typed error, not raw garbage.
- `screenshot` captures a full-page PNG through a real browser
  (`--viewport WxH` sets the page size, `--out f.png` writes the file,
  otherwise base64 rides in `content`).

The quality gate, `Clean`, and `Render` never change for
`raw`/`screenshot` — the gate classifies before anything is emitted.

**PDF extraction:** `magpie scrape` on an `application/pdf` URL (or
`%PDF-` magic) returns one `## Page N` markdown section per page, titled
from the URL path stem, flowing through the same quality gate as HTML —
a text-less scan PDF is a typed `empty` quality error (exit 8), an
encrypted PDF is a loud error (never empty markdown). Crawl does not
follow `.pdf` links.

### Network & security

Every fetch (static, robots, crawl) goes through one guarded transport:

- **SSRF guard (default-deny):** only public `http(s)` hosts; loopback,
  private, link-local (cloud metadata), and multicast addresses are
  rejected before dialing — including on every redirect hop and again on
  the connected peer IP (DNS-rebind safe). Rejections wrap a typed
  sentinel and exit 2. `file://` URLs are gated behind
  `MAGPIE_ALLOW_FILE=1`.
- **Proxy pool — `MAGPIE_PROXY_FILE`** (wins over
  `MAGPIE_PROXY=http(s)://host:port`, which becomes a 1-entry pool; both
  win over the standard `HTTP_PROXY`/`HTTPS_PROXY` env, honored
  otherwise). One entry per line: `http(s)://`, `socks5://`/`socks5h://`
  (Tor on loopback works), or vendor-paste `host:port:user:pass`. `#`
  comments, blank lines skipped. Strategies via
  `MAGPIE_PROXY_STRATEGY=round-robin` (default) or `sticky-host`;
  `{{session}}` inside an entry resolves to a stable 8-hex token per
  target host (rotating-gateway sticky sessions). A dead entry (dial/CONNECT
  failure — never an HTTP 4xx/5xx, which is a page outcome) is skipped for
  60 s and the fetch fails over to the next; all entries dead is a loud
  error. `run_history.proxy` records the redacted `host:port` that served
  — credentials never appear in any error, log, or record. `--proxy-file`
  is a root flag sugar over the env. Tor caveat: the privacy path, not an
  unblocking path — Tor exits are widely blocked by CDNs. NO_PROXY
  bypasses the pool exactly like the single proxy.
- **Body cap:** 50 MB on the decoded stream, so gzip bombs are truncated,
  not downloaded.
- **Run telemetry:** `run_history` rows accumulate `fetch_pages`,
  `fetch_bytes`, and `fetch_ms` next to LLM tokens/cost; databases created
  before Phase D gain the columns automatically on open.
- **Prompt-injection stripping (default-on):** hidden text — inline styles
  `display:none` / `visibility:hidden` / `font-size:0` / `opacity:0`, the
  `hidden` attribute, and HTML comments — is stripped from every cleaned
  page before any consumer (LLM extraction, markdown, MCP) sees it. Invisible
  text is the classic LLM-agent injection vector; a page cannot hijack your
  agent through markup you never saw. `aria-hidden` and computed styles are
  deliberately out (precision); the only escape hatch is `--page-format raw`.
- **Remote browser (`--cdp-url` / `MAGPIE_CDP_URL`):** point scrape (browser
  fetches and screenshots) at a running Chrome/Chromium via CDP instead of
  launching one — farms and containers never pay the local download. Scheme
  must be `ws`/`wss`/`http(s)` (validated pre-I/O); endpoint credentials
  never appear in errors.

### TLS impersonation

**`--browser chrome|firefox|safari|edge|ios|chrome_android|random`**
(scrape, batch, crawl, MCP) swaps the stock TLS stack for a byte-exact
browser fingerprint: browser TLS ClientHello (JA3/JA4) *plus* matching
HTTP/2 framing (SETTINGS, WINDOW_UPDATE, pseudo-header order) and header
set. Sites that block the stock Go handshake pass. Notes:

- `random` picks uniformly among all six profiles once per process.
- Profiles govern the static fetch + header escalation only; browser
  rendering (`--render browser`) always launches real Chrome — a browser
  render is not a fingerprint profile.
- Headers come from the fingerprint profile, not `--header-profile` —
  a stale UA next to a fresh hello is itself a fingerprint tell. Set
  `--header-profile` explicitly to override; cleartext `http://` targets
  always use our header profiles (the impersonation transport only speaks
  TLS).
- Every security property holds: pre-dial SSRF check, redirect
  re-validation, peer-IP check, proxy policy (`MAGPIE_PROXY` tunnels
  browser connections via CONNECT; `NO_PROXY` bypasses), 50 MB cap.

### Search

`brave` (`MAGPIE_BRAVE_API_KEY`), `serper` (`MAGPIE_SERPER_API_KEY`),
`serpapi` (`MAGPIE_SERPAPI_API_KEY`), `exa` (`MAGPIE_EXA_API_KEY`) are
BYOK; `searxng` needs only `MAGPIE_SEARXNG_URL` (your self-hosted
instance, JSON format); `duckduckgo` (the default) needs nothing. Search
rides the same guarded transport — SSRF guard and proxy pool included,
with localhost/LAN endpoints allowed for the provider itself (it is
operator-configured; `MAGPIE_SEARXNG_URL` on 127.0.0.1 is the canonical
setup). SERP hit URLs always scrape through the strict pipeline. Missing
keys exit 7 with the set-key hint.

### Bot challenges

Challenge-classified responses warm the cookie jar (homepage GET) and
retry once; a vendor-verified challenge that survives fails as a typed
`ChallengeError` naming `cloudflare`, `turnstile`, `datadome`, `awswaf`,
or `hcaptcha` (exit 8 under `--render auto` after one browser-escalation
attempt). Rich articles that merely mention "Just a moment" still clean
normally — detection is size-gated.

## MCP server

`magpie serve` exposes the pipeline to agents over the Model Context
Protocol: stdio for local clients, stateless Streamable HTTP for remote
ones. `crawl_site` runs synchronously to completion (no background jobs),
returns a `run_id`, and re-invoking it with that `run_id` reports stored
status without touching the extractor.

Claude Desktop config (`{ "mcpServers": { "magpie": {
"command": "magpie", "args": ["serve"] } } }`):

| Tool | Action |
| :-- | :-- |
| `scrape_url` | Fetch → clean → extract one URL (schema optional; `actions` DSL lines and `lang` accepted — the `screenshot` action verb is CLI-only) |
| `crawl_site` | Crawl a site, or poll a previous run via `run_id` (with progress) |
| `extract_structured` | Extract from HTML/markdown, no fetch (schema) or plain text (prompt) |
| `get_cached_selectors` | List cached selectors for a domain |
| `batch` | Scrape ≤100 URLs with bounded concurrency (markdown only, zero LLM) |
| `map` | List sitemap-derived URLs for a site |
| `summarize` | Summarize one URL in ≤N sentences |
| `diff` | Word-level diff of a URL vs a previous snapshot |
| `brand` | Brand colors, fonts, logo, favicon (zero LLM) |
| `list_extractors` | List zero-LLM vertical extractors |
| `vertical_scrape` | Extract one URL with a named vertical (zero LLM) |
| `search` | Search the web via BYOK/no-key SERP providers, optionally scrape the top hits |

HTTP mode: `magpie serve --transport http --addr 127.0.0.1:8089`.
Details: `magpie serve --help`.

## Custom builds & WASM plugins

Compile-time modules register via `core.RegisterModule` in `init()` and
ship inside custom static binaries:

```bash
magpie build --with example.com/rodfetcher@v1.2.0 --output magpie-custom
```

`--with` must be `module@version` (bare paths are rejected — they would
silently resolve to `latest`). Remote modules need network; the build is
otherwise hermetic.

Untrusted `.wasm` transforms run in a wazero sandbox: exactly four host
functions (`magpie_log`, `magpie_get_input`, `magpie_set_output`,
`magpie_config_get`), WASI with zero preopened dirs (no filesystem,
sockets, or env), version-gated against `core.CoreAPIVersion`. Guests
export `magpie_api_version` and `run`. Full ABI contract:
`plugin/wasm/host.go`. TinyGo and Rust guests work (both import
`wasi_snapshot_preview1`, which is linked but capability-free).

## Development

```bash
go test ./...                                  # hermetic: no network, browser, or keys
go test -tags browser ./fetch/ -run 'TestRodSmoke|TestUTLS'  # needs Chrome + network; skips otherwise
go test ./clean/ -update                       # regenerate markdown goldens
golangci-lint run ./... && go vet ./... && test -z "$(gofmt -l .)"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...  # + linux/amd64, darwin/arm64
```

Conventions: `AGENTS.md`. Source of truth: `spec.md`.

## Project layout

`cmd/magpie/` CLI · `fetch/` static + detect + rod (rod lives only here)
· `clean/` trafilatura→markdown · `extract/` schema + providers + repair
loop + coerce + cost · `config/` file/env/flag/keyring · `store/` SQLite ·
`testdata/` goldens · `plan/` phase plans.

## Contributing

No formal contributing guide yet — issues and pull requests are welcome.
Please keep the default suite hermetic (no network, browser, or keys;
gate browser tests with `//go:build browser`), keep CGO out, and run
`go test ./...`, `go vet ./...`, `golangci-lint run ./...`, and
`gofmt -l .` (must print nothing) before opening a PR.

## Credits

Thanks to the following open-source projects:

- [`North-web-dev/impersonate-http`](https://github.com/North-web-dev/impersonate-http)
  (MIT, wrapping `refraction-networking/utls`) — browser TLS
  fingerprinting, verified against tls.peet.ws; imported only inside
  `fetch/`
- [`ledongthuc/pdf`](https://github.com/ledongthuc/pdf) (BSD-3,
  stdlib-only) — PDF text extraction; imported only inside `clean/`

See `go.mod` / `go.sum` for the full dependency list and their licenses.

## License

[MIT](LICENSE) — © 2026 Dominique Degottex. Contributions are welcome
under the terms in [CONTRIBUTING.md](CONTRIBUTING.md) (MIT + a relicensing
grant so the project can evolve its license as it grows).

## Last Updated

This README was last updated on 2026-09-18.
