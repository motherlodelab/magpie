# Phase W — Testing: Per-site verticals wave 2 (amazon, ebay, etsy, woocommerce, substack, dev_to)

**Scope:** the six new extractors `vertical/{etsy,woocommerce,substack,devto,ebay,amazon}.go` (W.1–W.6), registry-count edits (`vertical/vertical_test.go` `TestList_ExactNameSet` :91, `mcp/agent_test.go:506`, `cli/agent_test.go:241`), the six new `TestLive*` funcs in `vertical/live_test.go` (W.7), README/spec grep gates.
**Key Pattern:** **No new fakes** (V precedent). Every test drives a `vertical.Lookup`'d extractor through the existing `fakeVerticalFetcher` against committed fixtures — hand-written fixtures are the **exact-mapping oracle** (fixed key sets, omission pins), captured real pages only prove reality behind `//go:build live` with loose shape asserts. New in W vs V: **DOM-first extraction** (goquery, precedent in og/social/trustpilot/upwork) and **one API-URL rewrite** (substack — proven via the fake's `order` recording, never by trusting the record). The price-string parser is tested **behaviorally through `Extract`** (external test package boundary), never by calling the unexported helper.
**Dependencies:** stdlib `testing`, `context`, `math`, `reflect`, `strings`, `net/url` + in-repo helpers only: `fakeVerticalFetcher`/`fakeResp` (vertical/vertical_test.go:20/26 — **records fetch order in `.order`**), `verticalFixture` (:73), `mustURL` (:82), `TestList_ExactNameSet` (:91), `TestMatchURL_StrictOnly` (:138), `TestOptInNeverSteals` (:173), `stripTags` (vertical/jobs.go:34, production reuse per phase-W.md). goquery already a dep (5 production files use it). **No new test deps; `git diff go.mod` empty.**

**Decisions pinned while verifying seams** (test-plan additions to phase-W.md, discovered by reading source):
1. **A price-string parser is a de-facto second new helper.** phase-W.md names only `hostSuffix` as new, but amazon/ebay/woo DOM paths read prices as text ("US $89.99"). Pinned here: **en-US dot-decimal format only** — strip thousands commas, float from the number, currency from a minimal symbol map (`$`→USD, `€`→EUR, `£`→GBP); prefix text before the symbol is ignored; **unknown symbol/code ⇒ `currency` key omitted, `amount` kept if numeric; fully unparseable ⇒ whole `price` key omitted, never zero**. Regional formats (`€12,50`) misparse by design — they ride the regional-TLD `ponytail:` ceiling phase-W.md already declares. Tested behaviorally via inline-HTML literals per extractor, not by direct call (package boundary).
2. **DOM-sourced numbers are `float64`** — same type JSON-decoded numbers produce (`numVal` precedent). rating 4.5, reviews 8234, likes/comments/reactions: all `float64` in records, asserted with `1e-9` where non-integer.
3. **Dates stay raw string passthrough** (V deviation 4 carries): substack `post_date`, dev_to `time[datetime]` attribute — no parsing, no formatting.
4. **substack's `url` key = the page URL the user gave**, never the rewritten API endpoint (`/api/v1/posts/…`). Decoy-URL discipline from V (`productMap` precedent) extended to API rewrites.
5. **ebay `condition` prefix-strip:** `offers.itemCondition` `"https://schema.org/NewCondition"` ⇒ record value `"NewCondition"` (raw URL would be leak-shaped noise; suffix is the honest enum).
6. **`amazon-product-dom.html` is conditional** — only if the W.7 capture shows a second common shape. The fixture-count gate allows exactly 15 mandatory testdata paths, 16 with it.
7. **Existing dispatch pins verified non-colliding:** `TestMatchURL_StrictOnly`/`TestOptInNeverSteals` use `example.com`/`shop.example`/`blog.example`/`github.com` URLs — none sit on the six new hosts — so they stay green **untouched** (V deviation 5 pattern).

---

