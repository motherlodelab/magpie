# Phase W — Per-site verticals from webclaw's proven list: amazon, ebay, etsy, woocommerce, substack, dev_to

**Duration:** 2 days (~15h)
**Depends on:** master @ 8567ea6 (registry at 21; `vertical/jsonld.go` `blocksOfTypes`/`firstTypedBlock` from Phase V; `-tags live` quarantine pattern in `vertical/live_test.go`; CI green). Independent of Phase R/K desktop work — additive-only, public-repo-only.
**Blocks:** Nothing hard. Closes the remaining actionable half of competitive-analysis-2026-09-20 **P1-7** ("still missing amazon/ebay/etsy/woocommerce/instagram/linkedin/substack/dev_to") — instagram/linkedin stay deferred (login-walled, ToS-gray, violates zero-key positioning). Feeds the GUI demo breadth (paste-a-URL works on the highest-traffic commerce sites).
**Risk Level:** MEDIUM — additive files against proven patterns (LOW by itself), but amazon/ebay/etsy DOM shapes are hostile, churny, and only fully validated at the live-capture task (W.7); a wrong guess costs a missed field, not a broken build or data loss. 6 sections (no §2 failure section).
**Stack:** go
**Runner:** none — `run-phase` hard-blocks on `stack: go` (AGENTS.md); execute manually via the execution prompt in §6.

