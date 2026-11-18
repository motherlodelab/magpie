# Phase AA — Multi-store crawl (`--seeds-file`) + Shopify catalog fast path (`magpie shopify`)

**Duration:** ~2 days (~12h)
**Depends on:** Nothing — both features are additive on the shipped `crawl` engine and `vertical` registry
**Blocks:** The client-delivery ops runbook (thousands of e-commerce stores across countries in 24h): AA gives it its two workhorse commands
**Risk Level:** MEDIUM — two additive features; the only behavior change (multi-seed robots = skip-not-abort) is gated behind the new flag and single-seed behavior is preserved exactly
**Stack:** go (per AGENTS.md — `run-phase` hard-blocks on this stack; execute steps manually)

---

## Objective

Grounded in a real client requirement: collect product data from many e-commerce
stores across most countries **within one day**. Two gaps block that today:

1. `magpie crawl` takes exactly ONE seed URL (`crawl.Options.SeedURL`) —
   2,000 stores means 2,000 process invocations with no shared resume, no shared
   summary, and a per-store shell harness to maintain. **P1:** `--seeds-file`
   gives one run, one frontier, one resume id, per-host fairness for free
   (HostLimiters are already per-host).
2. Shopify stores (the majority of the world's small e-com) expose the entire
   catalog at `/products.json?limit=250&page=N` — 12,500 products in 50 requests.
   Today magpie only fetches `/products/{handle}.js` **per product URL**
   (`vertical/commerce.go` `extractShopify`), which needs a handle list from
   crawl/sitemap first. **P2:** a `magpie shopify <store>` command that
   paginates the catalog endpoint directly and emits one product record per
   product with zero per-product fetches and zero LLM calls.

The two features compose in a shell script: Shopify stores ride `magpie shopify`
(fast path), everything else rides `magpie crawl --seeds-file`.

## What Success Looks Like

1. `go test ./crawl/ ./vertical/ ./cli/` passes with the new tests; touched
   packages clean under `-race`; `go build ./... && go vet ./... &&
   golangci-lint run ./... && gofmt -l .` all clean.
2. `printf 'http://127.0.0.1:PORT/\nhttp://127.0.0.1:PORT/b/\n' | magpie crawl
   --seeds-file - --corpus --max-pages 10` crawls BOTH seeds in one run; one
   `run_id`; `--status <run_id>` shows combined counts.
3. A seeds file whose first seed is robots-disallowed crawls the REST and warns
   on stderr (single-seed robots-block still exits 5 unchanged).
4. `magpie shopify http://127.0.0.1:PORT` against an httptest `/products.json`
   fixture emits one JSONL record per product, stopping at the short page;
   records match `extractShopify`'s shape field-for-field.
5. `magpie shopify` against a store returning 404 on `/products.json` exits 3
   with a clear `no catalog` message (new sentinel `vertical.ErrNoCatalog`,
   armed in `cli.exitCode`).
6. `magpie crawl --help` documents `--seeds-file`; README crawl + shopify
   sections and the exit-code table are updated.

**Good:** "seeds-file run produces ONE run row with pages_ok = sum over seeds; shopify emits `{title,vendor,price,type,images,url}` per product, 2 requests per 500 products"
**Bad:** "multi-store crawling works"

---

## Architecture / Key Design Decisions

```
P1:  seeds file ──┐
                  ├─> validate lines (exit 2 pre-I/O) ─> crawl.Options.Seeds
  crawl <url>  ───┘                                        │
                                      seed(): for each seed (skip on Resume):
                                        robots check (multi: skip+warn, single: abort as today)
                                        sitemap expansion (base = THIS seed, cap per seed)
                                      → ONE frontier.Add(all, depth 0) → existing pump
                                        (per-host rate limits + bloom dedupe + resume tail
                                         unchanged — run row never persisted the seed)

P2:  magpie shopify <store> ─> fetch /products.json?limit=&page=1..N (StaticFetcher,
                               env proxy pool, TLS fingerprint, no render)
                               → stop on short/empty page or --max-pages
                               → shopifyProductRecord() shared with extractShopify
                               → stream JSONL to stdout/--out
```

### Decisions (locked)

- **D1 — `Seeds []string` alongside `SeedURL`, not instead.** MCP `crawl_site`
  (`mcp/tools.go:222`) and every embedder keep `SeedURL` untouched. Inside
  `crawlContext`, a `seedList()` helper returns `Seeds` if non-empty else
  `[]string{SeedURL}`. Zero MCP churn, zero breaking change.
- **D2 — robots: per-seed skip in multi-seed mode, abort preserved in
  single-seed mode.** One store's robots.txt must not kill a 2,000-store run.
  Rule in `seed()`: `len(seeds) == 1` → exactly today's behavior (abort,
  `FinishRun "robots_blocked"`, `ErrRobotsBlocked` → exit 5); `len(seeds) > 1`
  → stderr warn `crawl: seed %s: robots disallows; skipping`, seed excluded
  (including its sitemap expansion), run proceeds. **If ALL seeds are
  robots-blocked → return `ErrRobotsBlocked`** (an all-blocked run is a
  blocked run, not a silent empty one).