## User Stories

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | As an agent/GUI user, I want `magpie vertical` on an amazon/ebay/etsy product URL to auto-fire and return a priced record with the ID taken from the URL, so commerce data is structured without LLM calls or keys | `amazon_test.go`, `ebay_test.go`, `etsy_test.go` (match tables, full-record, fallback/variant, no-source) | `MatchURL` auto-fires all three on their host+path shapes and on nothing else (`amazon.de`, `/s?k=`, `notamazon.com` all miss); `asin`/`item_id`/`listing_id` equal the URL segment; `price` = `{amount float64, currency string}` at `1e-9`; amazon sale+list price pair, list omitted when absent; ebay/etsy JSON-LD record exact AND DOM-fallback record exact; challenge-stub/no-block ⇒ typed `vertical: <name>:` error |
| US-2 | As a scrapes-anything user, I want `woocommerce_product` to work on any `/product/` permalink **only when I ask by name**, so permissive shape matches never change my default output | `woocommerce_test.go` (match table incl. plural negative, `TestWooCommerce_NeverAutoFires`, `--name` dispatch smoke, JSON-LD + DOM records, omission) | `Match` fires `/product/{slug}` and **not** `/products/{handle}` (shopify's plural); `OptIn == true`; `MatchURL("https://shop.example/product/x")` misses; `Lookup("woocommerce_product")` + `Extract` on a non-woo host still extracts from served bytes; sale-price-over-regular, `.sku`/`.stock` DOM keys exact; absent source keys absent from record |
| US-3 | As a newsletter reader/archivist, I want `substack_post` on `*.substack.com/p/{slug}` to fetch the public API and return a text-bearing record, so I get post content without HTML noise | `substack_test.go` (hostSuffix table, full record, author-shape variants, API-500 error, custom-domain rewrite) | match: `a.b.substack.com/p/x` yes, `substack.com/p/x` yes, `notsubstack.com/p/x` **no**, `/about` no; record keys exact incl. `text` = stripTags output **with its glue ceiling pinned** (`"Hello world.Second graph."`), `likes`/`comments` float64, `published` raw, `url` = page URL; `author` and `bylines` shapes both ⇒ author string; `fakeResp{status:500}` ⇒ error contains `HTTP 500` + the **API** URL; `fx.order[0]` = `https://custom.blog/api/v1/posts/x` for a custom-domain Extract |
| US-4 | As a dev-content aggregator, I want `dev_to_article` to auto-fire on article URLs but never on tag pages or comment subpaths, so crawling dev.to yields article records only | `devto_test.go` (match table, full record, no-source error) | `/user/slug` yes, `/t/golang` no, `/user/slug/comments` no, `dev.to` exact host only; record: title/author/published(raw)/tags `[]string` in order/reactions+comments float64/description = first-`<p>` text; empty-body ⇒ typed error, never an empty record |
| US-5 | As the registry contract owner, I want 27 extractors listed everywhere, five new autos dispatching, woo OptIn, and zero collateral edits, so embedders (GUI) and MCP consumers see one honest registry | `TestList_ExactNameSet` 27 names + `mcp/agent_test.go:506`/`cli/agent_test.go:241` == 27 + `TestW_AutoDispatch` + `TestWooCommerce_NeverAutoFires` + diff/status gates | `magpie vertical --list` → 27; all six carry non-empty Label/Desc/Patterns in ExactNameSet; `MatchURL` returns the right **name** for one URL per auto extractor; `git diff go.mod` empty; `git status --porcelain testdata/` shows only the ≤16 named paths; README says 27 + six names; spec §10.5 says 27 and the "desktop-only amazon_product" sentence is gone |

---

## 1. Component Mock Strategy

Phase type: **pure logic** (parse + normalize against committed bytes; zero network in every tier). Mock strategy in one sentence: **every row instantiates the real extractor via `Lookup`, feeds it `fakeVerticalFetcher{bodies: …}` loaded from `verticalFixture` or inline literals, and asserts the flat `map[string]any` — exact values on hand fixtures, key-presence/shape on variants, loose shape only behind `-tags live`; the substack rewrite is proven through the fake's `order` slice, not the record.**

| Component | Mock Strategy | What to Assert | User Story |
|-----------|--------------|----------------|------------|
| `etsy_listing` JSON-LD path | `etsy_test.go` — fake serves `etsy-listing.html` (Product block); full-record assert | title/listing_id (from path)/price{32, USD}/seller (from `offers.seller.name`)/rating 4.9/reviews 214 (aggregateRating)/availability/url = page URL | US-1 |
| `etsy_listing` DOM fallback | fake serves `etsy-listing-dom.html` (no JSON-LD) | title from `h1[data-test-id=listing-page-title]`; price from `og:price:amount`+`:currency` metas; **seller/rating/reviews/availability keys absent** (omission pin — DOM shape carries no seller) | US-1, US-2 |
| `etsy_listing` match | match table, no fetch | `etsy.com/listing/{id}` + `www.` yes; `etsy.com/shop/x` no; `ebay.com/itm/1` no; `file://` no | US-1 |
| `woocommerce_product` JSON-LD path | `woocommerce_test.go` — `woo-product.html` (WC-shaped Product block + woo classes) | title/sku/brand (object `{name}` ⇒ string)/price{24.5, USD}/availability/rating 4.8/reviews 57/images `[]string`/url = page URL | US-2 |
| `woocommerce_product` DOM fallback | fake serves `woo-product-dom.html` (no JSON-LD, `.summary` markup) | `.product_title`; **sale-over-regular**: `.price ins` 19.90 wins over `del` 24.50; `.sku` ⇒ "MWB-01"; `.stock` ⇒ stock_status "In stock"; brand/rating/reviews omitted | US-2 |
| woo OptIn + shape pin | `TestWooCommerce_Match` + `TestWooCommerce_NeverAutoFires` + `--name` smoke | `Match` yes on `/product/x`, **no** on `/products/x`; `OptIn == true`; `MatchURL("https://shop.example/product/x")` misses; `Lookup`+`Extract` succeeds on `https://anyhost.example/product/x` (OptIn ignores host) | US-2 |
| price-string parser (behavioral) | inline HTML literals per extractor subtests (amazon/ebay/woo DOM paths) — never a direct helper call | `"$1,299.00"` ⇒ 1299 USD; `"US $24.99"` ⇒ 24.99 USD (prefix ignored); `"€19.99"` ⇒ 19.99 EUR; `"CHF 49.00"` ⇒ 49 + `currency` absent; `"Currently unavailable"` ⇒ `price` key absent | US-1, US-2 |
| `substack_post` match | hostSuffix table, no fetch | `demo.substack.com/p/x` + `a.b.substack.com/p/x` + `substack.com/p/x` yes; `notsubstack.com/p/x` no; `demo.substack.com/about`, `/archive` no; `file://` no | US-3 |
| `substack_post` extract | fake serves `substack-post.json` keyed on the **rewritten** API URL substring; full record | title/subtitle/author/published raw ISO/likes 823 + comments 47 as float64/cover_image/text = stripTags(body_html) exactly `"Hello world.Second graph."` (glue ceiling pinned)/`url` = **page** URL | US-3 |
| substack rewrite proof | custom-domain test — `Lookup`+`Extract` on `https://custom.blog/p/x`, fake keyed on `custom.blog/api/v1/posts/x` | `fx.order[0]` == `"https://custom.blog/api/v1/posts/x"`; `len(fx.order)` == 1 (no stray page fetch) | US-3 |
| substack author shapes | inline JSON literals: `bylines:[{name}]` and `author:[{name}]` | both ⇒ same author string; neither ⇒ `author` key absent | US-3 |
| substack API error | `fakeResp{status: 500}` on the API key | error contains `vertical: GET https://demo.substack.com/api/v1/posts/x: HTTP 500` (fetchBytes contract :117) | US-3 |
| `dev_to_article` match | match table, no fetch | `https://dev.to/alexdev/go-generics` yes; `/t/golang` no (tag); `/alexdev/generics/comments` no (3 segs); `https://dev.to/about` no (1 seg — fails the 2-seg rule); `notdev.to/x/y` no | US-4 |
| `dev_to_article` extract | fake serves `dev-to-article.html` (crayons markup) | title (`h1.crayons-title`)/author/published = `time[datetime]` raw/tags `["go","generics","tutorial"]` in order/reactions 128 + comments 12 float64/description = first-`<p>` text/url = page URL | US-4 |
| `dev_to_article` no-source | fake serves 404-ish empty body | typed error contains `vertical: dev_to_article:` + URL — never an empty record | US-4 |
| `ebay_item` JSON-LD path | `ebay_test.go` — `ebay-item.html`; full record | title/item_id (path, digits)/price{89.99, USD}/condition `"NewCondition"` (prefix-strip, decision 5)/seller/availability/url = page URL (decoy url in block must not win) | US-1 |
| `ebay_item` DOM fallback + id variants | `ebay-item-dom.html` + `/itm/123456?nordt=true` URL | `#itemTitle`/`.x-price-primary` "US $89.99" ⇒ price parser/seller from `.x-sellercard-atf__info__about-seller`; query string never pollutes `item_id` (== 123456) | US-1 |
| `amazon_product` DOM-first | `amazon_test.go` — `amazon-product.html`; full record | `#productTitle`/price `.a-price .a-offscreen` "$289.99"/list_price `.basisPrice .a-offscreen` "$349.99"/rating "4.5 out of 5 stars" ⇒ 4.5/reviews "8,234 ratings" ⇒ 8234/availability "In Stock"/features `[]string` ×3/asin from URL — all exact | US-1 |
| amazon omission + challenge | inline literals: no `.basisPrice`; challenge stub ("Sorry! Something went wrong!") | no `list_price` key when absent; challenge body ⇒ typed `vertical: amazon_product:` error; regional-TLD match negatives (`amazon.de/dp/x` no) | US-1 |
| Dispatch pin (five autos) | `TestW_AutoDispatch` — one URL each through `MatchURL` | returns exactly `amazon_product`/`ebay_item`/`etsy_listing`/`substack_post`/`dev_to_article` by **name**; `https://shop.example/product/x` misses (woo OptIn) | US-5 |
| Registry counts (3 files) | `TestList_ExactNameSet` append 6 names (`// Phase W additions.`); `mcp/agent_test.go:506` `!= 21` → `!= 27` + message; `cli/agent_test.go:241` same | `--list`/MCP/CLI doc all report 27; ExactNameSet enforces non-empty Label/Desc/Patterns for the six | US-5 |
| Live captures | `vertical/live_test.go` +6 `TestLive*` (`//go:build live`) — committed `*-live.*` bytes via the same fake; loose asserts | `Extract` ok; primary keys non-empty (title/url; substack: title+text non-empty); zero exact-value asserts | US-1–4 (reality gate) |
| Docs gates | grep, not tests: README + spec | `grep -c "all 27 extractors" README.md` ≥ 1; README list contains the six names; spec §10.5 says 27, mentions `amazon_product`, and `grep "desktop-only .amazon_product." spec.md` is **empty** (strike sentence gone) | US-5 |

---

## 2. Test Tier Table

| Tier | Dependencies | Speed | When to Run |
|------|-------------|-------|-------------|
| Default (`go test ./...`) | `fakeVerticalFetcher` + committed hand fixtures + inline literals; pure table tests — **no network, no browser, no build tags** | <1s added (suite budget ≤2 min per testing.md holds) | Every push; the only CI gate |
| Live (`go test -tags live ./vertical/`) | Committed real-page captures (`testdata/vertical/*-live.{html,json}`) through the same fake — **still zero network**; the tag quarantines recapture churn | ~1s | Manual: after each W.7 capture, before merge |
| Manual capture (not a test file) | Built binary + real network: `magpie scrape <url> --page-format raw --out testdata/vertical/<name>-live.html`; substack via `curl …/api/v1/posts/{slug}`; `--browser random` retry on challenges (`MAGPIE_CHROME_NO_SANDBOX=1` in WSL) | minutes | W.7 capture pass ONLY — never a CI claim, never in a test |

Fixture rule: exactly **15 mandatory testdata paths** — 9 hand-written (`etsy-listing.html`, `etsy-listing-dom.html`, `woo-product.html`, `woo-product-dom.html`, `substack-post.json`, `dev-to-article.html`, `ebay-item.html`, `ebay-item-dom.html`, `amazon-product.html`) + 6 captured (`*-live.html` ×5, `substack-post-live.json`); `amazon-product-dom.html` optional 16th (decision 6). `git status --porcelain testdata/` may show ONLY those. **Never a webclaw fixture (AGPL vs MIT).**

---

## 3. No New Fakes

Every oracle exists. `fakeVerticalFetcher`/`fakeResp` (vertical/vertical_test.go:20/26 — host-substring keyed, longest-match wins, `$` anchor, `fakeResp{status, body, err}`, status 0 ⇒ 200, **`order []string` records every fetched URL**), `verticalFixture` (:73), `mustURL` (:82) are already in scope — new test files live in `package vertical_test` in the same directory. The only new in-test material:

1. **Inline HTML/JSON literals** for variant shapes (price strings, substack author shapes, amazon no-list-price) — `TestUpworkExtract_MetaOnly` precedent: byte literals, no fixture files.
2. **The `fx.order` assertion pattern** (first use of the existing recording — substack rewrite proof).
3. **Six `TestLive*` funcs** appended to the existing `-tags live` file.

No second fetcher fake, no `httptest` server (extractors never see a transport — that is the `Fetcher` seam's point), no golden-file harness.

---

## 4. Test File List

```
magpie/
├── vertical/
│   ├── etsy.go / etsy_test.go               # NEW (~4 tests): match table, JSON-LD record, DOM fallback + omission, no-source
│   ├── woocommerce.go / woocommerce_test.go # NEW (~5 tests): match table (plural negative), NeverAutoFires + --name smoke,
│   │                                        #   JSON-LD record, DOM fallback (sale-over-regular), no-source
│   ├── substack.go / substack_test.go       # NEW (~5 tests): hostSuffix table, full record, author variants + API-500,
│   │                                        #   custom-domain rewrite via fx.order
│   ├── devto.go / devto_test.go             # NEW (~4 tests): match table, full record (tags/floats), no-source
│   ├── ebay.go / ebay_test.go               # NEW (~5 tests): match table, JSON-LD record (condition strip), DOM fallback,
│   │                                        #   id-from-query-shaped-path, no-source
│   ├── amazon.go / amazon_test.go           # NEW (~5 tests): match table (amazon.de negative), full record, price-string
│   │                                        #   table + list_price omission, challenge stub error — §7 shows it in full
│   ├── live_test.go                         # APPEND: 6 TestLive* funcs (loose asserts vs committed captures)
│   └── vertical_test.go                     # APPEND: TestList_ExactNameSet + 6 names under "// Phase W additions."
├── testdata/vertical/
│   ├── etsy-listing.html, etsy-listing-dom.html,
│   │   woo-product.html, woo-product-dom.html, substack-post.json,
│   │   dev-to-article.html, ebay-item.html, ebay-item-dom.html,
│   │   amazon-product.html                  # NEW hand-written exact-oracle fixtures (specs in §8)
│   │   amazon-product-dom.html              # OPTIONAL 16th (only if W.7 shows a second shape)
│   └── etsy-listing-live.html, woo-product-live.html, substack-post-live.json,
│       dev-to-article-live.html, ebay-item-live.html, amazon-product-live.html  # W.7 captures, loose oracle
├── mcp/agent_test.go                        # EDIT ONLY ~:506: len(exs) != 21 → 27 (+ message "…+ 6 Phase W")
├── cli/agent_test.go                        # EDIT ONLY ~:241: len(doc.Extractors) != 21 → 27 (+ message)
├── README.md                                # DELIVERABLE (docs): count line 21→27 + six names in built-in list
└── spec.md                                  # DELIVERABLE (docs): §10.5 bullet → 27 + strike the desktop-only
                                            #   amazon_product sentence (validation fix)
```

Existing tests that must stay green **untouched**: `TestMatchURL_StrictOnly` (:138), `TestOptInNeverSteals` (:173), all of `commerce_test.go`/`og_test.go`/V-wave suites, `TestLookup_All`/`TestRegister` (dynamic), `TestToolCatalog` (12 tools — no MCP surface change), everything under `-tags browser`.

---

## 5. Test Helper Structure (Go — no conftest.py)

| Helper | Home | Used for | New? |
|--------|------|----------|------|
| `fakeVerticalFetcher{bodies: map[string]fakeResp{…}}` | vertical/vertical_test.go:20 | serve fixture bytes per URL substring; `fakeResp{status, body, err}` — status 500 for substack API error | reuse |
| `fx.order` | same (:27, `Fetch` appends `req.URL`) | **prove the substack rewrite**: assert exact API URL fetched, count == 1 | first use (existing field) |
| `verticalFixture(t, name)` | :73 | load `testdata/vertical/<name>` (works for `.json` too — bytes are bytes) | reuse |
| `mustURL(t, s)` | :82 | parse match/extract URLs | reuse |
| `vertical.Lookup(name)` / `vertical.MatchURL(raw)` | vertical package | the only entry points tests need | reuse |
| inline HTML/JSON literals | each new test file | variant shapes (price strings, author shapes, omission bodies) | new consts, in-test |
| `//go:build live` harness | live_test.go (exists) | append 6 loose shape tests | append |
| `1e-9` float tolerance | commerce_test.go:43 precedent | price/rating asserts (28.99 not binary-exact) | reuse pattern |
| `t.Context()` / `t.Parallel()` | newer vertical tests | house style | reuse |

Fixture-vs-literal split (V rule carries): committed fixture = the one full-record exact assert per extractor per path (JSON-LD and DOM variants); literals = cheap shape variants (price strings, author shapes, omission bodies, challenge stubs). New tests take `t.Parallel()` (pure, per testing.md).

---

## 6. Key Testing Decisions

| Decision | Approach | Rationale |
|----------|----------|-----------|
| Hand fixture = exact oracle; live capture = reality proof | full-record asserts ONLY on hand fixtures; captures get key-presence asserts behind `-tags live` | Hand fixtures pin the contract and diff cleanly; real marketplace pages drift per recapture and get A/B-instrumented — exact asserts there would make every recapture a test-edit session. The tag quarantines exactly that churn |
| Price parser tested behaviorally, not directly | inline HTML literals through `Extract`; no test calls the unexported parser | Tests are `package vertical_test` — the boundary is the point: the contract is the record, not the helper. Direct access would also invite testing the en-US ceiling as if it were a spec |
| en-US price format is a pinned ceiling | comma-thousands stripped only in dot-decimal shapes; `"€12,50"` would misparse and is NOT a test case | Regional formats ride phase-W.md's declared regional-TLD `ponytail:` ceiling — amazon.de/ebay.de are out of scope, so their formats are too. Testing the misparse would pin a bug as a feature |
| Unknown currency ⇒ omit, unparseable ⇒ omit whole key | `"CHF 49.00"` ⇒ amount kept, `currency` absent; `"Currently unavailable"` ⇒ no `price` key | Omission-over-guessing at a new trust boundary (free text). A zero amount or `"???"` currency would poison LLM consumers silently |
| IDs from URL, asserted against adversarial URLs | asin/item_id/listing_id equal the path segment; ebay test uses `/itm/123456?nordt=true` | phase-W.md's "IDs come from the URL" rule; query strings and slugs must never leak into the ID |
| substack rewrite proven via `fx.order` | assert exact fetched URL + fetch count 1 | The record proves nothing about which endpoint was hit (a hand fixture keyed on the page URL would also "work" if Extract fetched the wrong thing and the fake fell through to a substring miss). The fake's request log is the only honest witness |
| substack `url` = page URL | fixture test asserts `/p/hello-world`, custom-domain test keeps `https://custom.blog/p/x` | Consumers paste page URLs; records that leak implementation endpoints (the API path) break diffing/watch and look like key exfil |
| `text` pins stripTags' glue ceiling | expected `"Hello world.Second graph."` (block tags glue, no breaks) | The ceiling is honest and documented (jobs.go ponytail comment); pinning the glued output means any future upgrade to a real HTML-to-text helper flips this test red deliberately |
| DOM numbers are float64 | rating/reviews/likes/comments/reactions asserted as float64 | Same type JSON paths emit (`numVal`); a record where `"reviews": 8234` is sometimes float64 and sometimes int breaks Go consumers who type-assert once |
| Dates raw passthrough | assert exact fixture strings | V deviation 4 carries: zero consumers parse them in-repo; parsing adds a failure mode |
| ebay condition prefix-strip | `"https://schema.org/NewCondition"` ⇒ `"NewCondition"` | Full URL in a record is schema-namespace leak-shaped noise; the suffix is the honest enum. Pinned so a later "keep the URL" refactor flips a test |
| Woo plural/singular pin | `Match` negative on `/products/x`, positive on `/product/x` | The one-character distance between shopify's permalink and Woo's default is exactly the class of bug a Contains-path matcher ships; both directions pinned |
| Five autos get a positive dispatch test | `TestW_AutoDispatch` asserts the returned **name** per URL | V only needed negatives (all five were OptIn); W is the first wave of new autos — `MatchURL` returning the WRONG auto (registration-order surprise) is now a real failure mode |
| Existing dispatch pins stay untouched | no edits to StrictOnly/OptInNeverSteals | Verified their URL lists sit on example.com-family hosts — no collision with the six new host-bound matchers. Editing them anyway would dilute their historical guarantee |
| Registry counts are phase deliverables | 3 named file:line edits + US-5 gates | Both count pins hard-assert 21 and WILL fail if forgotten — same trap V validation caught, same explicit-row cure |
| `amazon-product-dom.html` optional | fixture-count gate allows 15 or 16 | Only add a second DOM fixture if the live capture shows a genuinely different common shape; speculative fixtures are fixture sprawl |

---

## 7. Example Test Case

The `amazon_product` suite — the DOM-first pattern `devto.go` clones and the fallbacks borrow (append as `vertical/amazon_test.go`):

```go
package vertical_test

import (
	"math"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

const amazonPageHTML = `<!DOCTYPE html><html><body>
<span id="productTitle" class="a-size-large">Sony WH-1000XM4 Wireless Headphones</span>
<div id="corePriceDisplay_desktop">
  <div class="a-price"><span class="a-offscreen">$289.99</span></div>
  <span class="basisPrice"><span class="a-offscreen">$349.99</span></span>
</div>
<span data-hook="rating-out-of-text">4.5 out of 5 stars</span>
<span id="acrCustomerReviewText">8,234 ratings</span>
<div id="availability"><span>In Stock</span></div>
<div id="feature-bullets"><ul>
  <li><span class="a-list-item">Industry-leading noise cancellation</span></li>
  <li><span class="a-list-item">30-hour battery life</span></li>
  <li><span class="a-list-item">Speak-to-chat</span></li>
</ul></div>
</body></html>`

func TestAmazonProductMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("amazon_product")
	if !ok {
		t.Fatal("amazon_product not registered")
	}
	yes := []string{
		"https://www.amazon.com/dp/B08N5WRWNW",
		"https://amazon.com/dp/B08N5WRWNW",
		"https://www.amazon.com/gp/product/B08N5WRWNW/ref=s9",
	}
	no := []string{
		"https://www.amazon.com/s?k=headphones", // search, not a product
		"https://www.amazon.de/dp/B08N5WRWNW",   // regional TLD: ponytail ceiling
		"https://notamazon.com/dp/B08N5WRWNW",
		"file:///tmp/dp/B08N5WRWNW",
	}
	for _, raw := range yes {
		if !ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = false, want true", raw)
		}
	}
	for _, raw := range no {
		if ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = true, want false", raw)
		}
	}
}

func TestAmazonProductExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("amazon_product")
	const page = "https://www.amazon.com/dp/B08N5WRWNW"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"amazon.com/dp/": {body: []byte(amazonPageHTML)},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Sony WH-1000XM4 Wireless Headphones" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["asin"] != "B08N5WRWNW" { // from the URL, never the DOM
		t.Errorf("asin = %v", rec["asin"])
	}
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-289.99) > 1e-9 {
		t.Errorf("price.amount = %v, want 289.99", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	list, ok := rec["list_price"].(map[string]any)
	if !ok {
		t.Fatalf("list_price = %#v, want map", rec["list_price"])
	}
	if amt, _ := list["amount"].(float64); math.Abs(amt-349.99) > 1e-9 {
		t.Errorf("list_price.amount = %v, want 349.99", list["amount"])
	}
	if rec["rating"] != 4.5 { // "4.5 out of 5 stars" → float64
		t.Errorf("rating = %#v, want 4.5", rec["rating"])
	}
	if rec["reviews"] != 8234.0 { // "8,234 ratings" → float64
		t.Errorf("reviews = %#v, want 8234", rec["reviews"])
	}
	if rec["availability"] != "In Stock" {
		t.Errorf("availability = %v", rec["availability"])
	}
	feats, ok := rec["features"].([]string)
	if !ok || len(feats) != 3 || feats[0] != "Industry-leading noise cancellation" {
		t.Errorf("features = %#v, want 3 in order", rec["features"])
	}
	if rec["url"] != page {
		t.Errorf("url = %v, want page URL", rec["url"])
	}
}

func TestAmazonProductExtract_PriceStrings(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("amazon_product")
	cases := []struct {
		name string
		html string
		check func(t *testing.T, rec map[string]any)
	}{
		{"no list price", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">$10.00</span></div>`,
			func(t *testing.T, rec map[string]any) {
				if _, ok := rec["list_price"]; ok {
					t.Error("list_price present — omission broken")
				}
			}},
		{"comma thousands", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">$1,299.00</span></div>`,
			func(t *testing.T, rec map[string]any) {
				p := rec["price"].(map[string]any)
				if amt, _ := p["amount"].(float64); math.Abs(amt-1299) > 1e-9 {
					t.Errorf("amount = %v, want 1299", p["amount"])
				}
			}},
		{"unknown currency kept amount", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">CHF 49.00</span></div>`,
			func(t *testing.T, rec map[string]any) {
				p := rec["price"].(map[string]any)
				if _, ok := p["currency"]; ok {
					t.Error("currency present for unmapped symbol — guessing broken")
				}
			}},
		{"unparseable price omitted", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">Currently unavailable</span></div>`,
			func(t *testing.T, rec map[string]any) {
				if _, ok := rec["price"]; ok {
					t.Error("price present on unparseable string — must omit, never zero")
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
				"amazon.com/dp/": {body: []byte(tc.html)},
			}}
			rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.amazon.com/dp/B0TEST"))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			tc.check(t, rec)
		})
	}
}

