# Technical Specification: `magpie` — A Go CLI Web Scraper with LLM Selector Synthesis

## TL;DR
- **Build it as a minimal Cobra core + plugin registry**, using **go-rod for JS rendering** (it does not leave zombie Chrome processes on Windows/Mac where chromedp does, and its decode-on-demand event bus avoids chromedp's fixed-buffer deadlocks), **go-trafilatura v2 + html-to-markdown/v2 for cleaning**, a **provider-agnostic LLM extractor** with per-provider native structured-output, and a **domain+schema-hashed SQLite selector cache with null-rate self-healing** as the core differentiator.
- **Every ecosystem-moving dependency is pinned and verified as of Sept 2026**: official MCP Go SDK (latest stable **v1.5.0**; spec 2026-07-28 support landed in v1.7.0), wazero v1.12.x (chosen over Extism for zero-dep sandboxing and full control of the host-function surface), modernc.org/sqlite (pure-Go, no CGO, Windows-supported), cenkalti/backoff/v5, santhosh-tekuri/jsonschema/v6.
- **Ship in 3 milestones**: (1) single-URL fetch→clean→extract in week 1; (2) selector cache + concurrency + crawl in weeks 2–4; (3) MCP server + plugin system last.

## Key Findings & Headline Decisions

| Concern | Decision | One-line justification |
|---|---|---|
| JS render engine | **go-rod** (github.com/go-rod/rod) | Decode-on-demand + goob event bus avoids chromedp's fixed-buffer deadlocks, and it does not leave zombie Chrome processes on Windows/Mac (chromedp does). |
| Static fetch | **net/http** | Stdlib, zero deps, HTTP/2 built in. |
| Boilerplate removal | **markusmobius/go-trafilatura v2** | On the scrapinghub article-extraction benchmark it scores F1 0.960 / precision 0.940 vs go-readability's F1 0.934 / precision 0.900. |
| HTML→Markdown | **JohannesKaufmann/html-to-markdown/v2** (v2.5.2) | Actively maintained, plugin architecture, working table plugin since v2.3.0. |
| Schema validation | **santhosh-tekuri/jsonschema/v6** | Full draft 2020-12, mature, zero-dep, rich error paths. |
| WASM sandbox | **wazero** (tetratelabs) | Zero-dep, no CGO, pure-Go cross-compile; full control of host-function surface vs Extism's opinionated ABI. |
| MCP SDK | **modelcontextprotocol/go-sdk** | Official, stable since v1.0.0 (latest v1.5.0), spec-complete, supports stdio + Streamable HTTP. |
| SQLite | **modernc.org/sqlite** | Pure-Go, no CGO, supports windows/amd64 + windows/386, cross-compiles cleanly. |
| Retry/backoff | **cenkalti/backoff/v5** | Generics-based `Retry[T]`, context-aware, Permanent-error support. |
| robots.txt | **jimsmart/grobotstxt** | Faithful port of Google's C++ parser/matcher, stdlib-only runtime deps. |
| Concurrency | **errgroup + x/time/rate + bounded worker channels** | errgroup gives context-cancel error propagation; per-host token buckets for politeness. |
| Dedup at 100k+ | **SQLite visited table + in-memory bloom filter front** | Bloom filter absorbs the hot path; SQLite is the durable source of truth for resume. |
| API-key storage | **zalando/go-keyring** (v0.2.8) | One API across Windows Credential Manager / macOS Keychain / Linux Secret Service; delegates to wincred on Windows. |

---

## 1. FETCH

### 1.1 Engine choice: go-rod over chromedp

Both are Chrome DevTools Protocol drivers with no external driver dependency and are actively maintained as of Sept 2026 (chromedp v0.16.0 shipped Aug 2026; go-rod v0.116.2 with an active community fork `rah-0/rod` v0.121.0 from Sept 2026). The decisive factors for a **Windows-primary solo dev**:

1. **Zombie process cleanup.** go-rod's own comparison doc states that when a crash happens, "Chromedp will leave the zombie browser process on Windows and Mac." For an unattended crawler on Windows 11 this is disqualifying for chromedp as the default.
2. **Concurrency stability.** chromedp uses a fixed-size event buffer and single event loop; slow handlers block each other and can deadlock under high concurrency. go-rod is built on the `goob` async bus and decodes CDP messages on demand, so it performs better under heavy network events.
3. **Pinned browser.** go-rod ships/downloads a specific Chromium + protocol version per release with full unit tests, avoiding chromedp's "system browser silently upgraded" breakage class.

chromedp's advantages (simpler binary distribution, larger star count ~11.5k, more idiomatic `context`-first API) do not outweigh the Windows zombie-process and deadlock risks for this workload. **Decision: go-rod.** Note the maintenance caveat: go-rod's upstream maintainer activity has slowed (LFX Insights flags ~1 active maintainer, ~28-day median issue response); pin to a known-good version and vendor it. The `rah-0/rod` fork (v0.121.0, dependency-free core, Go 1.27.1) is a fallback if upstream stalls.

Wrap the browser behind the `Fetcher` plugin interface (§6) so the engine can be swapped without touching the pipeline.

### 1.2 Auto-detect heuristic: "DOM is empty / JS-required"

Run the static `net/http` fetch first. Escalate to go-rod **only if** the static response trips the detector. Compute a score; escalate when score ≥ 2:

| Signal | Threshold | Weight |
|---|---|---|
| Visible-text length after boilerplate strip | < 200 chars | +2 |
| Script-bytes to text-bytes ratio | > 3.0 | +1 |
| `<body>` contains a known SPA mount node and little else | see fingerprints | +2 |
| `<noscript>` contains "enable JavaScript" / "requires JavaScript" | present | +1 |
| Number of `<a href>` with non-anchor targets | < 3 | +1 |
| Content-Length of raw HTML | < 5 KB but ≥ 10 `<script src>` | +1 |
| `<meta name="fragment" content="!">` (legacy AJAX-crawl marker) | present | +1 |

**SPA framework fingerprints** (presence of any is a strong +2 signal):
- React: `<div id="root">` or `<div id="app">` empty, `data-reactroot`, `window.__REACT_DEVTOOLS_GLOBAL_HOOK__`
- Vue: `<div id="app">` with `data-v-` attributes, `window.__VUE__`
- Angular: `<app-root>`, `ng-version` attribute
- Next.js: `<div id="__next">`, `<script id="__NEXT_DATA__">`
- Nuxt: `<div id="__nuxt">`, `window.__NUXT__`, `<script>window.__NUXT__=`
- Svelte/SvelteKit: `<div id="svelte">`, `__sveltekit_` hydration scripts

Crucially, if `<script id="__NEXT_DATA__">` or `window.__NUXT__` is present, the data is often **already in the HTML as JSON** — extract it directly and skip browser rendering entirely. This is a major cost saver for the Amazon.fr repricing use case where Next/Nuxt-style embedded JSON is common.

### 1.3 HTTP client config

```go
var DefaultHTTPClient = &http.Client{
    Timeout: 30 * time.Second, // whole-request ceiling
    Transport: &http.Transport{
        Proxy:                 http.ProxyFromEnvironment,
        ForceAttemptHTTP2:     true,
        MaxIdleConns:          100,
        MaxIdleConnsPerHost:   10,
        IdleConnTimeout:       90 * time.Second,
        TLSHandshakeTimeout:   10 * time.Second,
        ExpectContinueTimeout: 1 * time.Second,
        DisableCompression:    false, // gzip/deflate/br auto
    },
    CheckRedirect: func(req *http.Request, via []*http.Request) error {
        if len(via) >= 10 {
            return errors.New("stopped after 10 redirects")
        }
        return nil
    },
}
```

- **Timeouts:** 30s whole-request via `client.Timeout`; per-attempt `context.WithTimeout` in the worker (default 20s) so retries get fresh budgets; 10s TLS handshake.
- **Redirect policy:** follow up to 10, then error (matches Go default but explicit).
- **Cookie jar:** `net/http/cookiejar` with `publicsuffix.List` per-run (in-memory), reset between domains for the logged-out Amazon.fr constraint.
- **HTTP/2:** `ForceAttemptHTTP2: true` (default with TLS anyway).
- **Compression:** leave `DisableCompression: false` so the transport advertises gzip and auto-decodes.
- **Realistic headers:** ship a small rotating set of current desktop browser header bundles (User-Agent, Accept, Accept-Language `fr-FR,fr;q=0.9,en;q=0.8` for the Amazon.fr consumer, Accept-Encoding, Sec-CH-UA client hints, Sec-Fetch-* set). Default UA identifies the bot (see §11) unless the user opts into a browser UA.
- **Run-supplied headers (M0):** `fetch.FetchRequest.Headers []string` / `scrape.Options.Headers []string` — raw `"Name: value"` lines applied on the static path after the profile bundle, Lang, and cookies, so the caller wins same-name conflicts. Shape and control characters are rejected pre-I/O at the options boundary (`ValidateOptions` → `OptionsError`, exit 2 — CRLF in a header line is header injection); `Authorization: Bearer …` passes. CLI `--header` (repeatable), MCP `headers []string`. Static path only — the rod path does not inject run headers.
- **Browser cookie injection (M0):** `--cookies` is no longer static-only — before `page.Navigate`, the rod path parses `FetchRequest.Cookies` (`"a=b; c=d"`) into CDP `NetworkSetCookie` params scoped to the URL's host with `Path: /`, so the document request itself carries them (a JS-rendered login wall no longer sees a logged-out page). Injection happens per navigation in `openPage` — the cookie persists in that browser session's jar (fresh browser per scrape run, so runs stay isolated); host scoping is the browser jar's.

---

## 2. CLEAN

### 2.1 Stack: go-trafilatura v2 → html-to-markdown/v2

Benchmark evidence: on the independent scrapinghub article-extraction benchmark, **go-trafilatura scores F1 0.960 / precision 0.940 / recall 0.980** vs **go-readability F1 0.934 / precision 0.900 / recall 0.982** — go-trafilatura wins on precision (the metric that matters for boilerplate removal). On the maintainer's own 960-document corpus the Go port matches Python Trafilatura v2.2.0 (Precision 0.9171 / Recall 0.9113 / F1 0.9142). go-trafilatura is purpose-built for boilerplate removal and content retention, tracks Python Trafilatura v2.2.0, and is actively verified (Sept 11, 2026 CI on Go 1.26/1.27). With `EnableFallback: true` it falls back to readability/dom-distiller on hard pages (the fallback is opt-in — it does not run by default).

**Why not go-readability alone:** the original `go-shiori/go-readability` is now **deprecated** in favor of `codeberg.org/readeck/go-readability/v2`. Depending on a deprecated root module is a maintenance risk; go-trafilatura vendors the maintained readability path internally. (If you ever choose readability directly, use a maintained fork — the scrapinghub benchmark lists a `go_readability_fork` at F1 0.947, higher than the original's 0.934.)

Pipeline:
1. **go-trafilatura** extracts the main content node (drops nav/footer/ads/cookie banners), preserving structure. Cost: ~4.25s worst case vs go-readability's 2.87s — acceptable since it runs once per page and the LLM call dominates latency.
2. **html-to-markdown/v2 (v2.5.2, released Jun 7 2026; table plugin since v2.3.0)** converts the retained node to CommonMark + GFM tables.

**Decision: go-trafilatura v2 for extraction, html-to-markdown/v2 for serialization.**

### 2.2 Cleaning contract

**Always stripped:** `<script>` (except JSON-LD/`__NEXT_DATA__` which are harvested first), `<style>`, `<svg>`, inline event handlers, tracking pixels, cookie/consent banners, nav, footer, sidebars, ad slots, comment sections, `<iframe>`.

**Always preserved:**
- **Tables** → GFM markdown tables (colspan/rowspan handled by html-to-markdown's table plugin, `table.WithSpanCellBehavior(table.SpanBehaviorMirror)`: the spanning cell's text is repeated into each covered cell so field↔value adjacency survives).
- **Image alt text** → `![alt](src)`; `src` kept for provenance.
- **Structured data**: all `<script type="application/ld+json">` JSON-LD blocks extracted verbatim into a `structured_data` sidecar field (this is gold for price/product facts).
- **Microdata / RDFa**: `itemprop`/`itemtype` attributes parsed into key-value pairs in the sidecar.
- **Headings, lists, links, code, blockquotes** preserved as markdown.

**Target token budget per page:** default cap **8,000 tokens** of cleaned markdown fed to the LLM. Markdown is materially smaller than raw HTML — web2md and AlterLab both measured ~67–68% reduction (~3×), while one independent 10-page test found a 7.4× median; treat it as a 2–3× central estimate. Pages exceeding the cap are handled per §3.4. The JSON-LD sidecar is always sent in full even if it means trimming prose, because for repricing the structured block usually contains the price/EAN directly.

---

## 3. EXTRACT (provider-agnostic LLM layer)

### 3.1 Provider abstraction

```go
type Extractor interface {
    Extract(ctx context.Context, in ExtractInput) (ExtractResult, error)
    Name() string
}

type ExtractInput struct {
    Markdown       string
    StructuredData json.RawMessage // JSON-LD sidecar
    Schema         []byte          // compiled user schema
    Hints          FieldHints
}
type ExtractResult struct {
    Data     json.RawMessage
    Usage    TokenUsage // prompt, completion, cost estimate
    Provider string
    Model    string
}
```

### 3.2 Structured-output strategy per provider

As of 2026 all three providers offer grammar-constrained (sampling-level) JSON enforcement, but via different mechanisms:

| Provider | Mechanism | Notes |
|---|---|---|
| **OpenAI** | Native **Structured Outputs** (`response_format` with `strict:true`, `additionalProperties:false`, all fields required) | Cleanest; <0.1% malformed rate. First call per schema has 200–400ms grammar-compile penalty, then cached. |
| **Anthropic** | **JSON outputs** — `output_config: {"format": {"type": "json_schema", "schema": ...}}` on `/v1/messages` (GA as of Sept 2026, no beta header). Do **not** force a tool call: `tool_choice` `tool`/`any` returns 400 on Claude Fable 5.1. | Requires every object have `additionalProperties:false` and full `required`; no numeric/length constraints. Claude Opus 5 / Sonnet 5 / Haiku 4.5. |
| **Ollama (local)** | `format` = JSON schema (llama.cpp GBNF grammar) | Weaker guarantee on small models → always run the repair loop. |

**Portable schema subset** (the intersection that works everywhere): plain objects, `required` fields, nullable-instead-of-optional (`["type","null"]`), enums, no numeric bounds you depend on, no discriminated unions (OpenAI bans them). The synthesizer emits only this subset.

**Decision:** provider adapters each translate the user schema into their native strict mode; never rely on prompt-only JSON.

### 3.3 Retry/repair loop for invalid JSON

```
attempt = 0
loop:
  resp = call_provider(schema, markdown)
  if parse_ok(resp) && validate(schema, resp): return resp
  attempt++
  if attempt > 2: return error(last_validation_errors)
  # repair: feed the exact validator error paths back
  markdown = repair_prompt(resp, validation_errors)
  goto loop
```

Max **3 attempts** total. The repair prompt includes the santhosh-tekuri v6 JSON-pointer error locations (`- at '/price': got string, want number`) so the model fixes the specific field. Because native strict mode already guarantees parseable JSON on OpenAI/Anthropic, the repair loop mainly catches Ollama and semantic (schema-valid-but-wrong-type-coercion) failures.

### 3.4 Truncation strategy for long pages

1. If cleaned markdown ≤ 8k tokens: send whole.
2. If larger: **prioritized windowing** — always include the JSON-LD sidecar + the first heading block + any block whose text matches a `css_hint`/`regex` from the schema; then fill remaining budget top-down.
3. For list/repeated-record pages (the BardeenAgent case): don't send the whole page — send **one representative record's HTML** to synthesize selectors (§4), then apply selectors locally with no further LLM calls. This is the primary token-cost defense.

### 3.5 Schema validation library

**Decision: santhosh-tekuri/jsonschema/v6.** Full draft 2020-12 (and 4/6/7/2019-09), fully compliant with JSON-Schema-Test-Suite, thread-safe, rich hierarchical errors with JSON-pointers, zero third-party deps. kaptinlin/jsonschema is a strong alternative with nice ergonomics (`Unmarshal`, struct-tag `schemagen`, default-value injection) and it actually borrows santhosh-tekuri's format validators — but santhosh-tekuri is the more battle-tested core validator and its error-path output is exactly what the repair loop needs. Use kaptinlin's `schemagen` offline as a dev convenience to scaffold schemas from Go structs, but validate at runtime with santhosh-tekuri/v6.

### 3.6 Cost tracking

Per call, record `{provider, model, prompt_tokens, completion_tokens, usd_estimate, ts}` into the `llm_calls` table (§9). Maintain a static price table per model (updatable via config). Emit a per-run cost summary and support a `--max-cost` ceiling that aborts a crawl when exceeded.

### 3.7 User-facing schema file format

**Decision: JSON Schema draft 2020-12 subset, in YAML or JSON**, with a reserved `x-magpie` extension namespace for hints (JSON Schema allows unknown `x-` keywords; validators ignore them).

```yaml
$schema: "https://json-schema.org/draft/2020-12/schema"
type: object
additionalProperties: false
required: [title, price]
properties:
  title:
    type: string
    x-magpie: { css_hint: "h1#productTitle", trim: true }
  price:
    type: number
    x-magpie:
      css_hint: "span.a-price .a-offscreen"
      regex: '[0-9]+[.,][0-9]{2}'
      coerce: "eur_decimal"   # strips €, converts ',' → '.'
  ean:
    type: [string, "null"]
    x-magpie: { jsonld_path: "$.gtin13" }
```

Custom `x-magpie` extensions: `css_hint`, `xpath_hint`, `regex` (post-extraction capture), `coerce` (type coercion: `int`, `float`, `eur_decimal`, `iso_date`, `bool`), `jsonld_path` (pull from the JSON-LD sidecar first), `trim`, `multiple` (expect array).

---

## 4. SELECTOR SYNTHESIS + CACHE (the core differentiator)

### 4.1 Prior art (cited)

- **BardeenAgent / WebLists** (Bohra et al., arXiv:2504.12682, Apr 2025). Introduces the WebLists benchmark (200 tasks / 50 sites). Key result, quoting the abstract: "BardeenAgent achieves 66% recall overall, more than doubling the performance of SOTA web agents, and reducing cost per output row by 3x." Table 3 reports 66.2% recall at 72.5% precision, vs SOTA web agents 31% and LLM+search 3%. Its "List Mode" finds the least-common-ancestor of selected elements and targets immediate children; it generates CSS selectors two ways — **heuristic** (sample parent-child/position/class/attribute strategies, filter non-matching, join viable ones) and **LLM-based** (Gemini 1.5 Pro, temp 0.4) for cases with no clean common parent. The ablation is the load-bearing evidence for our whole design: removing the selector model drops average recall from **66.2% to 35.6%** — selector synthesis is the single biggest contributor.
- **Firecrawl `/extract`**: converts page → clean markdown → LLM fills schema (or free prompt). No public selector-caching internals; it is per-call LLM extraction, which is exactly the expensive baseline we cache away from.
- **ScrapeGraphAI**: Python, graph-of-nodes (Fetch→Parse→GenerateAnswer), LLM-per-page, "no CSS/XPath." Resilient to drift but pays ~5k tokens (~$0.015 GPT-4o-mini) *per page* — our cache amortizes this to near zero after synthesis.

Our differentiator vs all three: **synthesize selectors once from N samples, validate against direct LLM extraction, cache by domain+schema, and self-heal on null-rate.** Firecrawl/ScrapeGraphAI re-pay LLM cost every page; we pay once per template generation.

### 4.2 Lifecycle

```
1. SYNTHESIZE
   - Collect N sample pages (default N=3) for a domain+schema.
   - For each: run direct LLM extraction (§3) → "ground truth" field values.
   - Ask LLM to emit CSS (preferred) or XPath selectors per field that
     reproduce those values. Also try heuristic generation (BardeenAgent-style)
     and keep whichever validates.
2. VALIDATE
   - Run synthesized selectors against all N samples with a DOM engine (goquery for CSS).
   - Require field-level agreement with the direct-LLM ground truth on
     >= (N-1)/N samples per field (default: 3/3 must agree, or 2/3 with a warning).
   - Fields that fail validation fall back to per-page LLM extraction and are
     flagged non-cacheable.
3. CACHE
   - Store validated selectors keyed by (domain, schema_hash).
   - schema_hash = SHA-256 of the canonicalized schema JSON.
4. APPLY (steady state)
   - For each new page: run cached selectors via goquery/xpath. NO LLM call.
   - Apply x-magpie regex + coerce. Emit record.
5. SELF-HEAL
   - Track per-field null rate over a sliding window (default last 50 pages).
   - If any field's null rate crosses threshold (default 0.30), mark that
     field stale and trigger LLM re-synthesis for ONLY the broken fields.
   - Healthy fields keep serving from cache during re-synthesis (partial failure).
```

### 4.3 Partial failure handling

Selectors are cached and healed **per field**, not per page. A price selector breaking after a site redesign triggers re-synthesis of `price` alone while `title`/`ean` keep serving from cache. Re-synthesis uses the most recent pages that still produced valid values for the healthy fields as samples. If ≥ 50% of fields are broken simultaneously, treat it as a full redesign and re-synthesize the whole template.

Before paying for LLM re-synthesis, the heal pass first attempts **deterministic element relocation**: each field's doc carries a structural fingerprint (tag, classes, id, whitelisted attributes, parent/grandparent signature, numeric-text flag) of the element its cached selector matches, and relocation re-finds that element on the retained new-template samples by weighted similarity. A replacement selector is accepted only when it extracts the already-retained ground truth on every new-template sample (no shape-only acceptance); when the fingerprint is missing, fewer than two usable samples exist, the best candidate is below threshold or ambiguous, or the field is Multiple/jsonld, relocation **declines** and the existing LLM re-synthesis path runs unchanged. Relocation itself makes zero LLM calls.

### 4.4 Cache storage format & location

SQLite table `selector_cache` (§9). Selectors stored as a JSON document:

```json
{
  "schema_hash": "ab12...",
  "domain": "amazon.fr",
  "fields": {
    "title": {"type":"css","expr":"h1#productTitle","coerce":"","null_rate":0.01},
    "price": {"type":"css","expr":"span.a-price .a-offscreen","regex":"[0-9]+[.,][0-9]{2}","coerce":"eur_decimal","null_rate":0.04}
  },
  "synthesized_at": "2026-09-16T10:00:00Z",
  "samples_used": 3,
  "fingerprints": {"price": {"tag": "span", "id": "a-price", "parent": "div.buybox", "num_text": true}},
  "engine_version": 2
}
```

The optional `fingerprints` map records, per css field, the structural fingerprint of the element its selector matches (§4.3 relocation); it is absent on EngineVersion-1 docs, and old caches behave exactly as before.

Location: `%LOCALAPPDATA%\magpie\cache.db` on Windows, `$XDG_CACHE_HOME/magpie/cache.db` (fallback `~/.cache/magpie`) on Linux/macOS. Override with `--cache-db`.

---

## 5. CONCURRENCY ARCHITECTURE

### 5.1 Pipeline wiring (channels + backpressure)

Three-stage pipeline connected by **bounded buffered channels**; buffer sizes chosen so slow stages exert backpressure rather than unbounded memory growth:

```
frontier(URLs) --(chan, buf=1000)--> fetchStage --(chan, buf=100)--> cleanStage --(chan, buf=100)--> extractStage --> writer
```

- Frontier→fetch buffer 1000 (cheap URLs, absorb bursts).
- fetch→clean and clean→extract buffers 100 (bounded because pages are large in memory).
- The extract stage is the bottleneck (LLM latency); its worker count is small (default 4) and the buffer in front of it fills, propagating backpressure up to the fetchers, which then stop pulling from the frontier. This is the desired self-throttling.

### 5.2 Worker-pool pattern

**Decision: `golang.org/x/sync/errgroup` with `SetLimit(n)` for bounded concurrency**, one errgroup per stage. Rationale: errgroup propagates the first error and cancels the shared context (clean shutdown for free), `SetLimit` gives bounded concurrency without a hand-rolled semaphore, and it composes with `context`. A raw `chan struct{}` semaphore is more code and no benefit; a naive unbounded `go func()` per URL is a non-starter at 100k URLs.

Defaults: fetch workers = 8 (static) but gated by per-host rate limiter; browser (go-rod) workers = 2 (memory-heavy); clean workers = `GOMAXPROCS`; extract workers = 4 (API-rate/cost bound).

### 5.3 Per-domain rate limiting

**`golang.org/x/time/rate` token bucket, keyed per host.** A `map[string]*rate.Limiter` guarded by a mutex (or `sync.Map`); each host gets its own bucket. Default: **1 request/sec per host, burst 3** (polite). Configurable per-domain overrides. The fetch worker calls `limiter.Wait(ctx)` before every request; combined with §5.1 backpressure this bounds both global and per-host load.

### 5.4 Retry with backoff + jitter

**Decision: cenkalti/backoff/v5.** Generics `Retry[T](ctx, op, opts...)`, context-aware, built-in randomization (jitter), `backoff.Permanent(err)` to stop on non-retryable errors. Config: `WithMaxTries(4)`, initial interval 500ms, multiplier 2.0, randomization factor 0.5, max interval 30s, `WithMaxElapsedTime(2m)`.

**Retryable error taxonomy:**
- Retryable: connection reset, timeout, DNS temporary failure, HTTP 408/429/500/502/503/504, TLS handshake timeout. On 429/503 honor `Retry-After` header if present.
- Permanent (wrap in `backoff.Permanent`): HTTP 400/401/403/404/410, schema validation failure after repair loop, robots.txt disallow, context canceled.

### 5.5 Graceful shutdown

- Root `context.Context` canceled on SIGINT/SIGTERM (`signal.NotifyContext`).
- Cancellation propagates through every stage's errgroup; workers finish their current item (respecting a short drain deadline, default 30s) then exit.
- In-flight crawl state is checkpointed to SQLite continuously (§9 `crawl_state`), so a killed run resumes exactly where it stopped via `magpie crawl --resume <run_id>`.
- go-rod browser contexts are closed in `defer` to avoid the zombie-process class entirely.

### 5.6 Crawl frontier design

- **Dedup:** two-tier. An in-memory **bloom filter** (`bits-and-blooms/bloom/v3`, sized for ~1M URLs at 1% FP) is the fast negative check on the hot path; on a possible hit, confirm against the SQLite `dedup` table (canonicalized-URL SHA-256 primary key). This keeps the common "already seen" check O(1) in memory while SQLite remains the durable, exact, resumable source of truth.
- **Why not map+mutex alone:** a `map[string]struct{}` of 100k+ URLs is fine for memory but is lost on restart and doesn't scale to millions without unbounded RAM. Bloom+SQLite survives restarts and bounds memory.
- **Frontier storage:** SQLite `crawl_state` table holds `{url, status(pending/inflight/done/error), depth, discovered_at}`. Workers claim pending rows in a transaction (`UPDATE ... WHERE status='pending' LIMIT n RETURNING`), giving safe multi-worker draining and free resume.
- URL canonicalization before hashing: lowercase host, strip default ports, sort query params, drop fragments and known tracking params (`utm_*`, `gclid`).

---

## 6. PLUGIN INTERFACES

All plugins implement a common `Plugin` marker with version negotiation. Interfaces are intentionally small.

```go
// Version negotiation: every plugin declares the API version it was built against.
type Plugin interface {
    PluginInfo() Info
}
type Info struct {
    Name       string
    APIVersion string // semver, e.g. "1.0.0"; core rejects incompatible majors
    Kind       Kind   // Fetcher, Cleaner, ...
}

type Fetcher interface {
    Plugin
    Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error)
    CanHandle(req FetchRequest) bool // e.g. JS-required detector routes here
}

type Cleaner interface {
    Plugin
    Clean(ctx context.Context, raw RawPage) (CleanedPage, error)
}

type Extractor interface { // see §3.1
    Plugin
    Extract(ctx context.Context, in ExtractInput) (ExtractResult, error)
}

type Exporter interface {
    Plugin
    Export(ctx context.Context, records <-chan Record) error // exec/subprocess kind
}

type Notifier interface {
    Plugin
    Notify(ctx context.Context, ev Event) error // run-complete, error, cost-exceeded
}

type ProxyProvider interface {
    Plugin
    Proxy(ctx context.Context, req FetchRequest) (*url.URL, error)
}

type RateLimitStrategy interface {
    Plugin
    Wait(ctx context.Context, host string) error
    Report(host string, resp *FetchResponse) // adaptive strategies use response codes
}
```

### 6.1 Version negotiation

The core defines `CoreAPIVersion` (semver). At registration, the core compares the plugin's `APIVersion`: **same major = accepted** (minor/patch differences tolerated, forward-compatible); **different major = rejected** with a clear error. WASM plugins declare their version via an exported `magpie_api_version` function. This is the Caddy model plus an explicit semver gate.

### 6.2 Three plugin transports (per fixed constraints)

1. **Compile-time Go modules** (trusted) — Caddy/xcaddy registration model (§6.3).
2. **WASM sandbox** (untrusted third-party) — wazero (§7).
3. **exec/subprocess** (exporters) — JSON over stdin/stdout, one record per line.

Go's native `plugin` package is **banned** (no Windows support), consistent with the constraint.

### 6.3 Registration pattern (Caddy/xcaddy model)

Caddy's model: a module registers itself in `init()` via `caddy.RegisterModule`, implementing an interface that returns its ID + constructor. We mirror it exactly:

```go
package rodfetcher

import "github.com/you/magpie/core"

func init() {
    core.RegisterModule(RodFetcher{})
}

type RodFetcher struct{ /* config */ }

func (RodFetcher) MagpieModule() core.ModuleInfo {
    return core.ModuleInfo{
        ID:         "fetcher.rod",
        APIVersion: "1.0.0",
        New:        func() core.Module { return new(RodFetcher) },
    }
}
```

The `magpie build` command (§10) is the xcaddy analogue: it takes `--with github.com/x/plugin@version` flags, generates a `main.go` that blank-imports each module, and runs `go build` to emit a custom static binary. Under the hood it is a thin wrapper over the Go toolchain, exactly like xcaddy (which itself is "simply making a new Go module … and go build").

---

## 7. WASM SANDBOX: wazero over Extism

Both are viable; Extism is itself built on wazero. The decision hinges on **control of the capability surface for untrusted code**:

- **wazero**: zero dependencies (only `golang.org/x/sys`), no CGO, pure-Go — preserves the single-static-binary and clean cross-compile constraints perfectly. Host functions are defined as ordinary Go closures via `HostModuleBuilder`, so we grant *exactly* the capabilities we choose and nothing else. Tested on Windows/macOS/Linux by the project. Latest v1.12.x (advancing toward Wasm 3.0); v1.8.0 aligned with Go 1.23.
- **Extism**: higher-level plugin ergonomics (manifest, memory limits, `AllowedHosts`, `AllowedPaths`, its pure-Go Go SDK is CGO-free on wazero), but it imposes its own ABI/host-function model and an embedded `extism-runtime.wasm`. For *untrusted* plugins we want the minimal, hand-audited host surface, not a general-purpose plugin framework's conveniences.

**Decision: wazero.** For a security-sensitive untrusted-plugin sandbox, the zero-dependency runtime with a hand-built host surface beats the extra abstraction layer.

### 7.1 Host-function surface granted to untrusted WASM plugins

**Granted (explicitly exported host functions):**
- `magpie_log(level, ptr, len)` — structured logging only.
- `magpie_get_input() -> (ptr,len)` — read the page/record handed to the plugin.
- `magpie_set_output(ptr, len)` — return transformed data.
- `magpie_config_get(key_ptr,key_len) -> (ptr,len)` — read whitelisted config keys only.

**Denied (no host function exists, so it is unreachable):**
- No filesystem access (no WASI `fd_*` preopens granted).
- No network/sockets.
- No environment variables, no clock beyond a coarse injected timestamp, no random unless explicitly needed.
- No process spawn.

Memory is capped (`RuntimeConfig` max pages) and each invocation runs in a fresh instantiated module (isolation between calls). CPU is bounded by a context deadline enforced by the host.

---

## 8. MCP SERVER MODE

### 8.1 SDK choice

**Decision: official `github.com/modelcontextprotocol/go-sdk`.** As of Sept 2026 it is **stable — v1.0.0 formalized a no-breaking-changes guarantee, latest stable is v1.5.0, and full protocol 2026-07-28 support landed in v1.7.0** — maintained in collaboration with Google, spec-complete, supports both stdio and Streamable HTTP, and generates tool input schemas from Go structs via reflection + variadic option overrides. mark3labs/mcp-go is excellent and was the community standard (spec 2025-11-25, native `NewStreamableHTTPServer`), but the ecosystem is consolidating onto the official SDK (e.g. Grafana migrated off mark3labs in Sept 2026). For a new project, build on the official SDK.

### 8.2 Transport: support both

Support **stdio** (default; for local MCP clients like Claude Desktop / IDEs) and **Streamable HTTP** (for remote/shared deployment). Both are first-class in the official SDK (`mcp.StdioTransport`, `mcp.NewStreamableHTTPHandler`). `magpie serve --transport stdio|http --addr :8080`. Note the 2026-07-28 spec is stateless (no init handshake) when `StreamableHTTPOptions.Stateless = true` — enables round-robin load balancing without sticky sessions.

### 8.3 Tool definitions

```jsonc
// scrape_url
{
  "name": "scrape_url",
  "inputSchema": {
    "type":"object","additionalProperties":false,
    "required":["url"],
    "properties":{
      "url":{"type":"string"},
      "schema":{"type":"object","description":"JSON Schema subset; if omitted returns cleaned markdown"},
      "render":{"type":"string","enum":["auto","static","browser"],"default":"auto"},
      "use_cache":{"type":"boolean","default":true}
    }
  },
  "outputShape": {"url","final_url","markdown","structured_data","extracted","from_cache","usage"}
}

// crawl_site
{
  "name":"crawl_site",
  "inputSchema":{"required":["url"],"properties":{
    "url":{"type":"string"},
    "max_pages":{"type":"integer","default":100},
    "max_depth":{"type":"integer","default":3},
    "same_host":{"type":"boolean","default":true},
    "schema":{"type":"object"}
  }},
  "outputShape":{"run_id","pages_crawled","records","errors","usage"}
}

// extract_structured  (extract from provided content, no fetch)
{
  "name":"extract_structured",
  "inputSchema":{"required":["content","schema"],"properties":{
    "content":{"type":"string"},"schema":{"type":"object"},
    "content_type":{"type":"string","enum":["html","markdown"],"default":"html"}
  }},
  "outputShape":{"extracted","usage","validation"}
}

// get_cached_selectors
{
  "name":"get_cached_selectors",
  "inputSchema":{"required":["domain"],"properties":{
    "domain":{"type":"string"},"schema_hash":{"type":"string"}
  }},
  "outputShape":{"domain","schema_hash","fields","synthesized_at","null_rates"}
}
```

### 8.4 Long-running crawl progress

Use **MCP progress notifications** (the SDK exposes progress tokens) for live percentage/pages-done updates during a `crawl_site` call that stays within one request. For very long crawls, return a `run_id` immediately and make `crawl_site` resumable/pollable: the client re-invokes with the `run_id` (or a status tool) to fetch progress. **Decision: progress notifications for in-band updates, plus `run_id` polling as the durable fallback for crawls that outlive a single connection.**

---

## 9. STORAGE & CONFIG

### 9.1 SQLite driver

**Decision: modernc.org/sqlite (pure-Go, no CGO).** Verified Sept 2026: actively maintained (mirror updated Sep 8 2026, imported by 3,500+ modules), supports windows/amd64 and windows/386 (v1.31.0 added windows/386), and — critically — being CGO-free is what lets us keep `CGO_ENABLED=0` for clean cross-compilation from Windows to Linux/macOS as a single static binary. The mattn/go-sqlite3 CGO driver would break the cross-compile constraint (it needs gcc/musl-dev and a host toolchain). Caveat: no concurrent writes (SQLite limitation) — use a single writer connection (`SetMaxOpenConns(1)` for the writer pool) and WAL mode for concurrent reads.

### 9.2 Schema (exact DDL)

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS selector_cache (
    schema_hash    TEXT NOT NULL,
    domain         TEXT NOT NULL,
    fields_json    TEXT NOT NULL,          -- JSON doc from §4.4
    samples_used   INTEGER NOT NULL,
    engine_version INTEGER NOT NULL DEFAULT 1,
    synthesized_at TEXT NOT NULL,           -- RFC3339
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (domain, schema_hash)
);

CREATE TABLE IF NOT EXISTS crawl_state (
    run_id        TEXT NOT NULL,
    url           TEXT NOT NULL,
    url_hash      TEXT NOT NULL,            -- SHA-256 of canonical URL
    status        TEXT NOT NULL CHECK(status IN ('pending','inflight','done','error')),
    depth         INTEGER NOT NULL DEFAULT 0,
    error_msg     TEXT,
    discovered_at TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (run_id, url_hash)
);
CREATE INDEX IF NOT EXISTS idx_crawl_status ON crawl_state(run_id, status);

CREATE TABLE IF NOT EXISTS dedup (
    run_id     TEXT NOT NULL,
    url_hash   TEXT NOT NULL,               -- SHA-256 of canonical URL
    first_seen TEXT NOT NULL,
    PRIMARY KEY (run_id, url_hash)
);

CREATE TABLE IF NOT EXISTS run_history (
    run_id            TEXT PRIMARY KEY,
    command           TEXT NOT NULL,        -- scrape|crawl|extract
    started_at        TEXT NOT NULL,
    finished_at       TEXT,
    pages_ok          INTEGER NOT NULL DEFAULT 0,
    pages_err         INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    usd_estimate      REAL NOT NULL DEFAULT 0,
    status            TEXT NOT NULL DEFAULT 'running'
);

CREATE TABLE IF NOT EXISTS llm_calls (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id            TEXT NOT NULL,
    provider          TEXT NOT NULL,
    model             TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    usd_estimate      REAL NOT NULL,
    purpose           TEXT NOT NULL,        -- synth|extract|repair
    ts                TEXT NOT NULL,
    FOREIGN KEY(run_id) REFERENCES run_history(run_id)
);
```

### 9.3 Config format & precedence

**Decision: single YAML config file, with precedence flags > env > file > built-in defaults.** File at `%APPDATA%\magpie\config.yaml` (Windows) / `$XDG_CONFIG_HOME/magpie/config.yaml`. Env vars prefixed `MAGPIE_` (e.g. `MAGPIE_EXTRACT_PROVIDER=anthropic`). Every config key has a corresponding flag.

**Writes:** `config.Save(path, mutate func(*Config) error)` (Phase K.7) round-trips the file through `yaml.Node` — comments and unknown keys survive a save, so a GUI (or future CLI `config set`) can persist `extract_provider`/`model` without clobbering hand-edits. Missing/empty file is created from `DefaultConfig()`; a mutate or parse error leaves the file byte-identical. Only fields with explicit node support are persisted — extend per caller, no generic field-mapping layer.

### 9.4 API-key handling on Windows

**Decision: zalando/go-keyring (v0.2.8, released Mar 23 2026).** One cross-platform API that maps to **Windows Credential Manager**, macOS Keychain, and Linux Secret Service; its `keyring_windows.go` calls `wincred.GetGenericCredential(...)`, so on Windows you get danieljoos/wincred's native Credential Manager behavior with no per-OS code (it stores under a `service:username` target name). Keys are stored under service `magpie`, username = provider. Precedence for reading a key: explicit flag > `MAGPIE_<PROVIDER>_API_KEY` env > OS keyring > config file (discouraged; if used, warn and require file perms 0600). Never log keys. Caveat: on headless Linux/CI the Secret Service may be absent — fall back to env var there (the standard CI pattern). Drop to wincred directly only if you go Windows-only. `magpie config set-key anthropic` prompts and stores into the keyring.

---

## 10. CLI SURFACE

```
magpie
├── scrape <url>              Fetch→clean→extract a single URL
│     --schema <file>         JSON Schema (yaml/json); omit → cleaned markdown only
│     --render auto|static|browser   (default auto)
│     --provider anthropic|openai|ollama
│     --model <name>
│     --no-cache              Bypass selector cache
│     --out <file>            Output path (default stdout)
│     --format jsonl|json|csv|sqlite   (default json)
│     --max-cost <usd>
├── crawl <url>               BFS crawl + extract
│     --max-pages N (100)  --max-depth N (3)  --same-host (true)
│     --schema <file>  (required unless --corpus)  --concurrency N  --rate <per-host-rps>
│     --corpus                 Schema-less corpus mode: {url,title,depth,markdown} JSONL per page,
│                              zero LLM calls (jsonl only; mutually exclusive with --schema)
│     --resume <run_id>       Resume a checkpointed crawl
│     --format jsonl|json|csv|sqlite
├── extract                   Extract from stdin/file (no fetch)
│     --schema <file>  --content-type html|markdown
├── serve                     Run as MCP server
│     --transport stdio|http  (default stdio)  --addr :8080
├── build                     xcaddy-style custom-binary compiler
│     --with <module@version> (repeatable)  --output <file>
├── tui                       Interactive run form → live progress → record / markdown browser+editor
│     --mouse=true|false       mouse click-select + wheel scroll (default true on TTY;
│                              keyboard-first: every action has a keybinding; see plan/phase-4-tui.md)
├── cache
│     ├── inspect [--domain d] [--schema-hash h]   Show cached selectors + null-rates
│     ├── clear   [--domain d]                     Evict entries
│     └── heal    --domain d                        Force re-synthesis
└── config
      ├── set-key <provider>   Prompt + store in OS keyring
      └── show                 Print resolved config (keys redacted)
```

### 10.1 Output formats
- **JSONL** (default for crawl): one record per line, streamable.
- **JSON**: single array (single scrape).
- **CSV**: flat schemas only; nested fields error with a clear message.
- **SQLite**: write records into a `records` table in the run DB.
- **Corpus** (`crawl --corpus`): schema-less JSONL `{url,title,depth,markdown}` per page —
  cleaned main-content text for RAG ingestion; zero LLM calls (keyless); jsonl only.

### 10.2 Exit codes
- `0` success.
- `1` generic runtime error.
- `2` CLI/usage error (bad flags).
- `3` all pages failed / no records extracted.
- `4` partial success (some pages errored) — for crawl.
- `5` robots.txt disallowed the target / blocked by policy.
- `6` cost ceiling exceeded (`--max-cost`).
- `7` config/credential error (missing API key).

### 10.3 Example invocations
```
magpie scrape https://www.amazon.fr/dp/B0XXXX --schema price.yaml --format json
magpie crawl https://books.example.com --schema book.yaml --max-pages 500 --format jsonl --out books.jsonl
echo "$HTML" | magpie extract --schema price.yaml --content-type html
magpie serve --transport http --addr :8080
magpie build --with github.com/acme/magpie-proxy@v1.2.0 --output magpie-custom
magpie cache inspect --domain amazon.fr
```

### 10.4 Egress & challenges (Phase G delta)

- **Proxy pool:** `MAGPIE_PROXY_FILE` (multi-entry; wins over
  `MAGPIE_PROXY`, which is a 1-entry pool; both win over the standard
  env proxies). Line forms: `http(s)://`, `socks5(h)://`, or vendor-paste
  `host:port:user:pass`. `#` comments. Strategies:
  `MAGPIE_PROXY_STRATEGY=round-robin|sticky-host`; `{{session}}` in an
  entry → per-target-host stable 8-hex token. Dial/CONNECT failures put
  an entry on a 60 s cooldown and the fetch fails over (≤ min(3, |pool|)
  attempts); HTTP statuses are page outcomes, never egress failures.
  All-dead → loud error listing redacted `host:port` endpoints only.
  `run_history.proxy` records the redacted serving entry; credentials
  never surface in errors/logs/records. `--proxy-file` is flag sugar
  over the env. NO_PROXY bypasses the pool (the SSRF peer check then
  applies — safe default); operator-chosen egress (any pool/proxy entry)
  skips the peer check for the proxy dial, exactly as `MAGPIE_PROXY`
  always has. Tor (`socks5://127.0.0.1:9050`) is the canonical loopback
  proxy: trusted egress, publicly-dialed targets.
- **Per-run proxy (Batch B):** `fetch.FetchRequest.Proxy` /
  `scrape.Options.Proxy` / `crawl.Options.Proxy` — a single egress URL
  (pool-line grammar) that beats the env pool for that request/run
  (flag-over-env precedence; the env pool is untouched and the override
  is not sticky). Validated pre-I/O (`fetch.ValidateRequestProxy` →
  `ErrProxyConfig` family, message names the field, never the value).
  Threads everywhere egress matters: static fetches, rod
  (`--proxy-server`; inline credentials unsupported — Chromium's flag
  grammar has none), screenshot captures, extractor sub-fetches
  (`verticalFetcher` injects it), and crawl page fetches + robots.txt
  checks (`Checker.UseProxy`, validated once in `newCrawlContext`) + rod
  escalation. The SSRF gauntlet is unchanged: a loopback TARGET via a
  public per-run proxy stays rejected pre-dial (pinned by a
  `proxy_security_test.go` matrix row).
- **Typed challenges:** `fetch.ChallengeError{Vendor, StatusCode, URL}`
  with `fetch.DetectChallenge(body, headers, status)` — `cf-mitigated`
  header authoritative; body signatures (cloudflare, turnstile,
  datadome, awswaf, hcaptcha) size-gated by the same thin-page rule as
  `clean.IsChallengePage`. Static fetch warms up + retries once on any
  challenge (typed or fingerprint-flagged); a surviving typed challenge
  is `ChallengeError` (exit 8, grouped with quality). `render=auto` gets
  one rod escalation before failing with the typed error.
  `clean.Classify` remains the final backstop for untyped challenges.
- **Page formats:** `--page-format` = `markdown|llm|text|json|html|raw|screenshot`.
  `html` = the scope-applied cleaned document (PDFs: markdown in an
  `<article>` wrapper); `raw` = decoded response body verbatim (quality
  gate still classifies); `screenshot` = full-page PNG via rod
  (`--viewport WxH`; `--out` writes bytes, else base64 in `content`;
  `render=static` is an options error). `CleanedPage.HTML` is additive
  `omitempty` — markdown goldens never drift.
- **TLS breadth:** `--browser` accepts the impersonate-http profile set
  (`chrome|firefox|safari|edge|ios|chrome_android|random`); header
  profiles mirror the library bundles so cleartext fallback matches the
  fingerprint.
- **Search:** `magpie search` (MCP tool `search`) over a data registry of
  six backends — brave, serper, serpapi, exa (BYOK via the generic key
  derivation), searxng (`MAGPIE_SEARXNG_URL`, zero-key), duckduckgo
  (zero-key default). JSONL records `{position,title,url,snippet[,page]}`;
  `--scrape-top N` runs the first N hit URLs through `scrape.Batch`.
  Missing keys exit 7; unknown providers exit 2. The client rides the
  guarded transport with an explicit `AllowPrivate` opt-in — every peer
  of the search client is operator-configured (fixed provider endpoints
  or `MAGPIE_SEARXNG_URL`; a localhost searxng is the canonical
  zero-key deployment), while SERP hit URLs scrape through the strict
  pipeline.

### 10.5 P1 differentiation (Phase H delta)

- **Actions DSL:** `scrape --action` (repeatable) / `--actions <file>` /
  MCP `scrape_url` `actions` run rod interactions before capture: `click
  <sel>` · `type <sel> <text…>` · `scroll <n|top|bottom>` · `wait <ms>`
  (cap 30000) · `wait-for <sel>` · `screenshot <path>` · `eval-js
  <expr…>`. One action per line; `type`/`screenshot`/`eval-js` take the
  rest of the line (no quoting); `#` comments skipped; errors name the
  verb + 1-based line and exit 2 pre-I/O. Actions force browser
  rendering (`render=static` rejected); `RodFetcher.Fetch` is a nil-
  actions delegate of the same code path, so non-action behavior cannot
  drift. `screenshot` is CLI-only — `handleScrape` rejects the verb at
  the MCP boundary (an action line must never become an agent's
  server-side file-write); `page_format screenshot` stays base64-through-
  envelope. The fetch keeps a fixed settle: no implicit network-idle —
  slow pages get explicit `wait`/`wait-for` lines.
- **Watch:** `magpie watch <url> --every <d> [--once] [--webhook URL]`
  stores a markdown snapshot per check (snapshots table, `checked_at`
  RFC3339Nano under PK `(url_hash, checked_at)` — append-only, no
  pruning), word-diffs on change, and fires exactly one POST
  `{url, changed, old_hash, new_hash, diff}`. `--every` floors at 30s;
  `--once` is the cron mode; Ctrl-C exits 0. The webhook client is
  built inline with `AllowPrivate: true` (operator-chosen endpoint,
  searxng trust tier — never shared), single attempt; failure lands in
  `WatchResult.WebhookStatus`, never fails the check. Checks are
  markdown-only by construction (schema cleared) — zero LLM.
- **Locale:** `--lang` (scrape, batch, crawl, watch, MCP `lang`) sets
  `Accept-Language` verbatim, overriding the profile bundle; values with
  control characters are rejected at `ValidateOptions` (header-injection
  boundary). No `--country` flag: a country implies IP geo, which is
  already expressible as a proxy-pool entry; a fake knob would silently
  do nothing on direct connections.
- **Vertical breadth (Phases V+W):** the registry is **27 extractors**.
  Phase W adds six host/product verticals: `etsy_listing`, `ebay_item`
  and `amazon_product` (marketplace listings — JSON-LD first with buy-box
  DOM fallback, except amazon which is DOM-only by design; IDs from the
  URL, never the page), `woocommerce_product` (any `/product/` permalink,
  **OptIn** — the shape is nobody's host), `substack_post` (rewrites
  `/p/{slug}` to the same-origin posts API, so custom-domain pubs work;
  `Match` stays `*.substack.com`-ceiled) and `dev_to_article`
  (crayons-class DOM selectors). This covers the earlier schema.org
  standards wave — `job_posting` (JobPosting
  JSON-LD: Greenhouse/Lever/Ashby-style boards), `event` (Event +
  subtypes, online/physical disambiguation), `local_business`
  (LocalBusiness + subtypes, address/geo/hours), `article`
  (Article/NewsArticle/BlogPosting) and `rss` (RSS 2.0 + Atom via stdlib
  XML, capped at 50 items — the fuel for `watch`/`diff`) — alongside the
  earlier `stackoverflow` (SE API, `filter=withbody`),
  `trustpilot` (embedded JSON-LD via goquery — no JSON-LD is a loud
  error), `dockerhub`, `huggingface` (models carry `pipeline_tag`,
  datasets omit it), `upwork_job` (typed challenge surface) and reddit
  permalinks going `.json`-first, emitting nested `comments` trees capped
  at depth 10 / 200 total (the `more`-object pagination is the upgrade
  path); the HTML summary shape remains the fallback. The standards
  extractors share one JSON-LD block scanner (`vertical/jsonld.go`).
  `og` — and the entire standards wave — are **OptIn by design**
  (deviation from the competitive analysis):
  our contract guarantees permissives never auto-fire — an always-on
  extractor would attach a `Record` to every scrape and silently change
  the default output shape for all users; explicit `--vertical og`/
  MCP selection covers the use case honestly. Embedders register
  proprietary per-site verticals at startup via `vertical.Register`.
  `crawl --status <run_id>`
  is CLI parity for the MCP poll (unknown id exits 4, handler-local).

### 10.6 Competitive-parity delta (Phase J)

Five features closing the Scrapling comparison gaps, all additive (new flags,
additive-omitempty fields) except the hidden-text strip, which changes
`clean.Clean` output for pages that contain hidden text:

- **Prompt-injection stripping (default-on):** `clean.Clean` removes hidden
  text at the choke point, before trafilatura/markdown conversion and before
  any consumer (CLI, MCP, crawl, watch, corpus): inline styles matching
  `display:none`, `visibility:hidden`, `font-size:0`, `opacity:0`
  (lowercased-regexp match, whitespace/case tolerant), the `hidden`
  attribute, and HTML comment nodes. `aria-hidden` is deliberately **out of
  scope** (icon fonts / decorative markup make it low-precision), as are
  computed styles (white-on-white, off-screen) — the strip is selector-level,
  not a CSS engine. Pages with nothing hidden are returned **byte-identical**
  (no re-serialization, so goldens and sidecars never drift). Escape hatch:
  `--page-format raw` bypasses `Clean` entirely. The strip never fails a
  page; `Classify` scores the stripped document by design (quality reflects
  what consumers actually see).
- **`--capture-xhr` (repeatable Go regexps; MCP `capture_xhr`):** returns the
  XHR/fetch response bodies the page itself loaded (rod passive network
  events — `GetResource` cannot see XHR; hijack would perturb the page).
  Capture is restricted to XHR/Fetch resource types matching any pattern;
  caps: **50 captures × 64 KiB body** — past a cap the data is truncated
  (`"truncated":true`) or dropped, never an error; a CDP-evicted body
  ("-32000") becomes metadata-only. Capture requires browser rendering
  (`render=static` rejected at the exit-2 boundary, screenshot wording);
  the bodies ride `FetchResponse.XHR` → CLI markdown envelope `xhr` array
  (and the `page_format json` envelope when non-empty) → MCP `ScrapeOut.xhr`.
  Crawl deliberately has no capture (v1: scrape-only — corpus records have
  nowhere to put bodies).
- **`--auto-throttle` (MCP `auto_throttle`):** adaptive per-host pacing, off
  by default (Scrapy ships it off too — turning pacing into a moving target
  unannounced would surprise existing users and goldens). `Report` fires per
  attempt **inside** the `FetchWithRetry` closure (backoff.go consumes
  intermediate 429/503s, so post-hoc wiring would see only final 200s and
  the backoff would be dead code): delay ×2 on 429/5xx (cap **60s**), an
  explicit `Retry-After` sets the delay directly (cap **5m**), 2xx decays ×¾
  toward the floor — max(configured 1/rps, `Crawl-delay`). It paces FUTURE
  requests to the host and is complementary to the same-request retry
  taxonomy in `crawl/backoff.go`. **Static path only:** the rod-escalation
  branch bypasses `Report` (rare path, acceptable).
- **`crawl --sitemap-only` (MCP `sitemap_only`):** frontier = scope-filtered
  sitemap URLs only — no seed enqueue (the seed is fetched only if the
  sitemap lists it), no link-following. `maxPages` caps and scope filters
  apply unchanged. Zero in-scope URLs → typed `ErrSitemapOnlyEmpty` →
  **exit 3** (documented contract; never a silent 0-page success), and an
  expansion error is fatal in this mode (there is no seed-only fallback).
  `--sitemap-only --no-sitemap` is contradictory → **exit 2** at both edges
  (one `crawl.ValidateSitemapOnly` predicate shared by CLI and MCP).
- **`--cdp-url` / `MAGPIE_CDP_URL` (MCP `cdp_url`):** drive a remote or
  already-running browser via CDP (`ws://`, `wss://`, or `http(s)://`; scheme
  validated pre-I/O, exit 2; flag > env, the MAGPIE_PROXY pattern). With a
  CDP endpoint set, `ensureBrowser` connects directly and the local launcher
  never runs — **no local Chromium download/launch** (farms, containers,
  browsers-as-a-service). Userinfo in the endpoint is redacted in errors
  (never surfaced). Applies to scrape browser fetches and screenshots; scrape
  accepts it even under `render=auto` (it only matters if a browser runs).

---

## 11. POLITENESS & SAFETY DEFAULTS

- **robots.txt: jimsmart/grobotstxt** (v1.0.3). A faithful native Go port of Google's official C++ robots.txt parser/matcher (Apache-2.0, stdlib-only runtime deps), so matching semantics equal Googlebot's; exposes `AgentAllowed` and `Sitemaps`. temoto/robotstxt is the other common choice and is fine, but grobotstxt's fidelity to Google's reference implementation is the tiebreaker. Fetch and cache `/robots.txt` per host; enforce `AgentAllowed` before every fetch; honor `Crawl-delay` where present as a floor on the per-host rate limiter; parse `Sitemap:` directives to seed the frontier. Respect robots by default; a `--ignore-robots` flag exists but prints a prominent warning and is off by default.
- **Default rate limits:** 1 req/s per host, burst 3 (§5.3); global concurrency defaults conservative (8 static fetch / 2 browser).
- **User-agent policy:** default UA identifies the tool and a contact URL (e.g. `magpie/1.0 (+https://github.com/you/magpie)`). Browser-like UAs are opt-in via config for sites that block generic bots; never impersonate by default.
- **Explicitly out of scope (core):** no login-wall / paywall bypass, no CAPTCHA solving, no anti-bot evasion/stealth fingerprinting, no credential stuffing. These may only ever exist as user-supplied plugins, never in core. Aligned with the logged-out Amazon.fr constraint: the repricing consumer reads public price/facts only.

---

## 12. TESTING STRATEGY

- **Golden-file tests for clean & extract:** store input HTML fixtures + expected cleaned markdown / expected JSON under `testdata/`. `go test -update` regenerates goldens. This locks cleaning-contract behavior across go-trafilatura/html-to-markdown upgrades (e.g. the v2.5.0 empty-text-node panic that combining go-trafilatura output with html-to-markdown exposed, fixed upstream in v2.5.1 — golden tests catch regressions like it).
- **httptest fixtures for fetch:** `httptest.Server` serving canned HTML (static, SPA-shell, redirect chains, 429/503, gzip) to test the auto-detect heuristic, redirect policy, retry taxonomy, and rate limiter deterministically — no network.
- **Deterministic selector self-healing tests:** serve version A of a page (selectors valid), assert cache hit + 0 LLM calls; swap to version B (price selector broken) via the httptest handler, feed enough pages to cross the null-rate threshold, assert re-synthesis fires **for the price field only** while other fields keep serving. LLM calls are mocked with a fake `Extractor` returning fixed ground-truth, so healing logic is tested without a real model.
- **Headless browser path in CI:** GitHub Actions matrix on `windows-latest` and `ubuntu-latest`. Chrome is provided via **`browser-actions/setup-chrome@v2`** (cross-platform incl. Windows, installs Chrome-for-Testing + matching ChromeDriver; actively maintained through 2026, with recent Windows-ARM64 snapshot support). Gate browser tests behind a build tag (`//go:build browser`) so the default `go test ./...` stays fast and hermetic; run the tagged suite as a separate CI job.
- **WASM plugin tests:** compile a tiny fixture plugin to `.wasm`, load via wazero, assert the denied host functions are truly unreachable (a plugin attempting network/file ops fails to instantiate or traps).

---

## 13. BUILD PLAN

### Repo layout
```
magpie/
├── cmd/magpie/main.go               # Cobra root, blank-imports built-in modules
├── core/                         # module registry, RegisterModule, ModuleInfo, version gate
│   ├── registry.go
│   ├── module.go
│   └── pipeline.go               # channel wiring, errgroup stages
├── fetch/
│   ├── http.go                   # net/http static fetcher
│   ├── rod.go                    # go-rod browser fetcher
│   └── detect.go                 # SPA/JS-required heuristic
├── clean/
│   ├── trafilatura.go
│   ├── markdown.go
│   └── contract.go               # strip/preserve rules, JSON-LD sidecar
├── extract/
│   ├── extractor.go              # interface + repair loop
│   ├── openai.go  anthropic.go  ollama.go
│   ├── schema.go                 # x-magpie parsing, santhosh-tekuri validation
│   └── cost.go
├── selector/
│   ├── synth.go  validate.go  heal.go  cache.go
├── crawl/
│   ├── frontier.go  bloom.go  ratelimit.go  backoff.go
├── store/
│   ├── sqlite.go                 # modernc driver, migrations, DDL
├── mcp/
│   ├── server.go  tools.go       # official go-sdk
├── plugin/
│   ├── wasm/host.go              # wazero host-function surface
│   ├── exec/exporter.go          # subprocess exporter protocol
├── config/
│   ├── config.go  keyring.go     # zalando/go-keyring
├── build/                        # xcaddy-style compiler for `magpie build`
├── tui/                           # Bubble Tea interactive UI (Phase 4; see plan/phase-4-tui.md)
└── testdata/
```

### Milestone 1 — Week 1: single-URL fetch→clean→extract
- **Dirs/packages:** `cmd/magpie`, `core`, `fetch` (http + detect), `clean`, `extract`, `config`, `store`.
- **Dependencies (pinned):**
  - `github.com/spf13/cobra` (CLI)
  - `github.com/go-rod/rod` v0.116.x (browser; wire even if detect rarely triggers)
  - `github.com/markusmobius/go-trafilatura/v2` (needs Go 1.26+)
  - `github.com/JohannesKaufmann/html-to-markdown/v2` v2.5.2
  - `github.com/santhosh-tekuri/jsonschema/v6`
  - `modernc.org/sqlite` (latest, ~Sep 2026) — pin `modernc.org/libc` to the exact version from its go.mod
  - `github.com/zalando/go-keyring` v0.2.8
  - `github.com/PuerkitoBio/goquery` (CSS apply)
  - Provider SDKs: official OpenAI Go, Anthropic Go, Ollama via HTTP.
- **DoD checklist:** `magpie scrape <url> --schema x.yaml` returns valid JSON; auto-detect routes an SPA URL to go-rod; cleaning golden tests pass; JSON-LD sidecar harvested; santhosh-tekuri validation + 3-attempt repair loop working; cost logged to `llm_calls`; API key read from keyring/env; builds `CGO_ENABLED=0` on Windows and cross-compiles to linux/amd64 + darwin/arm64.

### Milestone 2 — Weeks 2–4: selector cache + concurrency + crawl
- **Dirs/packages:** `selector`, `crawl`, expand `core/pipeline`, expand `store`.
- **Dependencies added:**
  - `golang.org/x/sync/errgroup`
  - `golang.org/x/time/rate`
  - `github.com/cenkalti/backoff/v5`
  - `github.com/bits-and-blooms/bloom/v3`
  - `github.com/jimsmart/grobotstxt`
- **DoD checklist:** selector synthesis from N=3 samples with field-level validation against direct-LLM ground truth; cache keyed `(domain, schema_hash)` persisted in SQLite; steady-state pages extract with **0 LLM calls**; null-rate self-healing triggers per-field re-synthesis (deterministic test passes); `magpie crawl` does bounded-concurrency BFS with per-host rate limiting, bloom+SQLite dedup at 100k URLs, backoff retries with correct retryable/permanent taxonomy; SIGINT drains and checkpoints; `--resume` works; robots.txt enforced.

### Milestone 3 — MCP + plugins
- **Dirs/packages:** `mcp`, `plugin/wasm`, `plugin/exec`, `build`.
- **Dependencies added:**
  - `github.com/modelcontextprotocol/go-sdk` (v1.5.0 stable; v1.7.0+ for 2026-07-28 spec)
  - `github.com/tetratelabs/wazero` v1.12.x
- **DoD checklist:** `magpie serve` exposes `scrape_url`, `crawl_site`, `extract_structured`, `get_cached_selectors` over both stdio and Streamable HTTP; progress notifications during crawl + `run_id` polling; WASM plugins load in wazero with the locked-down host surface (denied capabilities proven unreachable by test); exec exporters run as subprocesses over JSONL; `magpie build --with` produces a custom static binary via the Go toolchain (xcaddy model); compile-time module registration + semver version gate enforced; CI green on Windows + Linux including the tagged browser suite.

---

## Recommendations (staged)
1. **Start narrow, prove the cache.** Build Milestone 1, then immediately validate the selector-synthesis-and-cache loop on 3–5 real Amazon.fr product pages before building crawl/concurrency. The BardeenAgent ablation (recall 66.2%→35.6% without the selector model) says this is where the value is — if synthesis+validation doesn't hold up on your real target, nothing downstream matters. **Benchmark that changes the plan:** if field-level selector agreement on 3 samples is < 80% for price/EAN, invest in the heuristic (non-LLM) generator or fall back to per-page LLM extraction for those fields rather than over-engineering the cache.
2. **Keep go-rod optional and lazy.** Most Amazon.fr price/fact data lives in embedded JSON (`__NEXT_DATA__`-style) or JSON-LD; prefer static + sidecar harvesting and only pay for a browser when the detector scores ≥ 2. **Threshold:** if > 40% of target pages escalate to browser, revisit the detector weights before scaling concurrency (browser workers are the memory bottleneck at 2 concurrent by default).
3. **Pin the volatile deps and watch them.** go-rod upstream maintainer activity has slowed and the MCP spec is still moving (2026-07-28 went stateless). Pin exact versions, vendor, and gate upgrades behind the golden-file + browser CI suites. If go-rod upstream stalls further, evaluate the `rah-0/rod` fork.
4. **Defer plugins until the core is stable.** Milestone 3's WASM/exec system is the most speculative; a solo dev gets more value from a rock-solid single-binary core than from an untrusted-plugin sandbox nobody has written plugins for yet. Ship MCP first within Milestone 3, WASM last.

## Caveats
- **Vendor blogs and secondary sources** inform several quality/token-reduction figures (the "~3× smaller markdown," ScrapeGraphAI cost-per-page). Treat token-reduction as an approximate 2–3× central estimate (measured 67–68% by web2md/AlterLab; one independent test found 7.4× median; some vendors cite only 20–30%). Benchmark on your own corpus. The go-trafilatura F1/precision figures come from the maintainer's README and the independent scrapinghub benchmark and should likewise be re-checked on your target sites.
- **go-trafilatura v2 requires Go 1.26+** and is Apache-2.0 (vs MIT for readability); confirm this is acceptable for your licensing.
- **SQLite single-writer** is a hard constraint of the engine — the design routes all writes through one connection; extremely high write throughput would need a different store, but at 100k-URL crawl scale WAL + single writer is sufficient.
- **modernc.org/sqlite** requires importing the exact `modernc.org/libc` version from its go.mod to avoid build issues — pin both together.
- **Selector self-healing thresholds** (null-rate 0.30, window 50 pages, N=3 samples) are starting points; tune per target site's volatility.
- **MCP progress over long crawls**: if a crawl outlives the client connection, in-band progress notifications stop; the `run_id` polling fallback is the durable path. Verify your MCP client supports progress tokens.
- **Anthropic structured outputs** are GA as of Sept 2026 (`output_config.format`, no beta header). Forced `tool_choice` is rejected on Claude Fable 5.1, so the extractor must never depend on forcing a tool call.
- **html-to-markdown/v2** does not sanitize untrusted content and markdown cannot represent colspan/rowspan cleanly — the table plugin's `WithSpanCellBehavior` option (§2.2) addresses the latter.