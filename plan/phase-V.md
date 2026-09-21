# Phase V — Standards verticals wave 1: job_posting, event, local_business, article, rss

**Duration:** 2 days (~15.5h)
**Depends on:** master @ c0ab0b5 (`vertical.Register` seam merged @ 3509cbb; provider-seam PR #19 touched only cli/extract; upwork_job PR #20 added vertical/upwork{,_test}.go + a fixture without touching any plan-referenced file; the 16 existing extractors; `plan/verticals.md` thesis). Independent of Phase K desktop work — public-repo-only, additive-only.
**Blocks:** Nothing hard. Feeds the GUI demo breadth (paste-a-URL works on thousands more sites) and `watch`/`diff` monitoring (`rss` is its fuel, per verticals.md).
**Risk Level:** LOW — pure additive files against a stable, proven pattern (JSON-LD clones of `ecommerce_product`/`trustpilot`; XML clone of `arxiv`); hermetic fixture tests; no behavior change to existing extractors.
**Stack:** go
**Runner:** none — `run-phase` hard-blocks on `stack: go` (AGENTS.md); execute manually via the execution prompt in §6.

**Scope thesis (tier selection from `plan/verticals.md`, 2026-09-19, which this phase implements):** ride standards, not sites — one schema.org extractor covers thousands of sites. Per-site extractors (amazon_product, etsy, …) are explicitly out — the one sanctioned exception is `upwork_job` (PR #20: a challenge-typed surface the generic extractors can't reach); the Amazon repricing vertical belongs in the **private desktop repo** as a proprietary extractor via the `Register` seam — per the `Register` doc comment in `vertical/vertical.go` ("the desktop app registers proprietary verticals at startup"), not verticals.md itself. **License hygiene: never copy webclaw's testdata fixtures** (they are AGPL; this repo is MIT) — capture or hand-write our own.

**Deferred (do not build here):** `osm` (input is query+bbox, breaks the URL-addressed Extractor contract — needs its own design), wikidata/sec_edgar/wikipedia (wave 2), all per-site verticals.

---

## 1. Objective + What Success Looks Like

Add five zero-LLM, zero-key extractors, taking the registry from 16 to 21: Tier 1's `job_posting`, `event`, `local_business` (schema.org JSON-LD normalization — each a thousands-of-sites cover) and `rss` (RSS 2.0 + Atom via stdlib XML — millions of feeds, fuel for `watch`/`diff`), plus the first of Tier 2, `article` (schema.org JSON-LD — types what `og` half-covers). All five are **OptIn (explicit-only)**: they match permissively by nature, so they never auto-fire from `MatchURL` — same contract as `ecommerce_product`. A small shared helper generalizes the JSON-LD block scan so four new extractors don't copy-paste `scanProductBlocks`.

1. `go run ./cmd/magpie vertical --list` shows **21** extractors, including the five new names with usable `desc` strings (these render in MCP `list_extractors`).
2. `go test ./vertical/ -v` green, **no network**: every extractor runs against committed `testdata/vertical/` fixtures via the existing `fakeVerticalFetcher` pattern.
3. `MatchURL` auto-dispatch unchanged: a test asserts none of the five fires for arbitrary URLs (OptIn semantics locked).
4. Refactor safety: `ecommerce_product` tests pass **unchanged** after `scanProductBlocks` is generalized into `blocksOfType`.
5. `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` all green; `gofmt -l .` prints nothing.

**Good:** "`magpie vertical --list` prints `job_posting`, and `go test ./vertical/ -run TestJobPosting -v` passes 4 tests against `testdata/vertical/job-posting.html`"
**Bad:** "The new verticals work"

## 2. Key Design Decisions

```
vertical/
├── vertical.go        (unchanged: Extractor, register, helpers str/num/child/hostIs/pathSegs)
├── jsonld.go          (NEW: blocksOfType + firstTypedBlock — generalized scanProductBlocks)
├── commerce.go        (refactor: scanProductBlocks delegates to blocksOfType)
├── jobs.go            (NEW: job_posting)   ├── events.go   (NEW: event)
├── business.go        (NEW: local_business)├── article.go    (NEW: article)
└── rss.go             (NEW: rss, RSS 2.0 + Atom via encoding/xml — clone arxiv's pattern)
```

### Data Model Rules (Go — follow exactly)

- **Records are `map[string]any`** with flat, fixed keys — the established convention (every existing extractor returns maps; JSON-LD is schemaless by nature; typed structs here would buy nothing and fight the helpers).
- **Self-registration:** each new extractor is one `register(Extractor{...})` in an `init()` in its own file — Match + Extract + Info with honest `Patterns` (human-readable examples, never regexes) and a one-sentence `Desc` (renders in MCP `list_extractors`). Each new name must also join `TestList_ExactNameSet` in `vertical/vertical_test.go` — that test pins the exact registry set and enforces non-empty Label/Desc/Patterns.
- **OptIn: true on all five.** JSON-LD extractors are permissive by construction (any http(s) URL might carry the block); `rss` matches URL *hints* (`/feed`, `/rss`, `.xml`/`.atom` suffix) — a hint is not proof, so OptIn. **`MatchURL` must never auto-fire them** — a test pins this.
- **Reuse, never duplicate:** `str`, `num`, `child`, `anyMap`, `hostIs`, `pathSegs`, `fetchBytes` from vertical.go; `clean.HarvestSidecar` + the new `blocksOfType` for JSON-LD. If a helper is missing, add it once in vertical.go or jsonld.go.
- **Omission over guessing:** if the page doesn't carry a field (currency, endDate…), omit the key — never emit empty strings/zeros. Precedent: shopify's locked-absent `currency` key.
- **Test style:** `package vertical_test`, `fakeVerticalFetcher{bodies: ...}` + `verticalFixture(t, "...")` + `mustURL`; float comparisons with `1e-9` tolerance. No live fetches, ever.

### Normalization contracts (fixed key sets)

| Extractor | Triggering JSON-LD `@type` | Record keys |
| :-- | :-- | :-- |
| `job_posting` | `JobPosting` | title, organization, locations ([]string locality/region/country), remote (bool: `jobLocationType=TELECOMMUTE`), datePosted, validThrough, employmentType, salary {min,max,value,unit,currency}, url |
| `event` | `Event` (+subtypes) | name, startDate, endDate, isOnline (bool: VirtualLocation / `eventAttendanceMode` Online), location {name,address}, offers [{price,currency,availability,url}], performers []string, organizer, url |
| `local_business` | `LocalBusiness` (+subtypes) | name, telephone, email, url, address {street,locality,region,postalCode,country}, geo {lat,lng}, openingHours []string, priceRange, sameAs []string |
| `article` | `Article`/`NewsArticle`/`BlogPosting` | headline, authors []string, datePublished, dateModified, publisher, image, url |
| `rss` | RSS 2.0 `<rss>` or Atom `<feed>` root | title, link, description, items []{title,link,published,summary} (cap 50, `items` never nil) |

Arrays may arrive as scalar-or-array in JSON-LD (e.g. `author`, `openingHours`) — normalize both shapes (pattern exists in `productMap`'s offers handling and `isProductType`). Salary `value` may be `QuantitativeValue` (minValue/maxValue/unitText) or a number. Atom links: prefer `rel="alternate"` (or no rel), fall back to the first `<link>`; `published` from `updated` when `published` is absent.

### Fail-safe rules

- No triggering block found ⇒ typed error `vertical: <name>: no <type> data at <url>` (existing convention) — never an empty record.
- Malformed JSON-LD blocks are skipped, not fatal (scan continues; matches `scanProductBlocks` behavior).
- `rss`: undecodable as both dialects ⇒ error naming the URL; >50 items ⇒ truncate to 50 (first 50 = document order; ceiling noted in code with a `ponytail:` comment).

## 3. Tasks

### Task V.1 — `jsonld.go`: generalize the block scan (2h)

Extract the reusable core from `commerce.go`: `blocksOfType(html []byte, typ string) []json.RawMessage` (HarvestSidecar first, brace-scan fallback — exactly today's two-stage logic, parameterized by type substring) and `firstTypedBlock(html []byte, types ...string) (map[string]any, bool)`. Refactor `scanProductBlocks`/`extractEcommerce` to delegate. **No behavior change** — commerce tests must pass untouched. Inherited quirk (keep it): the brace-scan fallback fires only when the sidecar is absent, so a page whose sidecar lacks the type is a miss; the new extractors inherit that via `blocksOfType` — widening it is a later refactor if a real page ever misses.

```go
// Confirmed patterns this builds on (vertical/commerce.go, unchanged since 527e668; verified @ c0ab0b5):
sidecar := clean.HarvestSidecar(body)          // array-or-single JSON sidecar
blocks := scanProductBlocks(body)               // goquery script-scan + clean.BraceObjects
json.Unmarshal(block, &m); isProductType(m["@type"])
```

**Sanity check:** `go test ./vertical/ -run 'Commerce|Shopify' -v` green with zero edits to tests.

### Task V.2 — `job_posting` (2.5h)

**Depends on:** V.1

`vertical/jobs.go`: init-registered OptIn extractor, Match = any http(s) URL (clone `matchEcommerce`). Extract per the §2 contract. Salary: unwrap `baseSalary` as `{QuantitativeValue}` or number; `jobLocation` may be an array — collect all localities. `description` is HTML — strip tags with one regex + whitespace collapse (`ponytail:` naive vs real HTML unescape; upgrade path = clean package helper if it ever matters). Fixtures: one hand-written minimal `job-posting.html` (Greenhouse-style JSON-LD) + one real captured page (Task V.7).

Tests: match positives/negatives (http yes, file:// no), full-record assert on the hand fixture, scalar-vs-array author/location variants, no-block error.

**Sanity check:** `go test ./vertical/ -run JobPosting -v` — 4+ tests green.

### Task V.3 — `event` (2.5h)

**Depends on:** V.1

`vertical/events.go` per §2 contract. `eventAttendanceMode` enumeration contains `Online` ⇒ `isOnline:true`; location may be `Place` (name+address) or `VirtualLocation` (url). offers scalar-or-array. Fixtures: hand-written `event.html` (Eventbrite-style) + captured page.

**Sanity check:** `go test ./vertical/ -run Event -v` — 4+ tests green.

### Task V.4 — `local_business` (2h)

**Depends on:** V.1

`vertical/business.go` per §2 contract. Subtype `@type` values (`Restaurant`, `Store`, …) must match — reuse the `strings.Contains` style of `isProductType`. `geo` as `latitude`/`longitude` numbers (floats, `num`). Fixtures: hand-written `local-business.html` + captured homepage.

**Sanity check:** `go test ./vertical/ -run LocalBusiness -v` — 3+ tests green.

### Task V.5 — `article` (2h)

**Depends on:** V.1

`vertical/article.go` per §2 contract. `author` scalar, array, or `{name}` objects — normalize all three (clone `productMap`'s brand handling). Do **not** re-read meta tags here — `og` already emits `og_title`/`og_image`/`canonical` (it has no site_name/favicon); article's `headline`/`image` come from the typed JSON-LD block under their own keys, so the two never collide. Fixtures: hand-written `article.html` (NewsArticle) + captured news page.

**Sanity check:** `go test ./vertical/ -run Article -v` — 3+ tests green.

### Task V.6 — `rss` (3h)

**Depends on:** V.1

`vertical/rss.go`: Match = path hint (`/feed`, `/rss`, or path ends `.xml`/`.atom`) — permissive hint ⇒ **OptIn**. Sniff root element (`rss` vs `feed`) into two small structs (`encoding/xml`, clone `arxiv.go`'s style — no external feed library, AGENTS.md no-new-deps). Item cap 50. `items` always non-nil. Tests: RSS 2.0 fixture, Atom fixture (incl. `rel="alternate"` link selection and `updated`-as-fallback), cap test (55 items → 50), non-feed error.

**Sanity check:** `go test ./vertical/ -run Rss -v` — 4+ tests green.

### Task V.7 — Fixture capture pass + docs (1.5h)

**Depends on:** V.2–V.6

- Dogfood capture (manual, live network, NOT tests): `magpie scrape <url> --page-format raw --out testdata/vertical/<name>-live.html` for one real page per vertical (a Greenhouse/Lever-hosted posting, an Eventbrite event, a business homepage, a news article, a blog feed). Blocked by a challenge? Retry with `--browser random`; worst case ship hand-written fixtures only and note it. **Never commit webclaw fixtures.** Run each extractor against its live fixture via a small `go test -run Live` that's skipped unless `-tags live` (hermetic suite stays clean). Note: `-tags live` is a NEW build tag — only `//go:build browser` exists today; same convention (AGENTS.md: no live-network tests in the default suite).
- README count line ("all 15 extractors" → 21, ~L290) + built-in extractor list (~L301) — both also backfill `upwork_job`, which PR #20 landed without a README bump — plus spec.md §10.5 "Vertical breadth" bullet (prose, not a table): 16 → 21; mention the `Register` seam for proprietary verticals (desktop repo's amazon_product lives there).

**Sanity check:** `go test ./... ` green; `go test -tags live ./vertical/ -run Live -v` green (network, manual only); `magpie vertical --list` shows 21.

## 4. Deliverables

```
magpie/
├── vertical/
│   ├── jsonld.go              # blocksOfType + firstTypedBlock (generalized, reused by 5 extractors)
│   ├── jobs.go                # job_posting (JobPosting JSON-LD → flat record)
│   ├── events.go              # event (Event JSON-LD, isOnline/physical, offers)
│   ├── business.go            # local_business (LocalBusiness + subtypes, hours, geo)
│   ├── article.go             # article (Article/NewsArticle/BlogPosting authorship + dates)
│   ├── rss.go                 # rss (RSS 2.0 + Atom, cap 50, OptIn)
│   ├── commerce.go            # refactor: delegates to blocksOfType (no behavior change)
│   ├── jobs_test.go, events_test.go, business_test.go, article_test.go, rss_test.go
│   └── jsonld_test.go         # generic scanner + OptIn/MatchURL auto-dispatch pin
├── testdata/vertical/
│   ├── job-posting.html, event.html, local-business.html, article.html,
│   ├── feed-rss.xml, feed-atom.xml
│   └── *-live.html            # 5 captured pages (manual capture, V.7)
├── README.md                  # count line + extractor list 16 → 21 (incl. upwork_job backfill), Register-seam note
└── spec.md                    # §10.5 Vertical-breadth bullet
```

## 5. Exit Criteria

- [ ] `magpie vertical --list` shows 21 extractors; the five new `desc` strings render (MCP `list_extractors` inherits the data — the only mcp/ + cli/ edits are the extractor-count assertions: `mcp/agent_test.go:506` and `cli/agent_test.go:239`, 16 → 21)
- [ ] `go test ./vertical/ -v` green with 20+ new tests, zero network; every extractor has: match positives/negatives, full-record fixture assert, variant-shape test, no-block error test
- [ ] `jsonld_test.go` pins `MatchURL` never auto-fires the five (OptIn contract)
- [ ] commerce tests pass **unchanged** after the V.1 refactor (`git diff vertical/commerce_test.go` empty); `TestList_ExactNameSet` updated to the 21-name set (16 old + 5 new)
- [ ] `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` green; no new entries in go.mod
- [ ] `rss` cap + non-nil `items` pinned by tests; omission-over-guessing spot-checked (missing currency ⇒ key absent)
- [ ] README/spec updated; `-tags live` capture suite documented and passing manually

## 6. Execution Prompt

Copy everything between the `---` lines into a new pi session to implement this phase:

---

You are building Phase V of **magpie** — a Go CLI web scraper (fetch → clean → extract), module `github.com/motherlodelab/magpie`, binary `magpie`. Pure Go, **zero CGO, no new Go module dependencies without asking**. The default test suite is **hermetic: no network** — extractors run against committed fixtures. Coding ethos: lazy senior dev — minimum code, reuse the helpers, `map[string]any` records, omission over guessing. `stack: go` means the `run-phase` skill will hard-block — implement manually, test-first per task.

### Established in prior phases (facts, not suggestions)

- `vertical/vertical.go` defines: `Extractor{Info, Match, Extract, OptIn}`, `register()` (init-time), `Lookup`, `MatchURL` (skips OptIn), and helpers `str`, `num`, `child`, `anyMap`, `hostIs`, `pathSegs`, `fetchBytes`, `fetchJSON`. Leaf package: imports `fetch` (types) and `clean` only.
- `vertical/commerce.go` is the JSON-LD exemplar: `clean.HarvestSidecar(body)` → `productBlocks` (array-or-single) → fallback `scanProductBlocks` (goquery script scan + `clean.BraceObjects` + `json.Valid`) → `productMap` normalization (offers scalar-or-array, brand string-or-object). `vertical/arxiv.go` is the XML exemplar (stdlib `encoding/xml`, small structs).
- Tests live in `package vertical_test` with `fakeVerticalFetcher{bodies: map[string]fakeResp{...}}`, `verticalFixture(t, name)`, `mustURL(t, s)`; float asserts use `1e-9` tolerance. Fixtures: `testdata/vertical/*.{json,html,xml}`.
- `OptIn: true` means explicit-only (never auto-fired by `MatchURL`). `ecommerce_product` (any http(s) URL) and `shopify_product` (any `/products/` path) are the precedents.
- Registry currently has 16 extractors (PR #20 added `upwork_job`); `magpie vertical --list` and MCP `list_extractors` render `Info.{Name,Label,Desc,Patterns}`. Three tests pin the registry and MUST learn the five new names: `TestList_ExactNameSet` (`vertical/vertical_test.go`, exact-name map), `mcp/agent_test.go` (~L506, `len(exs) != 16`), `cli/agent_test.go` (~L239, `len(doc.Extractors) != 16`).

### Your Goal

Take the registry to 21: `job_posting`, `event`, `local_business`, `article` (schema.org JSON-LD → flat records) and `rss` (RSS 2.0 + Atom, stdlib XML). All five **OptIn**. Generalize the JSON-LD scan once (`jsonld.go`) instead of copy-pasting `scanProductBlocks`.

### Confirmed API patterns (from the repo @ c0ab0b5 — do not re-research)

```go
// Registration (vertical/commerce.go):
func init() { register(Extractor{Info: Info{Name: "...", Label: "...", Desc: "...",
    Patterns: []string{"https://{example}"}}, Match: matchX, Extract: extractX, OptIn: true}) }

// JSON-LD harvest (vertical/commerce.go):
sidecar := clean.HarvestSidecar(body)               // []byte, array-or-single
for _, frag := range clean.BraceObjects(scriptText) { if json.Valid([]byte(frag)) { ... } }

// XML (vertical/arxiv.go):
type feed struct { Entries []entry `xml:"entry"` }
xml.Unmarshal(body, &feed)
```

### Data Model Rules (follow exactly)

- Records: `map[string]any`, flat fixed keys per the contracts table in `plan/phase-V.md` §2 (copy it into your session). Missing source field ⇒ **omit the key** — never empty string/zero.
- One file per extractor, one `init()` register call, `OptIn: true` on all five.
- Normalize scalar-or-array JSON-LD shapes (clone `productMap` offers + `isProductType` patterns). Atoms: `rel="alternate"` link first; `updated` as `published` fallback.
- No typed record structs; no reflection; helpers from vertical.go only — new shared logic goes in `jsonld.go` once.

### Per-file guidance

- **vertical/jsonld.go + jsonld_test.go** — `blocksOfType(html, typ)` (HarvestSidecar two-stage, parameterized) + `firstTypedBlock(html, types...)`; refactor `extractEcommerce` to delegate; commerce tests must pass with zero edits. Also pin here: `MatchURL` returns no match for job/event/business/article/rss URLs (OptIn contract). Also add the five names to `TestList_ExactNameSet` in `vertical/vertical_test.go`.
- **vertical/jobs.go + jobs_test.go** — JobPosting: title, organization (`hiringOrganization.name`), locations array, remote via `jobLocationType == "TELECOMMUTE"`, dates, employmentType, salary (QuantitativeValue min/max/unitText + currency; number variant), description tag-stripped (one regex, `ponytail:` comment naming the ceiling). Fixtures: hand-written minimal + captured live.
- **vertical/events.go + events_test.go** — Event: `isOnline` from attendance-mode/virtual-location, location Place-or-Virtual, offers scalar-or-array, performers (scalar-or-array of names), organizer.
- **vertical/business.go + business_test.go** — LocalBusiness subtypes via Contains-match; address flattened; geo lat/lng floats; openingHours scalar-or-array; sameAs array.
- **vertical/article.go + article_test.go** — Article/NewsArticle/BlogPosting: headline, authors (scalar/object/array → []string), dates, publisher, image. Don't duplicate `og`'s fields.
- **vertical/rss.go + rss_test.go** — Match: path contains `/feed` or `/rss`, or ends `.xml`/`.atom`. Sniff root: RSS 2.0 (`channel.item`) vs Atom (`feed.entry`). Cap 50 items (first 50, `ponytail:` comment). `items` never nil. Undecodable ⇒ typed error naming the URL.
- **testdata/vertical/** — hand-written minimal fixtures per extractor + real captured `*-live.html` (manual `magpie scrape <url> --page-format raw --out ...`). **Never copy webclaw fixtures (AGPL).** Live pages are exercised only under `-tags live`, skipped in the default suite.
- **README.md / spec.md** — README count line ("all 15 extractors") + built-in extractor list → 21 (16 → 21, backfilling `upwork_job`, which PR #20 landed without a README bump); spec §10.5 Vertical-breadth bullet; one line: proprietary verticals (e.g. amazon_product) register via `vertical.Register` from embedders.

### Hard rules

- No new dependencies (`encoding/xml`, existing goquery/clean helpers only). No CGO. No network in default tests. `gofmt -l .` silent; `golangci-lint run ./...` green. Don't touch existing extractors beyond the V.1 delegation refactor. mcp/ and cli/ stay behavior-untouched — the ONLY edits there are the extractor-count assertions in `mcp/agent_test.go` and `cli/agent_test.go` (16 → 21); List() itself flows through automatically.

### Success criteria

1. `magpie vertical --list` → 21 extractors.
2. `go test ./vertical/ -v` green: ≥20 new tests across 6 files; OptIn/MatchURL pin passes; `TestList_ExactNameSet` carries the 21-name set.
3. `git diff vertical/commerce_test.go` empty (refactor safety).
4. `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` green; `git diff go.mod` empty; mcp/ + cli/ diffs are exactly the two count-assertion bumps.
5. Every record key set matches the §2 contracts; omission spot-checks pass (no zero-filled keys).

### Expected file structure at end

See §4 Deliverables in `plan/phase-V.md` — same tree, no extras.

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available (Extractor contract, helpers, exemplars, test fake pattern, OptIn semantics — all read from source @ c0ab0b5 today; plan-referenced vertical/ files identical since 3509cbb — PR #20 only added upwork.go/upwork_test.go + a vertical_test.go name entry)
- [PASS] Every sub-task has a clear, testable completion condition (per-task sanity checks + §5)
- [PASS] Execution prompt is self-contained: (a) prior-phase facts inline, (b) confirmed registration/harvest/XML snippets, (c) data-model rules (map records, omission rule), (d) per-file guidance with contracts table reference, (e) observable success criteria
- [PASS] Exit criteria map 1:1 to deliverables (each new file has a test matrix; refactor has an unchanged-tests criterion; docs have a list-count criterion)
- [PASS] Heavy external dependency has a fake/stub strategy (no external deps; fixtures hermetic; live capture isolated behind `-tags live`)
- [PASS] New libraries have a confirmed usage snippet (no new libraries; JSON-LD and XML patterns copied from commerce.go/arxiv.go verbatim)