func TestAmazonProductExtract_ChallengeStub(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("amazon_product")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"amazon.com/dp/": {body: []byte(`<html><body>Sorry! Something went wrong!</body></html>`)},
	}}
	const page = "https://www.amazon.com/dp/B0CHALLENGE"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: amazon_product:") ||
		!strings.Contains(err.Error(), page) {
		t.Fatalf("err = %v, want typed no-product error naming the URL", err)
	}
}
```

(The other five suites mirror this shape. JSON-LD-first files (etsy/ebay/woo) swap the DOM const for a fixture-loaded record test plus a `-dom` fallback test — V's `jobs_test.go` pattern; `substack_test.go` replaces the HTML const with JSON and adds the `fx.order` rewrite assert; `devto_test.go` clones this file with crayons selectors and a tags-order assert.)

---

## 8. Execution Prompt

Copy everything between the `---` lines into a new pi session to write these tests (alongside the phase-W implementation):

---
You are writing the tests for **Phase W of magpie** — per-site verticals wave 2: `amazon_product`, `ebay_item`, `etsy_listing` (host+path commerce), `woocommerce_product` (OptIn `/product/` shape), `substack_post` (public JSON API), `dev_to_article` (server-rendered DOM). magpie is a Go CLI web scraper (module `github.com/motherlodelab/magpie`, Go 1.26+, CGO-free) at `/home/domidex/projects/magpie`. Read `AGENTS.md`, `.pi/rules/go.md`, `.pi/rules/testing.md`, `plan/phase-W.md`, and `plan/phase-W-tests.md` first. Default suite is hermetic — **zero network in every tier**; live captures are committed files behind `//go:build live`; no new deps (`git diff go.mod` stays empty); `stack: go` means manual execution, test-first per task W.1→W.7.

