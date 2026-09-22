# Competitive analysis update: magpie vs webclaw vs Firecrawl vs Scrapling

Date: 2026-09-20. Supersedes the *gap lists* in `competitive-analysis-2026-09-18.md`
(that doc's proxy/proxy-generator reasoning and P2 "not chasing" lists still stand).
Sources: our repo at HEAD (post-Phase-J, mid-Phase-K), github API + READMEs +
source trees of `0xMassi/webclaw`, `firecrawl/firecrawl`, `D4Vinci/Scrapling`
(Scrapling docs read in full: overview, adaptive parsing, MCP server).

## TL;DR

- **The 09-18 P0 gap list is fully closed** and most of P1 landed in Phase J.
  magpie is now feature-at-par with webclaw OSS command-for-command, ahead on
  distribution, governance, and safety; behind on vertical breadth and ecosystem.
- **webclaw is a direct feature twin** (2,354★, AGPL, Rust, hosted webclaw.io).
  Its remaining edges: ~2× our vertical count, a REST server + Firecrawl-compat
  API + SDKs, and a published offline benchmark harness. All buildable; the REST
  server we deliberately defer to the `magpie serve` daemon phase (same Service layer).
- **Scrapling is new to the matrix** (82,452★, BSD-3, Python). Different audience
  (Python framework), but philosophically our closest competitor on the *differentiator*:
  its adaptive parsing is a deterministic cousin of our selector cache + self-heal —
  and its relocation trick is worth stealing (see §5): score-and-relocate before
  paying for an LLM re-synthesis.
- **Firecrawl stays in its lane**: hosted scale (182k★, 9 SDKs, live-view interact,
  deep-research agent). Not our competition for the local-first desktop user; its
  moat is infrastructure we have explicitly chosen not to build.
- **The desktop-GUI wedge is still open**: none of the four ships a GUI. Phase K's
  Wails app + offline-license BYOK product remains uncontested territory.

## 1. Competitors at a glance (2026-09-20)

| | magpie | webclaw | Firecrawl | Scrapling |
| :-- | :-- | :-- | :-- | :-- |
| Stars / forks | private→public v0.1.0 pending | 2,354 / 232 | 182,320 / 9,855 | 82,452 / 8,403 |
| Language | Go 1.26, CGO-free | Rust (BoringSSL) | TypeScript monorepo + Rust native ext | Python (lxml, curl_cffi, Playwright, CamouAuto) |
| License | MIT | AGPL-3.0 | AGPL-3.0 | BSD-3-Clause |
| Model | local-first single binary; BYOK; GUI (Phase K) + MoR licensing planned | OSS CLI/MCP/server + hosted webclaw.io | hosted API first; k8s self-host possible | free OSS; monetizes via proxy-vendor sponsorships |
| First release | 2026-09 | 2026-03 | 2024-04 | 2024-10 |
| Audience | humans + agents, desktop | agents + RAG pipelines | agent platforms / API consumers | Python scrapers, from script to spider farm |

## 2. Gap closure since 2026-09-18 (verified in source, not memory)

| 09-18 gap item | Status | Where |
| :-- | :-- | :-- |
| P0-1 proxy pool + rotation | ✅ DONE | `fetch/proxy.go`: round-robin + sticky-host (FNV-1a), 60s-cooldown failover, `{{session}}` sticky-gateway templating, credential redaction |
| P0-2 typed bot-challenge detection | ✅ DONE | `fetch/challenge.go` (Phase G): typed vendors — cloudflare/turnstile, awswaf, hcaptcha |
| P0-3 TLS/browser profiles | ✅ DONE | `--browser chrome\|firefox\|safari\|edge\|ios\|chrome_android\|random` + `--header-profile` |
| P0-4 output formats | ✅ DONE | `--page-format markdown\|llm\|text\|json\|html\|raw\|screenshot` |
| P0-5 search command | ✅ DONE | `scrape/search.go`: 6 providers — brave, serper, serpapi, exa, searxng (zero-key), **duckduckgo (default, zero-key)** |
| P1-6 actions DSL | ✅ DONE | `--action`/`--actions` + `screenshot` page-format + `--capture-xhr` |
| P1-7 more verticals | 🟡 partial | 15 registered (arxiv, shopify_product, ecommerce_product, github_repo, og, pypi, npm, crates_io, dockerhub, huggingface, reddit, hackernews, stackoverflow, trustpilot, youtube) vs webclaw ~30 — still missing amazon/ebay/etsy/woocommerce/instagram/linkedin/substack/dev_to |
| P1-8 watch | ✅ DONE | `cli/watch.go` + `scrape/watch.go`: `--every` (30s floor), snapshots, webhook on change |
| P1-9 crawl resume | ✅ DONE | `crawl --resume` (SQLite frontier) |
| P1-10 locale | 🟡 partial | `--lang` done; no `--country` geo param (only mattered for hosted-style geo routing — keep deferred) |
| Bonus (Phase J, unplanned) | ✅ | J.1 hidden-text/prompt-injection strip in `clean.Clean`; J.3 `--auto-throttle` adaptive backoff; J.4 `--sitemap-only`; J.5 `--cdp-url` remote browser |

Net: of 10 ranked items, 8 done, 2 partial-with-reason. The 09-18 doc can be
read as "closed"; this file becomes the live source of truth.

**Doc bug found during this audit:** README's MCP tool table lists 11 tools;
`mcp/server.go` registers 12 (`search` is missing from the table). One-line
README fix owed.

## 3. Scrapling — what it actually is

Python framework, three layers:

1. **Parser** (`Selector`, lxml-based): CSS/XPath/text/regex selection, element
   navigation, and **adaptive matching** — `page.css(sel, auto_save=True)` stores
   element fingerprints (tag, text, attrs, sibling tags, path tags, parent attrs)
   in SQLite keyed by `(domain, identifier)`; when the selector later returns
   nothing, `adaptive=True` scores every element on the page for similarity and
   returns the best match. Explicitly **"without AI"** — deterministic scoring.
   This is the philosophical sibling of our `selector/` cache + `heal.go`; theirs
   relocates user-written selectors, ours synthesizes and re-synthesizes via LLM.
2. **Fetchers**: `Fetcher` (curl_cffi TLS impersonation + HTTP/3), `DynamicFetcher`
   (Playwright Chromium / real Chrome / CDP), `StealthyFetcher` (CamouAuto patched
   Firefox: CF Turnstile/Interstitial auto-solve, canvas noise, WebRTC leak block,
   DoH). Deeper stealth than anything in our tree — but browser-bound.
3. **Spiders**: concurrent multi-session crawls, pause/resume, proxy rotation with
   **blocking-aware backoff** (we got the same class of behavior in Phase J.3
   `--auto-throttle`, at the fetch level), adaptive per-host speed.

Plus: interactive CLI shell (`scrapling` REPL), extract commands, MCP server with
**13 tools** (one-shot + persistent-session tools, `screenshot` returning a real
ImageContent block, mandatory auth-token on Streamable HTTP, localhost-by-default —
they went through the same HTTP-server hardening we did), prompt-injection
sanitization of hidden content (same threat model as our J.1 strip — independent
convergence, good validation), and a packaged agent skill.

**Where they beat us:** stealth depth (browser-tier), Python ecosystem reach,
community (82k), the interactive shell for humans, and marketing (12-language README).
**Where we beat them:** single binary vs `pip install` + browser downloads; typed
LLM extraction with cost caps (they have none); cleaning pipeline depth
(trafilatura + quality gate + injection strip); crawl governance (robots, bloom,
resume); WASM plugin surface; license (BSD is fine, but our MIT + CGO-free static
binary is the friendlier embed); verticals are *selectors* there, *products* here.

## 4. Updated matrix — only load-bearing deltas

Rows unchanged since 09-18 are omitted; see that doc. New/changed rows:

| Feature | magpie | webclaw | Firecrawl | Scrapling |
| :-- | :--: | :--: | :--: | :--: |
| Zero-LLM vertical extractors | 15 | ~30 | — (schema-based) | — (selector-based) |
| Web search → scrape | ✅ 6 providers, DDG zero-key | ✅ SerpApi (hosted) | ✅✅ first-class | ❌ (agent can improvise) |
| Watch/monitor | ✅ local `watch --every` + webhook | ✅ hosted watches | changeTracking API | ❌ (spider loops) |
| Screenshot | ✅ rod viewport | ❌ | ✅ | ✅✅ (ImageContent, JPEG/full-page) |
| Browser actions | ✅ DSL + capture-xhr + CDP remote | ❌ | ✅✅ + AI-prompt interact (live view) | 🟡 wait/wait_selector/session tools |
| Adaptive element relocation | ❌ (LLM heal only) | ❌ | ❌ | ✅✅ deterministic, SQLite |
| Interactive human shell | ❌ (GUI coming, Phase K) | ❌ | ❌ | ✅ |
| REST API server | ❌ (Service layer ready; daemon planned) | ✅ + Firecrawl-compat | ✅ | ❌ (MCP + CLI only) |
| MCP tools | 12 (local, all pipeline verbs) | 12 | npx wrapper → hosted | 13 (fetch/session-centric) |
| Prompt-injection strip | ✅ (J.1, clean choke point) | ❌ | ❌ | ✅ (MCP responses) |
| Published benchmarks | ❌ golden tests only | ✅ harness + fixtures + results/ | ✅ marketing benchmarks | ✅ benchmarks.py |
| Agent-skill packaging | ❌ | ✅ npx skill | ✅ firecrawl-cli init | ✅ SKILL.md zip |
| Installs config for you | ❌ copy-paste JSON | ✅ `npx create-webclaw` | ✅ `init --all` | ✅ docs per client |

## 5. What to steal next (re-ranked backlog)

1. **Deterministic relocation pass in `selector/heal.go` (from Scrapling).**
   Today: null-rate trips → LLM re-synthesis (cost, latency, needs key).
   Add a zero-LLM middle step: fingerprint the cached selector's element
   (tag/text/attrs/parent), score candidate nodes on the fresh page, accept above
   a threshold, else fall back to LLM synthesis. Fits the zero-LLM-verticals
   positioning, cuts heal cost to zero in the common redesign case. Pure DOM work,
   no new dep. *This is the highest-value cross-pollination in this doc.*
2. **Agent-skill + installer packaging (all three competitors ship it).**
   A `skill/SKILL.md` + README config snippets is docs work, not code; an
   `magpie init --client claude-code` that writes the `serve` MCP stanza is ~50
   lines in `cli/`. Cheap, high agent-adoption leverage, matches the MCP-first bet.
3. **Verticals to 20+, from webclaw's proven list** (unchanged P1-7): amazon,
   ebay, etsy, woocommerce, substack, dev_to. Each = JSON-LD/DOM against fixtures,
   one file + tests, following the existing `registries.go` pattern.
4. **Offline benchmark harness (webclaw-style).** Fixtures already exist in
   `testdata/`; add word-count/precision assertions + a `benchmarks/` doc. Credibility
   marketing the others get for free; also pins clean-quality regressions harder
   than goldens alone.
5. **Keep deferred (explicit):** REST API server (Service layer is the plan —
   `magpie serve` daemon phase), live-view AI interact, stealth browser tier
   (rod + profiles + challenge-warmup covers our local-first scope; CamouAuto-class
   stealth is a maintenance swamp we shouldn't enter), geo `--country` (hosted-only value).

## 6. Positioning summary (unchanged in direction, sharper in evidence)

- webclaw proved the *local-first OSS extractor* category is real and viral
  (2.3k★ in 6 months on the same feature list we built). That validates demand;
  our wins over it are structural (single static binary, governance, plugins,
  healing selectors, MIT) rather than feature-checklist items.
- Scrapling proved *self-healing selectors* is a marketable headline (their
  adaptive feature leads the README) — our `selector/` cache + heal is the same
  bet with an LLM twist. Item 5.1 makes it strictly stronger and keeps the
  "zero-LLM by default" story coherent.
- Firecrawl remains the scale/utility ceiling, not a local tool. Don't chase.
- The uncontested product is still the one in Phase K: a desktop app where a
  non-developer points at a site, gets typed data, pays once for the license,
  brings their own keys and proxies. None of the three is building that.