**Scope decision (supersedes Phase V):** phase-V.md §1 deferred per-site verticals except `upwork_job`, parking "amazon_product" in the desktop repo. The user re-opened P1-7 (2026-09-23): these six ship in the public core now. Consequence for desktop: a proprietary amazon extractor there becomes redundant after this lands — note it in CORE-HANDOFF at merge time (desktop owner's call, not ours).
**License hygiene (carry forward from Phase V): never copy webclaw's testdata fixtures (AGPL; this repo is MIT) — hand-write or capture our own.**

---

## 1. Objective + What Success Looks Like

Add six zero-LLM, zero-key extractors, taking the registry **from 21 to 27**: `amazon_product`, `ebay_item`, `etsy_listing` (host+path commerce pages), `woocommerce_product` (platform-shape, OptIn like `shopify_product`), `substack_post` (public per-publication JSON API), `dev_to_article` (server-rendered DOM). JSON-LD where the platform emits it (`blocksOfTypes`/`firstTypedBlock`, Phase V), DOM/API where it doesn't. Auto-dispatch for host-specific matches; OptIn for the permissive shape match — same rule the existing 21 follow.

1. `magpie vertical --list` shows **27** extractors; the six new names render usable `Label`/`Desc`/`Patterns` (MCP `list_extractors` inherits).
2. `go test ./vertical/ -v` green, **no network**: each extractor has ≥4 tests against committed `testdata/vertical/` fixtures via `fakeVerticalFetcher`.
3. Dispatch contract pinned: `MatchURL` auto-fires amazon/ebay/etsy/substack/dev_to on their host+path shapes, **never** fires `woocommerce_product` (OptIn); `--name woocommerce_product` forces it.
4. `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` all green; `git diff go.mod` empty; no edits outside `vertical/`, `testdata/vertical/`, the three registry-count pins, README, and spec §10.5.
5. `-tags live` suite green against committed `*-live` captures (loose shape asserts, recapture-churn tolerant) — one per extractor.

**Good:** "`go test ./vertical/ -run AmazonProduct -v` passes 4 tests against `testdata/vertical/amazon-product.html`, and `magpie vertical https://www.amazon.com/dp/B08N5WRWNW` returns a price record"
**Bad:** "Amazon scraping works"

## 2. Key Design Decisions

```
vertical/
├── vertical.go      (unchanged helpers; no new shared helpers except hostSuffix below)
├── jsonld.go        (unchanged: blocksOfTypes + firstTypedBlock — reused by ebay/etsy/woo)
├── amazon.go        (NEW: amazon_product — DOM-first, hostile markup)
├── ebay.go          (NEW: ebay_item — JSON-LD Product first, DOM fallback)
├── etsy.go          (NEW: etsy_listing — JSON-LD Product first, DOM fallback)
├── woocommerce.go   (NEW: woocommerce_product — JSON-LD first, .summary DOM fallback, OptIn)
├── substack.go      (NEW: substack_post — API rewrite /p/{slug} → /api/v1/posts/{slug}; hostSuffix helper lives here)
└── devto.go         (NEW: dev_to_article — DOM-only, crayons markup)
```

### Data Model Rules (Go — follow exactly)

- **Records are `map[string]any`**, flat fixed keys per the contracts table below (established convention; omission over guessing — missing source field ⇒ omit the key, never empty string/zero).
- **Self-registration:** one `register(Extractor{...})` in `init()` per file — Match + Extract + Info with honest `Patterns` (human-readable examples) and a one-sentence `Desc` that states auto-vs-OptIn (precedent: `og`'s Desc).
- **Dispatch rule (matches the existing 21):** host-specific matches ⇒ **auto**; permissive URL-shape guesses ⇒ **OptIn**. `shopify_product` (any `/products/` path, OptIn) is the direct precedent for `woocommerce_product` (`/product/` — Woo's default permalink, no collision with Shopify's plural).
- **IDs come from the URL, not the DOM:** asin/item_id/listing_id from `pathSegs` — always present, never wrong, one less brittle selector.
- **Reuse, never duplicate:** `str`, `num`, `child`, `anyMap`, `hostIs`, `pathSegs`, `fetchBytes`, `fetchJSON` (vertical.go); `stripTags` (jobs.go — the V-era tag-strip/whitespace-collapse helper); `blocksOfTypes`/`firstTypedBlock` (jsonld.go). One new helper: `hostSuffix(u, "substack.com")` in substack.go (single caller — rule of three; promote to vertical.go if a second caller appears).
- **Test style:** `package vertical_test`, `fakeVerticalFetcher{bodies: ...}` + `verticalFixture(t, ...)` + `mustURL`; floats with `1e-9` tolerance; live captures under `-tags live` with loose shape asserts only (clone `live_test.go`).
- **Error contract:** unparseable source ⇒ `vertical: <name>: no <thing> at <url>` — never an empty record (existing convention).

### Normalization contracts (fixed key sets)

| Extractor | Match (auto/OptIn) | Source strategy | Record keys |
| :-- | :-- | :-- | :-- |
| `amazon_product` | hostIs amazon.com/www + `/dp/{asin}` or `/gp/product/{asin}/{slug}` — **auto** | **DOM-first** (Amazon's JSON-LD is thin/inconsistent): `#productTitle`, `.a-price .a-offscreen`, `.basisPrice .a-offscreen` (list price), `#acrCustomerReviewText` (reviews), `span[data-hook=rating-out-of-text]` or `#acrPopover` title (rating), `#feature-bullets li span` (features), `#availability span` | title, asin, price {amount, currency}, list_price, rating, reviews, availability, features []string, url |
| `ebay_item` | hostIs ebay.com/www + `/itm/{id}` — **auto** | **JSON-LD first** (`Product` via `firstTypedBlock`), DOM fallback: `#itemTitle`, `.x-price-primary`, `.x-item-details` condition, seller `.x-sellercard-atf__info__about-seller` | title, item_id, price {amount, currency}, condition, seller, availability, url |
| `etsy_listing` | hostIs etsy.com/www + `/listing/{id}` — **auto** | **JSON-LD first** (Etsy ships solid schema.org Product), DOM fallback: `h1[data-test-id=listing-page-title]` (or first h1), `meta[property=og:price:amount]` + `og:price:currency` | title, listing_id, price {amount, currency}, seller, rating, reviews, availability, url |
| `woocommerce_product` | path contains `/product/` — **OptIn** (shopify precedent) | **JSON-LD first** (WC_Structured_Data emits `application/ld+json` Product — confirmed), DOM fallback: `.product_title`/`.entry-title`, `.price ins .woocommerce-Price-amount` (sale) or `.price .woocommerce-Price-amount`, `.sku`, `.stock`/`p.stock` | title, price {amount, currency}, sku, brand, stock_status, rating, reviews, images []string, url |
| `substack_post` | `hostSuffix *.substack.com` + `/p/{slug}` — **auto**; custom-domain substacks unreachable by Match (ceiling) — `--name substack_post` forces Extract, which rewrites *any* host's `/p/{slug}` to `{scheme}://{host}/api/v1/posts/{slug}` (same-origin public endpoint, no key) | **API-only** (`fetchJSON`): `title`, `subtitle`, `post_date`, `like_count`, `comment_count`, `cover_image`, `canonical_url`; author from `bylines`/`author` (whichever decodes — verify against the captured fixture); `text` = `body_html` via the existing `stripTags` helper (jobs.go — same package, reuse, don't duplicate) | title, subtitle, author, published, likes, comments, cover_image, text, url |
| `dev_to_article` | hostIs dev.to + exactly 2 path segs, seg[0] ≠ `t` (tag listings `/t/{tag}` are also 2-seg — excluded, pinned by a match test) — **auto** | **DOM-only** (server-rendered crayons markup; JSON-LD presence uncertain — don't rely on it): `h1.crayons-title`, `.crayons-article__header__author` / profile link, `time[datetime]`, `a.crayons-tag` / `#hashtags a` (tags), reactions from `.crayons-reaction__count--reaction` or its aria-label, comments from `#comments` header count, description from first `<p>` | title, author, published, tags []string, reactions, comments, description, url |

- **Fixture-provisional selectors:** amazon/ebay/etsy/etsy-DOM/woo-DOM selectors are written from known markup, then **finalized against the W.7 live capture** — the hand fixture pins the contract; the live fixture validates reality. If a selector is wrong at capture time, fix selector + hand fixture together (they're one unit).
- **Amazon regional domains** (amazon.de/co.uk/…): out of scope, `ponytail:` ceiling noted in amazon.go — host list grows when repricing needs it (same for ebay TLDs).

## 3. Tasks

Do JSON-LD clones first (warm-up), API, then DOM-heavy.

### Task W.1 — `etsy_listing` (2h)

`vertical/etsy.go`: init-registered **auto** extractor per §2 contract. `firstTypedBlock(body, "Product")` → normalize (offers scalar-or-array — clone `productMap`'s offers handling); DOM fallback only when the block is absent. listing_id from `pathSegs`. Fixtures: hand-written `etsy-listing.html` (JSON-LD variant), `etsy-listing-dom.html` (no JSON-LD → fallback path). Tests: match pos/neg (etsy.com yes, ebay.com no, /shop/ path no), full-record assert on both fixtures, no-source error. Add `etsy_listing` to `TestList_ExactNameSet` (`vertical/vertical_test.go` — one line per task, keeps the suite green until W.7 bumps the count pins).
**Sanity check:** `go test ./vertical/ -run EtsyListing -v` — 4+ green.

### Task W.2 — `woocommerce_product` (2h)

`vertical/woocommerce.go`: **OptIn** (`OptIn: true`; Desc says "Explicit --name only", og-style). JSON-LD first (`Product`; WC emits it), `.summary` DOM fallback (sale-price-over-regular, sku, stock). Match test pins `MatchURL` does **not** fire it (OptIn contract). Fixtures: `woo-product.html` (JSON-LD + woo classes), `woo-product-dom.html`. Tests: match pos/neg (`/product/{slug}` yes, `/products/{handle}` no — that's shopify's), full-record both fixtures, `--name` dispatch smoke via `vertical.Lookup("woocommerce_product")` Extract on a non-matching host, no-source error. Add name to ExactNameSet.
**Sanity check:** `go test ./vertical/ -run WooCommerce -v` — 4+ green.

### Task W.3 — `substack_post` (1.5h)

`vertical/substack.go`: **auto** on `*.substack.com/p/{slug}`; new tiny `hostSuffix` helper (this file). Extract = path rewrite to `/api/v1/posts/{slug}` + `fetchJSON` (hackernews/Algolia precedent) + normalize per §2 contract (`text` via one regex tag-strip, `ponytail:` comment). Non-`/p/` paths (e.g. `/about`, `/archive`) don't match. Fixtures: `substack-post.json` (trimmed captured API response shape, hand-written) + `substack-post-live.json` (W.7). Tests: match pos/neg (sub.domain.substack.com yes, notsubstack.com no, /about no), full-record assert, API-error path (500 body ⇒ typed error), custom-domain rewrite unit (Extract on `https://custom.blog/p/x` fetches `https://custom.blog/api/v1/posts/x` — assert via fake fetcher's request map). Add name to ExactNameSet.
**Sanity check:** `go test ./vertical/ -run Substack -v` — 4+ green.

### Task W.4 — `dev_to_article` (2h)

`vertical/devto.go`: **auto**, match = dev.to + `len(pathSegs)==2` + seg[0]≠`t`. DOM-only per §2 contract. Fixtures: `dev-to-article.html` (crayons markup, hand-written). Tests: match pos/neg (`/user/slug` yes, `/t/golang` no, `/user/slug/comments` no), full-record assert (incl. tags array + numeric reactions/comments), missing-page error (404-ish empty body ⇒ typed error, not empty record). Add name to ExactNameSet.
**Sanity check:** `go test ./vertical/ -run DevTo -v` — 4+ green.

### Task W.5 — `ebay_item` (2.5h)

`vertical/ebay.go`: **auto** on `/itm/{id}`. JSON-LD first, DOM fallback per §2. item_id from path (strip non-digits — `/itm/123456?v=…` shapes). Fixtures: `ebay-item.html` (JSON-LD), `ebay-item-dom.html`. Tests: match pos/neg, full-record both, query-string id extraction, no-source error. Add name to ExactNameSet.
**Sanity check:** `go test ./vertical/ -run EbayItem -v` — 4+ green.

### Task W.6 — `amazon_product` (3h — DOM-heaviest)

`vertical/amazon.go`: **auto** on `/dp/{asin}` and `/gp/product/{asin}/…`. DOM-first per §2 (selectors from known markup; price may be sale `.a-price` + list `.basisPrice` — list_price omitted when absent). Regional-TLD `ponytail:` ceiling comment. Fixtures: `amazon-product.html` (hand-written from known markup), `amazon-product-dom.html` variant if capture shows a second common shape. Tests: match pos/neg (`/dp/B08…` yes, `/s?k=query` no, amazon.de no — pinned ceiling), full-record assert, minimal-page error (challenge-stub body ⇒ typed error). Add name to ExactNameSet.
**Sanity check:** `go test ./vertical/ -run AmazonProduct -v` — 4+ green.

### Task W.7 — Live capture + count pins + docs (2h)

**Depends on:** W.1–W.6

- Dogfood capture (manual, live network, NOT tests): `magpie scrape <url> --page-format raw --out testdata/vertical/<name>-live.html` — one real page per vertical: an amazon `/dp/` item, an ebay `/itm/` item, an etsy `/listing/`, a Woo product (pick one from a WooCommerce plugin demo store), a substack post (**capture the API JSON**: `curl https://{pub}.substack.com/api/v1/posts/{slug} -o testdata/vertical/substack-post-live.json`), a dev.to article. Blocked by a challenge? Retry with `--browser random` (WSL: `MAGPIE_CHROME_NO_SANDBOX=1`). Worst case: ship hand-written fixtures only, leave the live test out for that extractor, and say so in the PR — do not weaken the contract to make a capture pass.
- Add six `TestLive*` funcs to `vertical/live_test.go` (clone pattern: fake fetcher serves committed bytes; loose asserts — non-empty title at minimum).
- **Finalize selectors:** run each extractor against its live fixture; where a selector misses, fix selector + hand fixture together (one unit), re-run hand-fixture tests.
- Count pins to 27: `TestList_ExactNameSet` final message, `mcp/agent_test.go:506` (`!= 21` → `!= 27`, message "+ 6 Phase W"), `cli/agent_test.go:241` (same).
- Docs: README L290 count line → 27; README L301 built-in extractor list += the six names; spec.md §10.5 Vertical-breadth bullet → 27 (prose: host-specific auto verticals now include the top commerce marketplaces; `woocommerce_product` OptIn via `/product/` shape) — and **strike the bullet's closing sentence** ("Proprietary per-site verticals (e.g. a desktop-only `amazon_product`) are not built here… `vertical.Register`"): the registry now ships amazon_product, the spec must not contradict itself.

**Sanity check:** `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` all green; `go test -tags live ./vertical/ -run Live -v` green (network-free — serves committed bytes); `magpie vertical --list` → 27.

## 4. Deliverables

```
magpie/
├── vertical/
│   ├── etsy.go / etsy_test.go               # etsy_listing (JSON-LD first, DOM fallback)
│   ├── woocommerce.go / woocommerce_test.go # woocommerce_product (JSON-LD first, OptIn)
│   ├── substack.go / substack_test.go       # substack_post (API rewrite; hostSuffix helper)
│   ├── devto.go / devto_test.go             # dev_to_article (DOM-only)
│   ├── ebay.go / ebay_test.go               # ebay_item (JSON-LD first, DOM fallback)
│   ├── amazon.go / amazon_test.go           # amazon_product (DOM-first)
│   ├── live_test.go                         # +6 TestLive* funcs
│   └── vertical_test.go                     # TestList_ExactNameSet: 21 → 27 names
├── testdata/vertical/
│   ├── etsy-listing.html, etsy-listing-dom.html, etsy-listing-live.html
│   ├── woo-product.html, woo-product-dom.html, woo-product-live.html
│   ├── substack-post.json, substack-post-live.json
│   ├── dev-to-article.html, dev-to-article-live.html
│   ├── ebay-item.html, ebay-item-dom.html, ebay-item-live.html
│   └── amazon-product.html, amazon-product-live.html
├── mcp/agent_test.go                        # count pin 21 → 27 (L506)
├── cli/agent_test.go                        # count pin 21 → 27 (L241)
├── README.md                                # count line + built-in list
└── spec.md                                  # §10.5 breadth bullet
```

## 5. Exit Criteria

- [ ] `magpie vertical --list` shows 27; six new `desc` strings render (mcp `list_extractors` inherits; mcp/ + cli/ diffs are exactly the two count pins)
- [ ] `go test ./vertical/ -v` green, zero network: each new extractor has match pos/neg, full-record fixture assert, variant/no-source error test; `TestList_ExactNameSet` carries the 27-name set with Label/Desc/Patterns non-empty
- [ ] Dispatch pinned: `MatchURL` fires amazon/ebay/etsy/substack/dev_to on their shapes and never fires `woocommerce_product`; woo works via `--name` (Lookup+Extract smoke test)
- [ ] `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` green; `git diff go.mod` empty; no edits outside §4 tree
- [ ] `-tags live` suite green on committed captures (or per-extractor live test absent with an explicit PR note); selector fixes landed as selector+hand-fixture pairs
- [ ] Omission-over-guessing spot-checks: no price ⇒ no `price` key (etsy/woo fixtures), missing list_price ⇒ key absent (amazon fixture)

## 6. Execution Prompt

Copy everything between the `---` lines into a new pi session to implement this phase:

---

You are building Phase W of **magpie** — a Go CLI web scraper (fetch → clean → extract), module `github.com/motherlodelab/magpie`, binary `magpie`. Pure Go, **zero CGO, no new Go module dependencies without asking**. Default test suite is **hermetic: no network**. Coding ethos: lazy senior dev — minimum code, reuse helpers, `map[string]any` records, omission over guessing. `stack: go` means `run-phase` hard-blocks — implement manually, test-first per task, in task order W.1→W.7 (plan/phase-W.md is the source of truth; its §2 contracts table is normative).

### Established in prior phases (facts, not suggestions)

- `vertical/vertical.go`: `Extractor{Info{Name,Label,Desc,Patterns}, Match, Extract, OptIn}`, `register()` (init-time), `Register()` (public seam), `Lookup`, `MatchURL` (skips OptIn). Helpers: `str`, `num`, `child`, `anyMap`, `hostIs`, `pathSegs`, `lastSegment`, `fetchBytes`, `fetchJSON`, `firstJSONArray`, plus `stripTags` (jobs.go) for HTML→text.
- `vertical/jsonld.go`: `blocksOfTypes(html []byte, types ...string) []json.RawMessage` and `firstTypedBlock(html []byte, types ...string) (map[string]any, bool)` — Phase V's generalized JSON-LD scan. Reuse, don't copy.
- Dispatch rule across the existing 21: host-specific match ⇒ auto; permissive URL-shape ⇒ OptIn (`shopify_product` matches any `/products/` path and is OptIn: true). Error contract: `vertical: <name>: no <thing> at <url>` — never an empty record. Records are flat `map[string]any`; missing field ⇒ omit key.
- Tests: `package vertical_test`, `fakeVerticalFetcher{bodies: map[string]fakeResp{"host": {body: ...}}}`, `verticalFixture(t, name)`, `mustURL(t, s)`; floats `1e-9`. Live captures: committed `testdata/vertical/*-live.*`, exercised only under `//go:build live` (`vertical/live_test.go`) with loose shape asserts.
- Registry is at **21**. `TestList_ExactNameSet` (`vertical/vertical_test.go` ~L91) pins exact names — add each new name as you land its extractor, finalize count message in W.7. Count pins: `mcp/agent_test.go:506` and `cli/agent_test.go:241` (`!= 21`) — bump to 27 in W.7 only.
- Registration snippet (vertical/commerce.go pattern):
```go
func init() { register(Extractor{Info: Info{Name: "...", Label: "...", Desc: "...",
    Patterns: []string{"https://{example}"}}, Match: matchX, Extract: extractX}) } // + OptIn: true where permissive
```
- Confirmed external facts (do not re-research): WooCommerce emits `application/ld+json` schema.org Product on product pages (WC_Structured_Data); Substack publications expose `https://{host}/api/v1/posts/{slug}` publicly (no key, same-origin — works on custom domains too); Etsy product listings ship schema.org JSON-LD; Amazon's JSON-LD is thin/inconsistent → DOM-first; dev.to is server-rendered crayons markup (crayons-title, crayons-tag, crayons-reaction__count classes).

### Your Goal

Registry 21 → 27: `etsy_listing`, `woocommerce_product`, `substack_post`, `dev_to_article`, `ebay_item`, `amazon_product` — one file + one test file each, per the §2 contracts table in plan/phase-W.md (copy it into your session).

### Data Model Rules (follow exactly)

- Flat `map[string]any`, fixed keys per contract; omit absent fields; nested price as `{amount float, currency string}` (jobs.salary precedent). IDs (asin/item_id/listing_id) parsed from the URL path, never the DOM.
- One `init()` register per file. `woocommerce_product` is `OptIn: true`; the other five auto. Desc strings state auto-vs-explicit (og's Desc is the model).
- New helper only where needed once: `hostSuffix(u *url.URL, suffix string) bool` lives in substack.go (promote to vertical.go only on a second caller).
- Normalize JSON-LD scalar-or-array shapes (clone `productMap` offers handling in commerce.go). `text` uses the existing `stripTags` (jobs.go) — no new regex.

### Per-file guidance (details + fixtures in plan/phase-W.md §2–3)

- **etsy.go** — auto on etsy.com `/listing/{id}`; `firstTypedBlock(body,"Product")` → record, DOM fallback (h1 + og:price metas). Fixtures: JSON-LD + DOM-variant.
- **woocommerce.go** — OptIn, match = path contains `/product/` (never `/products/` — shopify's); JSON-LD first, `.summary` fallback (sale price, sku, stock). Match test pins MatchURL-does-NOT-fire.
- **substack.go** — auto on `*.substack.com/p/{slug}`; Extract rewrites to `/api/v1/posts/{slug}` + `fetchJSON`; `text` from `body_html` stripped; custom-domain rewrite unit test via fake fetcher request map.
- **devto.go** — auto on dev.to 2-seg paths, seg[0]≠`t`; DOM-only crayons selectors (title/author/time[datetime]/tags/reactions/comments/first-p description).
- **ebay.go** — auto on ebay.com `/itm/{id}` (id digits via pathSegs, strip query); JSON-LD first, DOM fallback (`#itemTitle`, `.x-price-primary`, condition, seller).
- **amazon.go** — auto on `/dp/{asin}` + `/gp/product/{asin}/…`; DOM-first (`#productTitle`, `.a-price .a-offscreen`, `.basisPrice`, `#acrCustomerReviewText`, rating span, `#feature-bullets`, `#availability`); regional-TLD ceiling comment.
- **testdata/vertical/** — hand-written fixtures per §3 task names; live captures in W.7 (`*-live.*`; substack via curl of the API JSON). **Never copy webclaw fixtures (AGPL vs MIT).**
- **W.7** — six `TestLive*` funcs in live_test.go; finalize selectors against live captures (selector+hand-fixture fixed together); bump `TestList_ExactNameSet` message + `mcp/agent_test.go:506` + `cli/agent_test.go:241` to 27; README count line (~L290) + built-in list (~L301); spec.md §10.5 bullet → 27.

### Hard rules

- No new dependencies. No CGO. No network in default tests. No edits outside vertical/, testdata/vertical/, the two count pins, README, spec.md. Challenge-walled captures: retry with `--browser random`; if capture still fails, ship hand fixtures + say so in the PR — never weaken the extractor to pass a capture.

### Success criteria

1. `magpie vertical --list` → 27.
2. `go test ./vertical/ -v` green: ≥24 new tests across 6 pairs; OptIn pin green; ExactNameSet = 27 names.
3. `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` green; `git diff go.mod` empty; mcp/ + cli/ diffs are exactly the two count bumps.
4. Record keys match the §2 contracts; omission spot-checks pass.

### Expected file structure at end

See §4 Deliverables in plan/phase-W.md — same tree, no extras.

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available (helper inventory, blocksOfTypes/firstTypedBlock, fake fetcher, live-tag pattern, count-pin line numbers — all read from source @ 8567ea6 today)
- [PASS] Every sub-task has a clear, testable completion condition (per-task sanity checks + §5)
- [PASS] Execution prompt is self-contained: (a) prior-phase facts inline, (b) confirmed snippets (register, external API/schema facts), (c) data-model rules, (d) per-file guidance + contracts-table reference, (e) observable success criteria
- [PASS] Exit criteria map 1:1 to deliverables (each extractor has a test matrix; docs/counts have list checks; live captures have the tag-quarantined suite)
- [PASS] Heavy external dependency has a fake/stub strategy (no external deps; hand fixtures hermetic; hostile-site captures quarantined behind `-tags live` with an explicit ship-without-capture fallback)
- [PASS] New libraries have a confirmed usage snippet (none added; Substack endpoint + Woo JSON-LD emission confirmed via research 2026-09-23; Etsy/Amazon DOM shapes validated empirically in W.7 — the one open risk, folded into the MEDIUM rating and W.7 fallback)