### Acceptance Criteria (from User Stories)

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | amazon/ebay/etsy auto-fire, return priced records, IDs from URL | 3 test files (~14 tests) | MatchURL auto-fires, amazon.de/`/s?k=`/notamazon miss; price {amount float64, currency} at 1e-9; amazon sale+list; ebay/etsy JSON-LD exact AND DOM fallback exact; typed errors |
| US-2 | woo works by `--name` only, `/product/` not `/products/` | woocommerce_test.go (~5 tests) | plural negative; OptIn pin; MatchURL miss; Lookup+Extract on non-matching host; sale-over-regular; omission |
| US-3 | substack fetches the public API, honest text | substack_test.go (~5 tests) | hostSuffix table incl. notsubstack no; record exact; text glue-pinned; url = page URL; HTTP 500 typed error; fx.order proves rewrite, 1 fetch |
| US-4 | dev_to fires on articles only | devto_test.go (~4 tests) | `/t/` and 3-seg negatives; tags order; floats; first-p description; typed error |
| US-5 | registry 27 everywhere, no collateral | count edits + dispatch pins + gates | 27/27/27; TestW_AutoDispatch by name; woo never auto; go.mod diff empty; testdata = 15–16 named paths; README/spec greps |

### Why There Are No New Fakes
Extractors are black boxes behind the `Fetcher` interface — the existing `fakeVerticalFetcher` (vertical/vertical_test.go:20, `package vertical_test`, host-substring keyed, longest-match wins, `fakeResp{status, body, err}`, **`.order` records every fetched URL**) plus `verticalFixture` (:73) and `mustURL` (:82) are in scope for every new test file in that directory. Variant shapes are inline literals. Do NOT invent a second fetcher fake, an httptest server, or a golden-file harness.