- **D3 — sitemap expansion stays per-seed with that seed as scope base.**
  `scopeFilterExpansion(scope, seedURL, expanded)` is already parameterized —
  just call it inside the loop. The `expanded[:c.maxPages]` cap stays per-seed
  (global budget is enforced by the pump's `claimed >= c.maxPages`), so with
  N stores you raise `--max-pages` to the global budget you actually want.
  Document this in the flag help: max-pages is the GLOBAL page budget.
- **D4 — one `frontier.Add` for all seeds.** Dedupe of repeated store lines is
  free (bloom filter + DB frontier). Depth 0 for every seed; `--max-depth`
  counts from each seed independently (existing per-page depth mechanics).
- **D5 — resume needs no change.** `seed()` returns early on `Resume` and the
  run row never stored the seed; a multi-seed run resumes purely from the
  persisted frontier. CLI still requires a source (arg xor `--seeds-file`) on
  resume for interface stability; it is read but unused on resume — documented.
- **D6 — seeds file format mirrors `cli/batch.go`'s `--file` reader** (trim
  spaces, skip blanks, `-` = stdin, 1MB line buffer), PLUS: reject lines that
  don't parse as absolute `http(s)` URLs **pre-I/O** with
  `crawl: seeds file line N: not an absolute http(s) URL` → exit 2 (trust
  boundary: fail loudly before any fetch). Empty file → `fail(2)`. No URL cap —
  thousands of stores is the point (batch's 100-URL cap does NOT apply).
- **D7 — catalog is NOT a vertical `Extractor`.** The registry shape is
  one-URL → one-record (`vertical.Extractor.Extract`); a paginator is a
  different shape. It lives in `vertical/shopify_catalog.go` as a plain
  function with an `emit` callback (streaming keeps memory flat for 30k-product
  stores and lets the CLI write incrementally). `magpie vertical --list`
  stays untouched.
- **D8 — one record mapper shared by `.js` and catalog paths.** Factor
  `extractShopify`'s record building into
  `shopifyProductRecord(m map[string]any, scheme, host, handle string) map[string]any`
  (same `{title, vendor, price, type, images, url}` keys, currency still never
  guessed — both payloads omit it). **Price gotcha (pin this):** `/products/*.js`
  carries integer **cents** (`price / 100`); `/products.json` carries variant
  prices as formatted **strings** in major units (`"24.99"`) plus
  `price_min`/`price_max` strings. Catalog mapper: `variants[0].price` string →
  `strconv.ParseFloat`; no variants → `price_min`; neither → key omitted.
  A parity test asserts the two paths produce equal records for the same
  product fixture.
- **D9 — catalog fetch path is static-only, env-proxy-pool only.** `products.json`
  is JSON — no rod escalation. CLI offers `--browser` (TLS fingerprint) and
  `--lang`, validated by the existing `scrape.ValidateOptions` — same pattern as
  `cli/crawl.go`. No `--proxy` flag: crawl CLI has none either; the env proxy
  pool applies automatically (`fetch` honors it). Politeness: ≤
  `--max-pages` (default 50) small JSON requests per store — no rate flag
  (`ponytail:` ceiling noted in code: a page-per-second hammer is possible
  today; add pacing only if a store ever complains).
- **D10 — new sentinel `vertical.ErrNoCatalog`** for `products.json` returning
  404 (catalog disabled) or an HTML/parse failure on page 1. Wired end to end
  per house rule: sentinel in `vertical` → `errors.Is` arm in `cli.exitCode`
  (`cli/root.go:93`) → **exit 3** (no-pages semantics) → README exit-table row
  → an asserting test. Network/transport errors stay plain exit 1.

### Data Model Rules (Go — house pattern: plain structs, no frameworks)

| Item | Type | Why |
|------|------|-----|
| `crawl.Options.Seeds` | `[]string` field on the existing Options struct | additive; empty = old single-seed semantics |
| `vertical.CatalogOptions` | `struct{ Limit, MaxPages int }` | two ints; validated in the CLI edge, defaults applied there (library takes them literally) |
| `vertical.Catalog` | `func(ctx, f Fetcher, store *url.URL, opts CatalogOptions, emit func(map[string]any) error) (pages int, err error)` | callback = streaming; `pages` returned for the CLI summary line |
| Records | `map[string]any` (existing vertical convention) | same shape crosses `extractShopify`, catalog, and marketplace verticals — don't invent a struct |

---

## Tasks

### Task AA.1 — `crawl.Options.Seeds` + per-seed seed() loop (3.5h)

**TDD: write `crawl/seeds_multi_test.go` first (RED), then implement.**

1. Add `Seeds []string` to `crawl.Options` (after `SeedURL`, comment: "multi-seed
   mode; SeedURL ignored when set").
2. In `crawlContext`, add `seedList()` (Seeds if non-empty else `[SeedURL]`).
3. Rewrite `crawl/seed.go seed()` as a loop over `seedList()` implementing D2/D3/D4:
   - `Resume` early-return stays first.
   - Per seed: robots check → single-seed failure keeps today's abort path
     verbatim; multi-seed failure = stderr warn + `continue` (remember skips;
     all-skipped → finish run + return the existing wrapped `ErrRobotsBlocked`).
   - Sitemap expansion per seed: `ListSitemapURLs(ctx, c.static, seed)` →
     `scopeFilterExpansion(c.scope, seed, expanded)`; `SitemapOnly` logic
     evaluated per seed; `ErrSitemapOnlyEmpty` only when the TOTAL sitemap-only
     frontier across all seeds is empty (wrap with seed context in the message).
   - Collect all seeds+expansions, then ONE `frontier.Add(all, 0)` +
     `outstanding.Add(n)`.
4. Tests (hermetic, httptest, mirror `crawl_test.go` fakes — reuse existing
   fake fetcher/checker helpers verbatim):
   - two seeds crawl in one run, one run row, records from both;
   - multi-seed: seed A robots-disallowed → skipped+warned, seed B crawled,
     run status `ok`;
   - all seeds robots-disallowed → `ErrRobotsBlocked`;
   - duplicate seed line → crawled once;
   - `Resume` still ignores seeds entirely (existing resume test stays green).

**Sanity check:** `go test ./crawl/ -run Seeds -race`

### Task AA.2 — CLI `--seeds-file` (2h)

**TDD: extend `cli/crawl_test.go` (or a new `cli/crawl_seeds_test.go`) first.**

1. Flag: `--seeds-file` "store URL list file, one per line (- = stdin); mutually
   exclusive with the <url> argument; read but unused on --resume".
2. `Args`: 0 args allowed iff `--seeds-file != ""` and `--status == ""`;
   arg + `--seeds-file` → `fail(2)` contradiction.
3. Reader as a small `readSeedsFile(path string) ([]string, error)` in
   `cli/crawl.go` following `cli/batch.go`'s scanner block (D6): trim, skip
   blanks, `-` = stdin, `bufio.Scanner` with the 64KB/1MB buffer, wrap errors
   as `crawl: read %s: %w`. Validate each line with `url.Parse` + scheme check
   → line-numbered error → `fail(2)`. Zero valid lines → `fail(2)`.
4. `runCrawl` passes `Seeds: seeds` into `crawl.Options`; `SeedURL` left empty
   in file mode.

**Sanity check:** `printf 'http://127.0.0.1:PORT1/\n' | go run . crawl --seeds-file - --corpus --max-pages 1`

### Task AA.3 — Docs for P1 (0.5h)

- README crawl section: new flag row + one example (multi-store one-liner);
  note "max-pages is the global budget across all seeds".
- README exit-table: robots exit 5 note now reads "single-seed runs abort;
  multi-seed runs skip the blocked seeds (exit 5 only if ALL seeds are blocked)".

### Task AA.4 — `vertical/shopify_catalog.go` + shared record mapper (3h)

**TDD: `vertical/shopify_catalog_test.go` first (RED).**

1. Factor the record building out of `extractShopify` into
   `shopifyProductRecord(m, scheme, host, handle)` (D8); `extractShopify` calls
   it — its existing tests must stay green untouched.
2. `CatalogOptions{Limit, MaxPages int}` + `Catalog(ctx, f, store, opts, emit)`:
   - page URL: `{scheme}://{host}/products.json` with
     `?limit=N&page=P` (`url.Values`), N ≤ 250 (clamp or reject — reject >
     250: `vertical: shopify: limit above Shopify's 250 per page`).
   - fetch via the existing `fetchJSON` helper; 404 (or non-JSON body) on
     **page 1** → `fmt.Errorf("vertical: shopify: %s: %w", store.Host,
     ErrNoCatalog)`; transport errors surface as-is.
   - emit each product through `shopifyProductRecord` (handle from the payload's
     `handle` field); stop when `len(products) < limit`, page empty, or
     `page > MaxPages`. Return pages fetched.
3. Price per D8 (ParseFloat on variant string; fall back `price_min`; omit when
   neither). Test the cents-vs-string gotcha explicitly.
4. Parity test: one product fixture served both ways → records equal.
5. Pagination tests: limit=2 with 3 products → 2 pages, 3 records, stop on
   short page; `--max-pages` cap honored; empty page → stop.

**Sanity check:** `go test ./vertical/ -run Catalog -race`

### Task AA.5 — `ErrNoCatalog` sentinel wiring (0.5h)

1. `var ErrNoCatalog = errors.New("vertical: store catalog unavailable")` in
   `vertical/shopify_catalog.go` (message prefix = package convention).
2. Arm in `cli/root.go exitCode`: `errors.Is(err, vertical.ErrNoCatalog)` →
   `code = 3`, grouped beside `ErrSitemapOnlyEmpty` with a matching comment.
3. Test in `cli` exit-code table test (`exitfor_test.go` pattern).

### Task AA.6 — `magpie shopify` command (2h)

**TDD: `cli/shopify_test.go` first (RED) — httptest fixture store.**

1. `cli/shopify.go`: `Use: "shopify <store-url>"`, flags: `--limit` (250),
   `--max-pages` (50), `--out` ("" = stdout), `--format` (jsonl|json; default
   jsonl), `--browser`, `--lang`.
2. Edge validation before any I/O: exactly 1 arg; absolute http(s) URL →
   `fail(2)`; `scrape.ValidateOptions{Browser, Lang}` (same call as crawl);
   limit > 250 → `fail(2)`. No schema, no provider, no API key — keyless like
   `--corpus`.
3. Fetcher: `fetch.NewStaticFetcher()` (zero-arg constructor, verified) reused
   for all page fetches; TLS fingerprint and language ride the **per-request**
   fields `fetch.FetchRequest{Browser:…, Lang:…}` (fetcher.go:14–19) exactly as
   scrape does; proxy pool comes from env automatically.
4. Writer: JSONL — `json.Encoder` per record into a `bufio.Writer` on
   stdout/`--out` (mirror batch's per-line encoding so partial runs keep their
   records; `--format json` buffers all and prints one array). Finish with a
   stderr summary line: `shopify: N products, P pages, store HOST`.
5. Tests: happy path record stream + summary; 404 page 1 → `ErrNoCatalog` →
   `exitCode` 3; bad URL / limit>250 → exit 2 pre-I/O (zero fetches — assert
   the fixture server saw no request).

**Sanity check:** `go run . shopify http://127.0.0.1:PORT | head -3`

### Task AA.7 — Docs for P2 + full gates (0.5h)

- README: `magpie shopify` section under the verticals/commands area (what it
  does, the 250/page reality, examples) + exit-table row: 3 now also covers
  "store catalog unavailable (`ErrNoCatalog`)".
- Gates: `go build ./... && go vet ./... && golangci-lint run ./... && gofmt -l .`
  (must print nothing) + `go test ./crawl/ ./vertical/ ./cli/ -race` +
  full `go test ./...` within the ≤2min budget (new tests are httptest-fast).

---

## Deliverables

```
magpie/
├── crawl/
│   ├── crawl.go             # +1 field: Options.Seeds
│   ├── seed.go              # seed() loop: per-seed robots/expansion (D2–D4)
│   └── seeds_multi_test.go  # multi-seed: one run, robots skip, dedupe, resume
├── vertical/
│   ├── commerce.go          # extractShopify → shared shopifyProductRecord
│   ├── shopify_catalog.go   # Catalog paginator + ErrNoCatalog + CatalogOptions
│   └── shopify_catalog_test.go # pagination, price gotcha, shape parity
├── cli/
│   ├── crawl.go             # --seeds-file flag + readSeedsFile + wiring
│   ├── crawl_seeds_test.go  # flag contradiction, reader, end-to-end seeds run
│   ├── shopify.go           # magpie shopify command
│   ├── shopify_test.go      # fixture store: stream, exit 3, exit 2 pre-I/O
│   └── root.go              # +1 arm: ErrNoCatalog → exit 3
└── README.md                # crawl --seeds-file, magpie shopify, exit table
```

---

## Exit Criteria

- [ ] `go test ./crawl/ -run Seeds -race` — new multi-seed tests pass (one run row for two seeds; robots skip warn; all-blocked → `ErrRobotsBlocked`; duplicate seed deduped; resume untouched)
- [ ] `go test ./cli/ -race` — `--seeds-file` e2e via httptest: both seeds crawled in one run; arg+file → exit 2; bad line → exit 2 pre-I/O
- [ ] Single-seed regression: existing `crawl` test pass-set unchanged (robots abort exit 5 behavior pinned by an existing or new assertion)
- [ ] `go test ./vertical/ -run Catalog -race` — pagination stop rules, `--max-pages` cap, cents-vs-string price parity, `ErrNoCatalog` on 404
- [ ] `go test ./cli/ -run TestExitCode` (or equivalent table test) — `ErrNoCatalog` maps to 3
- [ ] `go run . shopify http://127.0.0.1:<httptest>` emits one JSONL record per fixture product + stderr summary; `exitCode(err)==3` on catalog-404 fixture
- [ ] `go build ./... && go vet ./... && golangci-lint run ./...` clean; `gofmt -l .` prints nothing; `go test ./...` within budget
- [ ] README documents `--seeds-file`, `magpie shopify`, and both exit-table notes

---

## Execution Prompt

Copy everything between the `---` lines into a new pi session (run from `~/projects/magpie`):

---
You are building Phase AA of magpie — multi-store crawl (`--seeds-file`) and a
Shopify catalog fast path (`magpie shopify`).

### What This Project Is
magpie (`~/projects/magpie`, module `github.com/motherlodelab/magpie`, Go 1.26+)
is a Go CLI web scraper: fetch → clean → extract. Read `AGENTS.md`,
`.pi/rules/go.md`, and `.pi/rules/testing.md` first — they are binding. Lazy
senior-dev standard: smallest diff that works, no new deps (stdlib + go.mod
only), errors = package-prefixed sentinels wrapped with `%w`, exit codes decided
ONLY in `cli.exitCode`, tests hermetic (httptest, no live network), touched
packages pass `-race`. Mark deliberate shortcuts with a `ponytail:` comment.

### Established Facts (verified against the tree — do not re-derive)
- `crawl.Options` (`crawl/crawl.go:44`) has `SeedURL string`; MCP
  (`mcp/tools.go:222`) and CLI (`cli/crawl.go:276`) both set it. Keep `SeedURL`
  untouched; add `Seeds []string`.
- `crawl/seed.go seed()`: returns early when `opts.Resume`; robots-checks the
  seed (failure = `FinishRun(runID,0,0,"robots_blocked")` + wrapped
  `ErrRobotsBlocked`); `seeds := []string{SeedURL}`; sitemap expansion via
  `ListSitemapURLs(ctx, c.static, seed)` capped `expanded[:c.maxPages]` and
  filtered by `scopeFilterExpansion(c.scope, seedURL, expanded)`; ONE
  `frontier.Add(seeds, 0)` then `c.outstanding.Add(int64(n))`.
- Resume rebuilds the frontier from the DB (`begin()` in crawl.go) — the run
  row never stores the seed, so multi-seed resume needs NO changes.
- `scope.Allows(base, cand)` is per-link-base: SameHost link-following works
  per-seed with zero scope changes.
- `cli/batch.go` reader pattern (copy its shape): `bufio.Scanner` with
  `sc.Buffer(make([]byte, 64*1024), 1024*1024)`, trim + skip blanks, `-` = stdin
  via `os.Stdin`, errors wrapped `batch: read %s: %w` (you: `crawl: read %s: %w`).
- `cli/root.go exitCode(err)` (line ~93) is the only exit-code decision point;
  precedent arms: `crawl.ErrRobotsBlocked`→5, `crawl.ErrSitemapOnlyEmpty`→3,
  `crawl.ErrBadScope`→2.
- `vertical/commerce.go`: `extractShopify` extracts the handle from any
  `/products/{handle}` path, fetches `{scheme}://{host}/products/{handle}.js`
  via `fetchJSON(ctx, f, url) (map[string]any, error)`, maps to
  `{title, vendor, price (cents/100), type, images []any (src list, never nil),
  url}` — currency is NEVER included (payload lacks it). `productMap` is the
  JSON-LD mapper (different shape; leave alone).
- PRICE GOTCHA: `/products/*.js` variant price = integer CENTS;
  `/products.json` variant price = formatted STRING major units ("24.99"),
  plus `price_min`/`price_max` strings. Catalog mapper: `strconv.ParseFloat`
  on `variants[0].price`, fallback `price_min`, omit if neither.
- `cli/crawl.go` validates `--browser`/`--lang` pre-I/O via
  `scrape.ValidateOptions(scrape.Options{Browser:…, Lang:…})`; failures are
  `scrape.OptionsError` → exit 2. `fail(code, format, args…)` is the CLI
  usage-error helper.
- Vertical package rules (`vertical/vertical.go`): leaf package, imports fetch
  types only; registry `Extractor` is one-URL→one-record — the paginator is a
  plain function, NOT a registered extractor.
- Tests: `httptest.Server` fixtures, existing fake fetchers in `crawl` tests
  are reused verbatim; golden regen only if a contract changes; suite budget
  ≤ 2 min.

### Your Goal
Implement Tasks AA.1–AA.7 from `plan/phase-AA.md` (read it first — it has the
locked design decisions D1–D10 and the full task list). TDD: each task's test
file first, watch it fail, then implement. Do not touch MCP; do not change
single-seed robots behavior; do not add dependencies or flags beyond the plan.

### Data Model Rules
- `crawl.Options.Seeds []string` — empty = legacy single-seed.
- `vertical.CatalogOptions{Limit, MaxPages int}`; CLI applies defaults
  (250 / 50) and rejects limit > 250.
- `vertical.Catalog(ctx, f, store *url.URL, opts, emit func(map[string]any) error) (int, error)`
  — streaming emit; pages-count return.
- Records stay `map[string]any` with the exact `extractShopify` keys; factor
  the shared mapper `shopifyProductRecord(m, scheme, host, handle)` so `.js`
  and `/products.json` paths cannot drift.

### Per-File Guidance
- `crawl/seed.go` — loop over a `seedList()` helper (Seeds if non-empty else
  [SeedURL]); single-seed robots failure keeps today's abort verbatim;
  multi-seed robots failure = stderr warn + skip (all skipped → finish run +
  wrapped `ErrRobotsBlocked`); sitemap expansion per seed with that seed as
  scope base; one `frontier.Add` for everything; SitemapOnly-empty fires only
  when the union across seeds is empty.
- `cli/crawl.go` — `--seeds-file` flag; Args: 0 args allowed iff flag set and
  no `--status`; arg+flag → fail(2); `readSeedsFile` mirrors batch's scanner;
  every line must parse as absolute http(s) or fail(2) with the line number;
  pass `Seeds:` into options.
- `vertical/shopify_catalog.go` — new file; paginator stops on short/empty page
  or MaxPages; page-1 404/non-JSON → wrap `ErrNoCatalog`; transport errors
  surface as-is.
- `cli/shopify.go` — new file; keyless command (no schema/provider); fetcher =
  `fetch.NewStaticFetcher()` reused across pages, per-request
  `fetch.FetchRequest{URL, Browser, Lang}`; JSONL
  streaming writer (bufio + json.Encoder per record), `--format json` buffers;
  stderr summary `shopify: N products, P pages, store HOST`; static fetch only.
- `cli/root.go` — one arm: `vertical.ErrNoCatalog` → 3.
- `README.md` — crawl `--seeds-file` row + example + global max-pages note;
  `magpie shopify` section; exit-table: robots multi-seed skip note; 3 covers
  catalog-unavailable.

### Success Criteria
1. `go test ./crawl/ ./vertical/ ./cli/ -race` green with the new test files
   listed in the plan's Deliverables.
2. Single-seed crawl behavior unchanged (existing crawl/cli tests untouched
   and green).
3. `go build ./... && go vet ./... && golangci-lint run ./...` clean;
   `gofmt -l .` prints nothing.
4. Manual smoke (optional, needs network): a public Shopify store emits
   records via `magpie shopify https://<store>`; skip if no store reachable.

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available — crawl engine (`seed()`/frontier/resume), per-host HostLimiters, `scopeFilterExpansion` (already seed-parameterized), `vertical` registry + `fetchJSON`, `cli/batch.go` reader, `exitCode` arm block, and the `FetchRequest{Browser,Lang}` per-request fields were all verified in-tree before writing
- [PASS] Every sub-task has a clear, testable completion condition — each task is TDD with named test files, a RED gate, and a one-line sanity check
- [PASS] Execution prompt is self-contained — includes (a) verified prior-phase facts with file:line anchors, (b) confirmed API snippets (`fetchJSON` signature, batch scanner pattern, `exitCode` precedent arms, cents-vs-string price gotcha, `NewStaticFetcher()` constructor), (c) a Data Model Rules section, (d) per-file guidance, and (e) observable success criteria
- [PASS] Exit criteria map 1:1 to deliverables — every deliverable file (crawl seeds loop+tests, CLI flag+tests, catalog+tests, shopify cmd+tests, root.go arm, README) has a matching criterion; nothing untested
- [PASS] Heavy external dependency strategy — no new dependencies at all; all tests run against `httptest.Server` fixtures (reusing crawl's existing fake fetchers); the manual live-store smoke is explicitly optional
- [PASS] New libraries — none introduced (stdlib `strconv`/`bufio`/`encoding/json` only), so no snippet research needed