### What NOT to Test
- **webclaw fixtures** — AGPL, never copied; hand-write or capture our own.
- **The unexported price parser / hostSuffix directly** — `package vertical_test` boundary; behavior only, through `Extract`/`Match`.
- **Regional price formats** (`€12,50`) — en-US dot-decimal is the pinned `ponytail:` ceiling; testing the misparse would pin a bug as a feature.
- **goquery itself** — it renders hand fixtures deterministically; we test OUR selectors and mapping only.
- **Time parsing** — dates are raw passthrough (V deviation 4); never assert a parsed/normalized date.
- **MCP list rendering beyond the count** — only `len(exs)` changes; `TestToolCatalog` stays at 12.
- **Existing dispatch pins** — `TestMatchURL_StrictOnly`/`TestOptInNeverSteals`/`commerce_test.go`/`og_test.go` stay byte-identical (verified non-colliding).
- **Real network** — the W.7 capture pass is manual CLI, never a test; `-tags live` reads committed bytes.

### Critical: The Harness You Must Reuse (all in `vertical/vertical_test.go`, `package vertical_test`)

```go
// :20 — keyed by URL substring, longest match wins, "$" anchors to URL end.
fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
    "amazon.com/dp/": {body: verticalFixture(t, "amazon-product.html")}, // status 0 ⇒ 200
    "demo.substack.com/api/": {body: verticalFixture(t, "substack-post.json")}, // keyed on the REWRITTEN url
    "demo.substack.com/api/v1/err": {status: 500, body: []byte(`{"error":"x"}`)},
}}
// fx.order []string — every fetched URL in order; assert:
//   fx.order[0] == "https://custom.blog/api/v1/posts/x" && len(fx.order) == 1
// :73 verticalFixture(t, "name") reads ../testdata/vertical/name (json included — bytes are bytes)
// :82 mustURL(t, "https://…") *url.URL
// Entry points: vertical.Lookup(name); vertical.MatchURL(raw)
// Extract: ex.Extract(t.Context(), fx, mustURL(t, page)) (map[string]any, error)
// Precedents: TestOG_NeverAutoFires (og_test.go:12), TestOptInNeverSteals (:173),
//             shopify currency-omission pin (commerce_test.go:36), jobs stripTags use (jobs.go:34)
```

New tests take `t.Parallel()`. Floats: `math.Abs(got-want) > 1e-9`. Errors: `strings.Contains` on `vertical: <name>: …` / `vertical: GET <url>: HTTP 500` (fetchBytes :117 contract). `t.Context()` house style.

### Test Files to Create / Edit

- **NEW `vertical/etsy_test.go`** (~4 tests) — match table (`etsy.com`/`www.` `/listing/{id}` yes; `/shop/x`, `ebay.com/itm/1`, `file://` no); full record on `etsy-listing.html` (title "Handmade Ceramic Mug, 12oz"; listing_id 1283746291 from path; price {32, USD}; seller "ClayAndKilnStudio" from `offers.seller.name`; rating 4.9 / reviews 214 from aggregateRating; availability; url = page); DOM fallback on `etsy-listing-dom.html` (title from `h1[data-test-id=listing-page-title]`; price from og:price:amount/currency metas "32.00"/"USD"; **seller/rating/reviews/availability keys absent**); no-source error.
- **NEW `vertical/woocommerce_test.go`** (~5 tests) — match table (`/product/merino-beanie` yes; `/products/hoodie` NO — shopify's plural; any scheme-gated host yes via Lookup.Match); `TestWooCommerce_NeverAutoFires` (`MatchURL("https://shop.example/product/x")` misses; `OptIn == true`); `--name` smoke (`Lookup` + `Extract` on `https://anyhost.example/product/x` against served `woo-product.html` bytes); full record on `woo-product.html` (title "Merino Wool Beanie"; sku "MWB-01"; brand "Northwind" from `{name}` object; price {24.5, USD}; availability InStock; rating 4.8 / reviews 57; images []string; url = page); DOM fallback on `woo-product-dom.html` (sale-over-regular: ins 19.90 beats del 24.50; sku; stock_status "In stock"; brand/rating/reviews absent); no-source error.
- **NEW `vertical/substack_test.go`** (~5 tests) — hostSuffix match table (`demo.substack.com/p/hello-world`, `a.b.substack.com/p/x`, `substack.com/p/x` yes; `notsubstack.com/p/x` NO; `/about`, `/archive` no; `file://` no); full record (fake keyed on `demo.substack.com/api/v1/posts/` serving `substack-post.json`; title/subtitle; author "Jane Q. Writer"; published raw "2026-08-14T09:00:00Z"; likes 823, comments 47 as float64; cover_image; **text == "Hello world.Second graph."** — stripTags glue ceiling pinned; **url == "https://demo.substack.com/p/hello-world"** — page URL, never the API endpoint); author-shape variants (inline JSON: `bylines:[{name}]` and `author:[{name}]` ⇒ same string; neither ⇒ key absent); API-500 (error contains `vertical: GET https://demo.substack.com/api/v1/posts/x: HTTP 500`); custom-domain rewrite (Extract on `https://custom.blog/p/x`, fake keyed on `custom.blog/api/v1/posts/x`, assert `fx.order[0]` + `len(fx.order)==1`).
- **NEW `vertical/devto_test.go`** (~4 tests) — match table (`dev.to/alexdev/go-generics` yes; `dev.to/t/golang` NO; `dev.to/alexdev/generics/comments` no (3 segs); `dev.to/about` no (1 seg); `notdev.to/a/b` no); full record on `dev-to-article.html` (title "Go generics in the wild"; author "Alex Devlin"; published = `time[datetime]` raw "2026-07-02T10:30:00Z"; tags `["go","generics","tutorial"]` in order; reactions 128.0 + comments 12.0; description "Writing scrapers in Go teaches you things."; url = page); no-source (empty body ⇒ typed error).
- **NEW `vertical/ebay_test.go`** (~5 tests) — match table (`ebay.com`/`www.` `/itm/{id}` yes; `/itm/` no id no; `etsy.com/listing/1` no; `file://` no); full record on `ebay-item.html` (title "Vintage Polaroid SX-70 Land Camera"; item_id from path; price {89.99, USD}; condition "NewCondition" — `https://schema.org/` prefix stripped; seller "camera-gear-2010"; availability; **url = page URL — decoy `"url"` inside the JSON-LD block must NOT win**); DOM fallback on `ebay-item-dom.html` (`#itemTitle`; `.x-price-primary` "US $89.99" ⇒ {89.99, USD} via the price parser; condition from `.x-item-details`; seller from `.x-sellercard-atf__info__about-seller`); id-from-query-shaped-path (`/itm/123456?nordt=true` ⇒ item_id 123456-only-digits); no-source error.
- **NEW `vertical/amazon_test.go`** (~5 tests) — full source is §7 of phase-W-tests.md; clone verbatim, adjusting only if the landed selectors differ from the fixture spec below (fix fixture + test together, one unit).
- **APPEND `vertical/live_test.go`** — 6 `TestLive*` funcs (`//go:build live` already on line 1): each serves `verticalFixture(t, "<name>-live.*")` through the fake → `Extract` ok → primary keys non-empty (title + url; substack additionally text non-empty). Zero exact-value asserts.
- **EDIT `vertical/vertical_test.go`** — `TestList_ExactNameSet` (:91): add `// Phase W additions.` + the six names.
- **EDIT `mcp/agent_test.go`** ~:506 — `!= 21` → `!= 27`, message "+ 6 Phase W".
- **EDIT `cli/agent_test.go`** ~:241 — `!= 21` → `!= 27`, message "+ 6 Phase W".

### Fixture Specs (hand-written — exact-oracle; write BEFORE the tests that read them)

- `amazon-product.html`: the §7 `amazonPageHTML` const, verbatim.
- `ebay-item.html`: ld+json Product — name "Vintage Polaroid SX-70 Land Camera"; offers{price 89.99, priceCurrency USD, availability "https://schema.org/InStock", itemCondition "https://schema.org/NewCondition", seller {name "camera-gear-2010"}}; **decoy `"url":"https://decoy.example/x"`**.
- `ebay-item-dom.html`: no JSON-LD; `#itemTitle`; `.x-price-primary` with "US $89.99"; `.x-item-details` "Condition: New"; `.x-sellercard-atf__info__about-seller` link "camera-gear-2010".
- `etsy-listing.html`: ld+json Product — name "Handmade Ceramic Mug, 12oz"; offers{price 32.00, priceCurrency USD, availability InStock, seller {name "ClayAndKilnStudio"}}; aggregateRating{ratingValue 4.9, reviewCount 214}.
- `etsy-listing-dom.html`: NO JSON-LD; `h1[data-test-id=listing-page-title]`; `meta[property=og:price:amount]` "32.00"; `meta[property=og:price:currency]` "USD". Nothing else — the omission pin depends on it.
- `woo-product.html`: ld+json Product — name "Merino Wool Beanie"; sku "MWB-01"; brand {name "Northwind"}; offers{price 24.50, priceCurrency USD, availability InStock}; aggregateRating{4.8, 57}; image array of 2 URLs.
- `woo-product-dom.html`: no JSON-LD; `.summary` with `.product_title`; `.price` containing `del` "$24.50" AND `ins` "$19.90" (sale wins); `.sku` "MWB-01"; `p.stock` "In stock".
- `substack-post.json`: `{"title":"Hello, world","subtitle":"A demo post","post_date":"2026-08-14T09:00:00Z","like_count":823,"comment_count":47,"cover_image":"https://substackcdn.com/cover.png","canonical_url":"https://demo.substack.com/p/hello-world","body_html":"<p>Hello <b>world</b>.</p><p>Second graph.</p>","author":[{"name":"Jane Q. Writer"}]}`.
- `dev-to-article.html`: `h1.crayons-title`; `.crayons-article__header__author` profile link "Alex Devlin"; `time datetime="2026-07-02T10:30:00Z"`; three `a.crayons-tag` (go, generics, tutorial); `.crayons-reaction__count--reaction` "128"; comments count element "12"; body first `<p>` "Writing scrapers in Go teaches you things."
- Captures (`*-live.html` ×5, `substack-post-live.json`): W.7 manual pass (`magpie scrape … --page-format raw --out` / curl for substack). Blocked ⇒ `--browser random`; worst case ship hand fixtures + PR note — never weaken a selector to pass a capture. Never webclaw files.

### Data Model Notes (Go)
- Records are flat `map[string]any`; assert with direct type-assertions (`rec["price"].(map[string]any)`), never re-marshal.
- **Omission is the contract:** absent source ⇒ absent key; assert `_, ok := rec[k]; ok ⇒ Errorf`, never `""`/`0` sentinels (shopify currency pin style, commerce_test.go:36).
- `price`/`list_price` are nested `{amount float64, currency string}`; scalar-or-array JSON-LD offers normalize like `productMap` (commerce.go).
- DOM numbers are float64 (rating/reviews/reactions/likes/comments).
- Dates raw strings; IDs from URL path; substack `url` = page URL.
- Errors: `vertical: <name>: no <thing> at <url>`; fetch-layer: `vertical: GET <url>: HTTP <n>`.

### Success Criteria
- `go test ./vertical/ -v -count=1` green: ≥24 new tests across the six new `_test.go` files (live tier additional, tagged out)
- `go test ./... -count=1` green; `go test -race ./vertical/ ./mcp/ ./cli/ -count=1` clean
- `go test -tags live ./vertical/ -run Live -v -count=1` green after the W.7 capture pass (or the per-extractor test is absent with an explicit PR note)
- `git diff go.mod` empty; mcp/ + cli/ diffs are exactly the two count bumps; `git diff vertical/commerce_test.go` empty; `TestMatchURL_StrictOnly`/`TestOptInNeverSteals` untouched
- `git status --porcelain testdata/` shows ONLY the 15 (or 16 with `amazon-product-dom.html`) named paths
- `go run ./cmd/magpie vertical --list | grep -c '"name"'` → 27; README greps: `"all 27 extractors"` present, six names in the list; `grep "desktop-only .amazon_product." spec.md` EMPTY
- `go vet ./...`, `gofmt -l .`, `golangci-lint run ./...` clean

### Expected File Structure at End
See phase-W-tests.md §4 — same tree, no extras.

---

## 9. Run Commands

```bash
# Baseline BEFORE writing (non-vacuous gate — testing.md)
go test ./vertical/ -count=1

# Fast hermetic suite (every push)
go test ./... -count=1

# Per-task focus (mirrors the W.x sanity checks)
go test ./vertical/ -run EtsyListing -v -count=1          # W.1
go test ./vertical/ -run WooCommerce -v -count=1          # W.2 (incl. NeverAutoFires + --name smoke)
go test ./vertical/ -run Substack -v -count=1             # W.3 (incl. fx.order rewrite + HTTP 500)
go test ./vertical/ -run DevTo -v -count=1                # W.4
go test ./vertical/ -run EbayItem -v -count=1             # W.5
go test ./vertical/ -run AmazonProduct -v -count=1        # W.6
go test ./vertical/ -run 'AutoDispatch|NeverAutoFires|StrictOnly|OptInNeverSteals|ExactNameSet' -v -count=1  # dispatch/registry pins

# Live tier (after the W.7 capture pass; still no network)
go test -tags live ./vertical/ -run Live -v -count=1

# Registry-count gates
go test ./mcp/ ./cli/ -run 'TestVertical|TestAgent' -v -count=1

# Fixture + collateral gates
git status --porcelain testdata/                          # ONLY the 15–16 named paths
git diff go.mod vertical/commerce_test.go                 # both empty
git diff vertical/vertical_test.go | grep -c '^+'         # ExactNameSet additions only

# Race on touched packages
go test -race ./vertical/ ./mcp/ ./cli/ -count=1

# Docs gates
grep -n "all 27 extractors" README.md && grep -n amazon_product README.md
! grep -n "desktop-only .amazon_product." spec.md         # strike sentence gone

# Full gate (mirrors phase-W exit criteria)
go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./... \
  && test -z "$(git diff go.mod)" \
  && go run ./cmd/magpie vertical --list | grep -c '"name"'   # 27
```
