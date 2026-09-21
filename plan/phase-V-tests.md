# Phase V — Testing: Standards verticals wave 1 (job_posting, event, local_business, article, rss)

**Scope:** `vertical/jsonld.go` scanner generalization + `commerce.go` delegation (V.1), the five new extractors `vertical/{jobs,events,business,article,rss}.go` (V.2–V.6), registry-count edits (`vertical/vertical_test.go`, `mcp/agent_test.go:506`, `cli/agent_test.go:239`), the new `-tags live` capture tier (V.7), README/spec grep gates.
**Key Pattern:** **No new fakes.** Every test drives a `vertical.Lookup`'d extractor through the existing `fakeVerticalFetcher` against committed fixtures. Hand-written fixtures are the **exact-mapping oracle** (fixed key sets, omission pins); captured real pages only **prove reality** behind the new `//go:build live` tag with loose shape asserts. V.1 refactor safety is a `git diff vertical/commerce_test.go` **gate**, not duplicated tests.
**Dependencies:** stdlib `testing`, `context`, `strings`, `math`, `reflect`, `net/url` only — plus in-repo helpers: `fakeVerticalFetcher`/`fakeResp` (vertical/vertical_test.go:20/26), `verticalFixture` (:73), `mustURL` (:82), `TestList_ExactNameSet` (:91), `TestMatchURL_StrictOnly` (:138), `TestOptInNeverSteals` (:173), `TestOG_NeverAutoFires` (vertical/og_test.go:12), `TestCommerceMatch_OptInPositives`/`TestEcommerceExtract_NoProduct`/`TestEcommerceExtract_NonstandardScriptType` (vertical/commerce_test.go:13/81/95). **No new test deps; `git diff go.mod` empty.**

**Deviations from plan/phase-V.md (sanctioned, discovered while verifying seams):**
1. **Live tests live in ONE new file `vertical/live_test.go` (`//go:build live`)** — V.7 says "a small `go test -run Live`" but names no file; one tagged file covering all five beats five per-extractor tagged sections.
2. **`isOnline` is pinned as a tri-state** — the contract says `bool` but not the neither-case. Pinned: Online enum or VirtualLocation ⇒ `true`; explicit Offline enum ⇒ `false`; neither ⇒ key omitted (omission-over-guessing).
3. **`locations` join format pinned:** each jobLocation's non-empty `[locality, region, country]` joined with `", "` (contract names the parts, not the join).
4. **Dates are raw string passthrough** (`datePosted`, `pubDate`, `updated`, …) — no time parsing. Consumers parse; parsing in the extractor adds a failure mode with zero consumer.
5. **The OptIn pin extends, not replaces,** `TestMatchURL_StrictOnly`/`TestOptInNeverSteals` — those use hardcoded URL lists that happen not to collide with the five new matchers; `jsonld_test.go` adds the per-extractor `OptIn == true` asserts and the feed/careers URL misses.

---

## User Stories

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | As an agent/GUI user, I want an explicit `--vertical job_posting` call on any Greenhouse/Lever-style careers page to yield a flat job record (title, org, locations, remote, salary), so hiring data is structured without LLM calls and never auto-fires on ordinary URLs | `jobs_test.go`: `TestJobPostingMatch_Table`, `TestJobPostingExtract` (full-record vs `job-posting.html`), `TestJobPostingExtract_Variants`, `TestJobPostingExtract_NoBlock` | every §2 contract key present with the exact fixture value; missing source field ⇒ key absent (`_ ,ok := rec[k]`); salary QuantitativeValue **and** number variants; 2 locations joined per deviation 3; `remote` true incl. per-Place `jobLocationType` variant; `file://` no-match; no-block error contains `vertical: job_posting: no JobPosting data at` |
| US-2 | As an events-map builder, I want `event` to disambiguate online vs physical and normalize offers, so "what's on this weekend" queries get one honest shape | `events_test.go`: `TestEventExtract` (full-record), `TestEventExtract_AttendanceModes` (table), `TestEventExtract_Variants` (offers/performers scalar-vs-array), `TestEventExtract_NoBlock` | Mixed ⇒ `isOnline:true`; Offline ⇒ `isOnline:false`; VirtualLocation ⇒ `isOnline:true` + `location.name` without `address`; neither ⇒ `isOnline` key absent; scalar offer ≡ 1-element array; performers string/array/{name} all ⇒ `[]string`; no-block typed error |
| US-3 | As a lead-gen user, I want `local_business` on a company homepage to flatten address/geo/hours, so a single record feeds maps and CRM import | `business_test.go`: `TestLocalBusinessExtract` (full-record), `TestLocalBusiness_Subtypes` (Restaurant/Store/plain), `TestLocalBusiness_Variants` (hours scalar, region omitted) | subtype `@type` matches via Contains; `geo.lat/lng` floats within `1e-9`; `openingHours` array and scalar both ⇒ `[]string`; missing `addressRegion` ⇒ key absent; sameAs preserved in order |
| US-4 | As a news-monitor agent, I want `article` to normalize authorship (scalar/object/array) from any Article/NewsArticle/BlogPosting block, and to never duplicate `og`'s keys | `article_test.go`: `TestArticleExtract` (full-record incl. OG-tags-on-page non-duplication pin), `TestArticle_Variants` (author shapes × types), `TestArticleExtract_NoBlock` | authors `[]string` from all three shapes; `og_title`/`twitter_card` absent from the record when the page carries OG tags; `dateModified` omitted when source omits it; all three `@type` spellings extract |
| US-5 | As a watch/diff monitor, I want `rss` to return capped, non-nil items from RSS 2.0 and Atom feeds — and the registry contract to hold: 21 extractors listed, the five never auto-firing, commerce byte-untouched | `rss_test.go` (RSS/Atom fixtures, 55→50 cap, empty-feed non-nil, non-feed error) + `jsonld_test.go` OptIn pin + `TestList_ExactNameSet` 21 names + `mcp/agent_test.go:506`/`cli/agent_test.go:239` == 21 + `git diff vertical/commerce_test.go` empty | cap keeps first 50 in document order; empty feed ⇒ `items` present, len 0, non-nil; Atom `rel="alternate"` preferred, no-rel fallback, `updated`-as-`published` fallback; HTML at `/feed` ⇒ error contains `vertical: rss:` and the URL; `MatchURL("https://blog.example/feed.xml")` misses; counts 21/21/21 |

---

## 1. Component Mock Strategy

Phase type: **pure logic** (parse + normalize against committed bytes; zero network in every tier). Mock strategy in one sentence: **every row instantiates the real extractor via `Lookup`, feeds it `fakeVerticalFetcher{bodies: …}` loaded from `verticalFixture`, and asserts the flat `map[string]any` record — exact values on hand-written fixtures, key-presence/shape on variants, loose shape only behind `-tags live`.**

| Component | Mock Strategy | What to Assert | User Story |
|-----------|--------------|----------------|------------|
| `blocksOfType` sidecar path | `jsonld_test.go` (NEW, `package vertical_test` — helpers already in scope): page whose `application/ld+json` sidecar carries the typed block | returns ≥1 raw block that unmarshals to a map with `@type` = requested substring (case: exact `@type` value tested, Contains-style per `isProductType`) | US-1–4 |
| `blocksOfType` brace-scan fallback | same file — page with a **nonstandard script spelling** (mirror `TestEcommerceExtract_NonstandardScriptType`, commerce_test.go:95): no HarvestSidecar match, correct block in an odd `type` attr | fallback still finds the block (delegation preserved the two-stage behavior) | US-1–4 |
| **Inherited sidecar quirk (pinned MISS)** | same file — page WITH a valid sidecar that lacks the type AND a correct block in a nonstandard script | `blocksOfType` returns nothing → extractor errors. Comment: `ponytail:` ceiling — widening the fallback later must flip this test deliberately, not silently | US-1–4 |
| `firstTypedBlock` multi-type | same file — block typed `BlogPosting`, call with `("NewsArticle", "BlogPosting")` | second type found when first absent; returns `(nil, false)` when neither present | US-4 |
| malformed-block skip | same file — sidecar array `[malformed, good]` | malformed skipped without error, good block returned (matches `scanProductBlocks` behavior) | US-1–4 |
| OptIn / MatchURL pin | `jsonld_test.go` — `TestNewVerticals_NeverAutoFires` (modeled on `TestOG_NeverAutoFires`): all five via `Lookup` + six URL misses | each `ex.OptIn == true`; `MatchURL` misses careers/event/business/article URLs **and** `https://blog.example/feed` / `https://blog.example/feed.xml` (rss must not auto-fire even on obvious feeds); strict dispatch of existing extractors unchanged (`TestMatchURL_StrictOnly` untouched and green) | US-1–5 |
| `job_posting` mapping | `jobs_test.go` — `fakeVerticalFetcher` serving `job-posting.html` (spec in §7); full-record assert | title/org/locations/remote/dates/employmentType/salary/description/url exact per §7 fixture table; `salary.min/max` floats with `1e-9`; description tag-stripped + whitespace-collapsed (`"Build scrapers."`); `url` = fetched page URL, NOT the decoy `url` inside the JSON-LD block | US-1 |
| `job_posting` variants | `jobs_test.go` — inline HTML literals (upwork MetaOnly precedent): scalar `jobLocation`; `baseSalary: 85000` number; `jobLocationType` on the Place; no salary; no `validThrough` | number salary ⇒ `salary{"value":85000}` only; single location ⇒ len 1; per-Place TELECOMMUTE ⇒ `remote:true`; absent source keys ⇒ record keys absent | US-1 |
| `job_posting` match + error | `jobs_test.go` — `TestJobPostingMatch_Table` + `TestJobPostingExtract_NoBlock` | http/https yes; `file://`, `ftp://` no; page with no JobPosting block ⇒ error prefix `vertical: job_posting: no JobPosting data at ` + URL | US-1 |
| `event` mapping | `events_test.go` — `event.html` full record + attendance-mode table + scalar/array variants | per US-2 row; offers scalar ≡ array-of-one (deep-equal via `reflect.DeepEqual` on the offer maps); organizer from `{name}` object ⇒ string | US-2 |
| `local_business` mapping | `business_test.go` — `local-business.html` + subtype table (`Restaurant`, `Store`, plain `LocalBusiness`) + hours scalar variant + missing-region variant | per US-3 row; all three `@type` spellings extract with identical key handling; address flatten omits empty parts | US-3 |
| `article` mapping + og non-duplication | `article_test.go` — `article.html` carries OG/Twitter meta tags ON PURPOSE; author shape × `@type` variant table | per US-4 row; the record has `headline` but NEVER `og_title`/`twitter_title`/`canonical` — `article.go` must not read meta tags at all (grep-able: no `meta[` selector in article.go) | US-4 |
| `rss` RSS 2.0 | `rss_test.go` — `feed-rss.xml` fixture | `title/link/description` from channel; 4 items with `title/link/published/summary`; `published` = raw `pubDate` string (deviation 4) | US-5 |
| `rss` Atom | `rss_test.go` — `feed-atom.xml`: feed has `rel="self"` + `rel="alternate"` links; entry1 `updated` only, entry2 `published`+`updated`, entry3 link with no `rel` | feed `link` = alternate href (self excluded); entry link with no rel ⇒ its `href` used; entry1 `published` from `updated`; entry2 keeps its own `published` | US-5 |
| `rss` cap + non-nil + error + match | `rss_test.go` — programmatic 55-item RSS (`strings.Builder`, no fixture file); `<rss><channel><title>t</title></channel></rss>` empty feed; HTML body at `/feed`; match table (`/feed`, `/rss`, `.xml`, `.atom` yes; plain path no) | 55 ⇒ len 50, `items[0].title=="item-0"`, `items[49].title=="item-49"` (document order); empty feed ⇒ `items` key present, `len 0`, non-nil slice; HTML ⇒ error contains `vertical: rss:` + URL; match table per OptIn hint contract | US-5 |
| Registry counts (3 files) | `vertical/vertical_test.go` `TestList_ExactNameSet` APPEND 5 names; `mcp/agent_test.go:506` `!= 16` → `!= 21` (+ message); `cli/agent_test.go:239` `!= 16` → `!= 21` | `--list`/MCP `list_extractors`/CLI doc all report 21; `TestList_ExactNameSet` also enforces non-empty Label/Desc/Patterns for the five (free desc gate) | US-5 |
| Live captures | `vertical/live_test.go` (NEW, `//go:build live`, deviation 1) — each extractor vs its committed `*-live.{html,xml}` capture via the same `fakeVerticalFetcher` harness; **loose asserts only** | `Extract` succeeds; primary keys non-empty (`title`/`name`/`headline`; `items` len ∈ [1,50]); zero exact-value asserts — captures drift on recapture by design | US-1–5 (reality gate) |
| Docs gates | grep, not tests: README ~L290/L301 + spec §10.5 | `grep "all 15 extractors" README.md` empty; `grep -c upwork_job README.md` ≥ 1 (backfill); README list contains the five new names; spec §10.5 mentions `job_posting` and "21" | US-5 |

---

## 2. Test Tier Table

| Tier | Dependencies | Speed | When to Run |
|------|-------------|-------|-------------|
| Default (`go test ./...`) | `fakeVerticalFetcher` + committed hand-written fixtures; pure table tests — **no network, no browser, no build tags** | <1s added (suite budget ≤2 min per testing.md holds) | Every push; the only CI gate |
| Live (`go test -tags live ./vertical/`) | Committed real-page captures (`testdata/vertical/*-live.*`) through the same fake fetcher — **still zero network**; the tag quarantines recapture churn + file size, not hermeticity | ~1s | Manual: after each V.7 capture, before merge; recaptures only touch this tier |
| Manual (not a test file) | Built binary + real network: `magpie scrape <url> --page-format raw --out testdata/vertical/<name>-live.*` per V.7; then `magpie vertical --name <x> <url>` eyeball | minutes | Pre-merge capture pass ONLY — never a CI claim, never in a test |

Fixture rule: this phase adds exactly **11 testdata paths** — 6 hand-written (`job-posting.html`, `event.html`, `local-business.html`, `article.html`, `feed-rss.xml`, `feed-atom.xml`) + 5 captured (`*-live.html` ×4, `rss-live.xml`). `git status --porcelain testdata/` may show ONLY those. **Never a webclaw fixture (AGPL).**

---

## 3. No New Fakes

Every oracle exists. `fakeVerticalFetcher`/`fakeResp` (vertical/vertical_test.go:20/26 — host-substring keyed, longest-match wins, `$` anchor; `fakeResp{status, body, err}`, status 0 ⇒ 200), `verticalFixture` (:73), `mustURL` (:82) are **already in scope** — new test files live in `package vertical_test` in the same directory, so nothing is re-declared. The only new in-test material:

1. **Inline HTML literals** for variant shapes (scalar-vs-array, number salary, attendance modes) — the `TestUpworkExtract_MetaOnly` precedent: byte literals, no fixture files.
2. **A programmatic 55-item feed** built with `strings.Builder` for the cap test.
3. **The `-tags live` file** — same harness, loose asserts, `//go:build live` at line 1.

No fake fetcher is invented beyond the existing one; no `httptest` server is needed (extractors never see a transport — that's the `Fetcher` seam's whole point).

---

## 4. Test File List

```
magpie/
├── vertical/
│   ├── jsonld.go                    # DELIVERABLE (impl, V.1): blocksOfType + firstTypedBlock
│   ├── commerce.go                  # DELIVERABLE (impl, V.1): delegates — NO test edits allowed
│   ├── commerce_test.go             # GATE: git diff must stay EMPTY; TestEcommerceExtract_
│   │                                #   NonstandardScriptType (:95) green proves the fallback survived
│   ├── jsonld_test.go               # NEW: sidecar path, brace-scan fallback, inherited-quirk MISS pin,
│   │                                #   firstTypedBlock multi-type, malformed-skip, OptIn/MatchURL pin (~6 tests)
│   ├── jobs.go / jobs_test.go       # NEW: match table, full-record, variants, no-block (~4 tests)
│   ├── events.go / events_test.go   # NEW: full-record, attendance table, variants, no-block (~4 tests)
│   ├── business.go / business_test.go # NEW: full-record, subtypes, variants (~3 tests)
│   ├── article.go / article_test.go # NEW: full-record + og non-dup, variants, no-block (~3 tests)
│   ├── rss.go / rss_test.go         # NEW: RSS fixture, Atom fixture, cap, empty-feed, error, match (~6 tests)
│   ├── live_test.go                 # NEW, //go:build live: 5 loose shape tests vs committed captures
│   └── vertical_test.go             # APPEND: TestList_ExactNameSet +5 names (comment line "Phase V additions.")
├── testdata/vertical/
│   ├── job-posting.html, event.html, local-business.html, article.html,
│   │   feed-rss.xml, feed-atom.xml  # NEW hand-written exact-oracle fixtures (specs in §8)
│   └── job-posting-live.html, event-live.html, local-business-live.html,
│       article-live.html, rss-live.xml  # NEW manual captures (V.7), loose-oracle, -tags live only
├── mcp/agent_test.go                # EDIT ONLY line ~506: len(exs) != 16 → 21 (+ fatalf message)
├── cli/agent_test.go                # EDIT ONLY line ~239: len(doc.Extractors) != 16 → 21
├── README.md                        # DELIVERABLE (docs): count line 16→21 + list incl. upwork_job backfill
└── spec.md                          # DELIVERABLE (docs): §10.5 Vertical-breadth bullet 16→21 + Register note
```

Existing tests that must stay green **untouched**: all of `commerce_test.go` (diff-gated), `og_test.go`, every other extractor test, `TestMatchURL_StrictOnly`/`TestOptInNeverSteals`/`TestLookup_All`/`TestRegister` (all pass unchanged — `TestLookup_All` is dynamic), `TestToolCatalog` (12 tools — no MCP surface change).

---

## 5. Test Helper Structure (Go — no conftest.py)

| Helper | Home | Used for | New? |
|--------|------|----------|------|
| `fakeVerticalFetcher{bodies: map[string]fakeResp{…}}` | vertical/vertical_test.go:20 | serve fixture bytes per URL substring; `fakeResp{status, body, err}` | reuse |
| `verticalFixture(t, name)` | vertical/vertical_test.go:73 | load `testdata/vertical/<name>` | reuse |
| `mustURL(t, s)` | vertical/vertical_test.go:82 | parse match/extract URLs | reuse |
| `vertical.Lookup(name)` / `vertical.MatchURL(raw)` | vertical package | the only entry points tests need — extractors are black boxes behind `Fetcher` | reuse |
| inline HTML literals | each new test file | variant shapes (scalar-vs-array, number salary, attendance modes) | **new consts, in-test** |
| 55-item feed builder | rss_test.go | cap test (`strings.Builder` loop) | **new, ~10 lines** |
| `//go:build live` harness | live_test.go | 5 loose shape tests | **new file** |
| `1e-9` float tolerance | commerce_test.go:43 precedent | salary/geo asserts | reuse pattern |

Fixture-vs-literal split: committed fixture = the one full-record exact assert per extractor (readable diff when the contract changes); literals = cheap shape variants (no fixture sprawl). New tests take `t.Parallel()` (pure, per testing.md); existing tests untouched.

---

## 6. Key Testing Decisions

| Decision | Approach | Rationale |
|----------|----------|-----------|
| Hand fixture = exact oracle; live capture = reality proof | full-record asserts ONLY against hand-written fixtures; captures get key-presence asserts behind `-tags live` | Hand fixtures pin the contract and diff cleanly when it changes; real captures drift on recapture — exact asserts there would make every recapture a test-edit session. The tag quarantines exactly that churn |
| Live tier is still hermetic | `-tags live` tests read committed captures via `fakeVerticalFetcher`; nothing dials out in ANY tier | AGENTS.md's "no live-network tests" then holds unconditionally; the capture itself is manual CLI (V.7), so the networked step is outside the test binary entirely |
| V.1 safety = diff gates, not re-tests | `git diff vertical/commerce_test.go` empty + `TestEcommerceExtract_NonstandardScriptType` green | The commerce suite IS the regression suite for the refactor; duplicating it behind a new name would drift. NonstandardScriptType is the one test that exercises the brace-scan fallback — it alone proves delegation preserved the two-stage logic |
| Inherited sidecar quirk pinned as a MISS | dedicated test: sidecar-present-but-wrong-type ⇒ error, with a `ponytail:` comment | Today's behavior is a known ceiling. Pinning the miss means widening the fallback later flips a red test deliberately — the failure mode where a "refactor" silently changes when the fallback fires is the bug class this suite exists to catch |
| `url` key = fetched page URL | fixture JSON-LD carries a decoy `url` that differs from the fetch URL; assert the page URL wins | `productMap` precedent; also pins "don't trust page-declared URLs" at the mapping layer |
| Omission asserted as key-absence | `if _, ok := rec["salary"]; ok { t.Errorf(...) }` — the `TestShopifyExtract` currency pin style (commerce_test.go:36) | Empty-string/zero sentinel checks would pass a wrong implementation that emits `""`. Key-absence is the actual contract (LLM consumers must not see placeholder keys) |
| `isOnline` tri-state pinned | table: Mixed ⇒ true, Offline ⇒ false, VirtualLocation ⇒ true, neither ⇒ key absent | The contract's `bool` + the global omission rule conflict unless the neither-case is explicit; deviation 2 resolves it once, in a test, where it's visible |
| Dates raw passthrough | assert exact fixture strings | Deviation 4: zero consumers benefit from parsing; RFC-1123-style strings stay greppable and comparable |
| Cap tested programmatically | 55 items via `strings.Builder`, assert first-50 document order | A 55-item fixture file is unreadable noise; the cap is behavior, the items are filler |
| Registry counts are phase deliverables, not afterthoughts | 3 named file:line edits in the file list + US-5 | Validation found the plan's "no mcp/ changes" was false — `mcp/agent_test.go:506` and `cli/agent_test.go:239` hard-pin 16 and WILL fail. Making them explicit rows prevents a surprise-red suite at the end |

---

## 7. Example Test Case

The `job_posting` suite — the pattern all four JSON-LD extractors clone (append as `vertical/jobs_test.go`):

```go
package vertical_test

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestJobPostingMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("job_posting")
	if !ok {
		t.Fatal("job_posting not registered")
	}
	yes := []string{"https://jobs.acme.example/boards/greenhouse/123", "http://careers.example/x"}
	no := []string{"file:///tmp/x.html", "ftp://example.com/x"}
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

func TestJobPostingExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("job_posting")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"jobs.acme.example": {body: verticalFixture(t, "job-posting.html")},
	}}
	const page = "https://jobs.acme.example/boards/greenhouse/123"
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Senior Go Engineer" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["organization"] != "Acme Robotics" {
		t.Errorf("organization = %v", rec["organization"])
	}
	if rec["remote"] != true {
		t.Errorf("remote = %v, want true (TELECOMMUTE)", rec["remote"])
	}
	locs, ok := rec["locations"].([]string)
	if !ok || !reflect.DeepEqual(locs, []string{"Austin, TX, US", "Rotterdam, NL"}) {
		t.Errorf("locations = %#v, want two joined addresses in order", rec["locations"])
	}
	if rec["datePosted"] != "2026-09-01" || rec["validThrough"] != "2026-12-01" || rec["employmentType"] != "FULL_TIME" {
		t.Errorf("dates/type = %v/%v/%v", rec["datePosted"], rec["validThrough"], rec["employmentType"])
	}
	if rec["description"] != "Build scrapers." {
		t.Errorf("description = %q, want tag-stripped text", rec["description"])
	}
	if rec["url"] != page { // decoy url inside the JSON-LD block must NOT win
		t.Errorf("url = %v, want fetched page URL", rec["url"])
	}
	salary, ok := rec["salary"].(map[string]any)
	if !ok {
		t.Fatalf("salary = %#v, want map", rec["salary"])
	}
	for k, want := range map[string]float64{"min": 120000, "max": 160000} {
		got, _ := salary[k].(float64)
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("salary.%s = %v, want %v", k, got, want)
		}
	}
	if salary["unit"] != "YEAR" || salary["currency"] != "USD" {
		t.Errorf("salary unit/currency = %v/%v", salary["unit"], salary["currency"])
	}
}

func TestJobPostingExtract_Variants(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("job_posting")
	cases := []struct {
		name string
		html string
		check func(t *testing.T, rec map[string]any)
	}{
		{"number salary", `{"@type":"JobPosting","title":"X","baseSalary":85000}`,
			func(t *testing.T, rec map[string]any) {
				s, ok := rec["salary"].(map[string]any)
				if !ok { t.Fatalf("salary = %#v", rec["salary"]) }
				if v, _ := s["value"].(float64); math.Abs(v-85000) > 1e-9 { t.Errorf("value = %v", v) }
				if _, ok := s["currency"]; ok { t.Error("currency present on number salary — omission broken") }
			}},
		{"per-place telecommute", `{"@type":"JobPosting","title":"X","jobLocation":{"@type":"Place","jobLocationType":"TELECOMMUTE","address":{"addressLocality":"Remote"}}}`,
			func(t *testing.T, rec map[string]any) {
				if rec["remote"] != true { t.Errorf("remote = %v, want true", rec["remote"]) }
			}},
		{"no salary, no validThrough", `{"@type":"JobPosting","title":"X"}`,
			func(t *testing.T, rec map[string]any) {
				for _, k := range []string{"salary", "validThrough", "locations", "remote"} {
					if _, ok := rec[k]; ok { t.Errorf("%s present — omission over guessing broken", k) }
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
				"jobs.acme.example": {body: []byte(`<html><head><script type="application/ld+json">` + tc.html + `</script></head></html>`)},
			}}
			rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://jobs.acme.example/1"))
			if err != nil { t.Fatalf("Extract: %v", err) }
			tc.check(t, rec)
		})
	}
}

func TestJobPostingExtract_NoBlock(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("job_posting")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"jobs.acme.example": {body: []byte(`<html><body>no structured data</body></html>`)},
	}}
	const page = "https://jobs.acme.example/none"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: job_posting: no JobPosting data at "+page) {
		t.Fatalf("err = %v, want typed no-data error naming the URL", err)
	}
}
```

(The other four JSON-LD extractors mirror this file's four-test shape. `t.Context()` is the house style in newer vertical tests — e.g. upwork_test.go.)

---

## 8. Execution Prompt

Copy everything between the `---` lines into a new pi session to write these tests (alongside the phase-V implementation):

---
You are writing the tests for **Phase V of magpie** — Standards verticals wave 1: `job_posting`, `event`, `local_business`, `article` (schema.org JSON-LD) and `rss` (RSS 2.0 + Atom), plus the `jsonld.go` scanner generalization. magpie is a Go CLI web scraper (module `github.com/motherlodelab/magpie`, Go 1.26+, CGO-free) at `/home/domidex/projects/magpie`. Read `AGENTS.md`, `.pi/rules/go.md`, `.pi/rules/testing.md`, `plan/phase-V.md`, and `plan/phase-V-tests.md` first. Default suite is hermetic — **zero network in every tier**; live captures are read from committed files behind `//go:build live`; no new deps (`git diff go.mod` stays empty); `stack: go` means manual execution, test-first per task.

### Acceptance Criteria (from User Stories)

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | Explicit `job_posting` on any JSON-LD careers page yields a flat job record; never auto-fires | `jobs_test.go` 4 tests | contract keys exact on fixture; omission = key absent; salary QuantitativeValue + number; 2 locations; remote via top-level OR per-Place TELECOMMUTE; `file://` no-match; typed no-block error |
| US-2 | `event` disambiguates online/physical, normalizes offers | `events_test.go` 4 tests | Mixed ⇒ isOnline true; Offline ⇒ false; Virtual ⇒ true + location.name only; neither ⇒ key absent; scalar offer ≡ 1-array; performers shapes; typed no-block error |
| US-3 | `local_business` flattens address/geo/hours; subtypes match | `business_test.go` 3 tests | Restaurant/Store/plain all extract; geo floats 1e-9; hours scalar≡array; region omitted when absent; sameAs order |
| US-4 | `article` normalizes authorship; never duplicates `og` keys | `article_test.go` 3 tests | authors []string from scalar/object/array; headline not og_title even with OG tags on page; dateModified omitted when absent; 3 @type spellings |
| US-5 | `rss` returns capped non-nil items from both dialects; registry contract holds | `rss_test.go` 6 tests + `jsonld_test.go` pin + 3 count edits + diff gates | 55→50 document order; empty feed ⇒ non-nil empty items; Atom alternate/no-rel/updated-fallback; HTML at /feed ⇒ typed error; MatchURL misses /feed.xml; 21/21/21 counts; `git diff vertical/commerce_test.go` empty |

### Why There Are No New Fakes
Extractors are black boxes behind the `Fetcher` interface — the existing `fakeVerticalFetcher` (vertical/vertical_test.go:20, `package vertical_test`, host-substring keyed, `fakeResp{status, body, err}`) plus `verticalFixture` (:73) and `mustURL` (:82) are already in scope for every new test file in that directory. Variant shapes use inline HTML literals; the cap test builds its feed programmatically. Do NOT invent a second fetcher fake, an httptest server, or a golden-file harness.

### What NOT to Test
- **webclaw fixtures** — AGPL, never copied; hand-write or capture.
- **`clean.HarvestSidecar`/`BraceObjects` internals** — clean package owns them; jsonld_test asserts only through `blocksOfType`/extractor behavior.
- **MCP list rendering beyond the count** — `List()` flows through; only the `len(exs)` assertion changes.
- **Time parsing** — dates are raw string passthrough (test-plan deviation 4); don't test formatting.
- **`openingHoursSpecification`** structured form — contract is text `openingHours` only (`ponytail:` ceiling in phase-V.md).
- **`arxiv`, browser fetch paths, `og` internals** — untouched; the only og-related row is article's non-duplication assert.
- **Real network** — the V.7 capture pass is manual CLI (`magpie scrape <url> --page-format raw --out testdata/vertical/<name>-live.*`), never a test.

### Critical: The Harness You Must Reuse (all in `vertical/vertical_test.go`, `package vertical_test`)

```go
// :20 — keyed by URL substring, longest match wins, "$" anchors to URL end.
fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
    "jobs.acme.example": {body: verticalFixture(t, "job-posting.html")},   // status 0 ⇒ 200
}}
// :73 — verticalFixture(t, "name") reads ../testdata/vertical/name
// :82 — mustURL(t, "https://…") *url.URL
// Entry points: vertical.Lookup(name) (Extractor, ok); vertical.MatchURL(raw) (Extractor, ok)
// Extract: ex.Extract(t.Context(), fx, mustURL(t, page)) (map[string]any, error)
// Precedents: TestOG_NeverAutoFires (og_test.go:12), TestEcommerceExtract_NoProduct (commerce_test.go:81),
//             TestEcommerceExtract_NonstandardScriptType (:95), TestShopifyExtract currency-omission pin (:36)
```

New tests take `t.Parallel()` (pure). Float asserts use `math.Abs(got-want) > 1e-9`. No-block errors match `vertical: <name>: no <Type> data at <url>` via `strings.Contains`.

### Test Files to Create / Edit

- **NEW `vertical/jsonld_test.go`** (~6 tests): sidecar-path hit; brace-scan fallback via a nonstandard script-type page (NonstandardScriptType style); **inherited-quirk MISS pin** (sidecar present without the type + fallback-visible block ⇒ error; `ponytail:` comment); `firstTypedBlock` with two types + both-absent; malformed-block-skipped; `TestNewVerticals_NeverAutoFires` — all five `Lookup` ok && `OptIn`, and `MatchURL` misses `https://careers.example/x`, `https://events.example/x`, `https://biz.example/`, `https://news.example/post`, `https://blog.example/feed`, `https://blog.example/feed.xml`.
- **NEW `vertical/jobs_test.go`** — full source for its 4 tests is in phase-V-tests.md §7; clone that shape.
- **NEW `vertical/events_test.go`** — fixture `event.html` full record (name, startDate, endDate, isOnline:true from MixedEventAttendanceMode, location{name,address}, 2 offers [{price 300/currency THB/availability InStock/url},{price 500,…}], performers ["Alice Doe","Bob Rae"], organizer, url=page URL); attendance table (Online ⇒ true, Offline ⇒ false, VirtualLocation ⇒ true + location.name only + no address key, no-mode-no-virtual ⇒ isOnline absent); variants (scalar offer ≡ 1-array via reflect.DeepEqual; performer scalar string / [{name}] both ⇒ []string; organizer {name} ⇒ string); no-block error.
- **NEW `vertical/business_test.go`** — fixture `local-business.html` (@type Restaurant) full record (name, telephone, email, url=page URL, address{street,locality,region,postalCode,country}, geo{lat 13.7307, lng 100.5589} at 1e-9, openingHours ["Mo-Fr 08:00-18:00","Sa-Su 09:00-22:00"], priceRange "$$", sameAs 2 URLs in order); subtype table (plain `LocalBusiness`, `Store` — same asserts, Contains-style type match); variants (openingHours scalar string ⇒ 1-element array; address without addressRegion ⇒ region key absent).
- **NEW `vertical/article_test.go`** — fixture `article.html` (@type NewsArticle) **carrying og:title/og:image/twitter:card meta tags**; full record (headline, authors ["Ada Lovelace","Grace Hopper"], datePublished, dateModified, publisher "The Daily Example", image, url=page URL) + non-duplication pin (no `og_title`, `og_image`, `twitter_card`, `canonical` keys in the record); variants (author scalar string; author single {"name"}; @type `BlogPosting`; @type `Article`; no dateModified ⇒ key absent); no-block error.
- **NEW `vertical/rss_test.go`** — `feed-rss.xml` (channel title/link/description + 4 items, pubDate strings passed through raw); `feed-atom.xml` (root `<feed xmlns="http://www.w3.org/2005/Atom">` with rel="self" + rel="alternate" links; entry1 updated-only ⇒ published=updated; entry2 published+updated ⇒ published wins; entry3 link with no rel ⇒ href used); cap test (55 items via `strings.Builder` ⇒ len 50, `item-0`…`item-49`, document order); empty-feed test (`<rss><channel><title>t</title></channel></rss>` ⇒ ok, `items` present, len 0, non-nil); non-feed test (HTML at `https://blog.example/feed` ⇒ error contains `vertical: rss:` + URL); match table (`/feed`, `/rss`, `.x.xml`, `y.atom` yes; `https://blog.example/post` no).
- **NEW `vertical/live_test.go`** (`//go:build live` on line 1) — 5 tests, one per extractor, each: `fakeVerticalFetcher` serving `verticalFixture(t, "<name>-live.*")` → `Extract` ok → primary keys non-empty (`title`/`name`/`headline`; rss `items` len ∈ [1,50]). Zero exact-value asserts.
- **EDIT `vertical/vertical_test.go`** — `TestList_ExactNameSet` (:91): add `// Phase V additions.` + the five names.
- **EDIT `mcp/agent_test.go`** ~:506 — `len(exs) != 16` → `21`, update the fatalf message.
- **EDIT `cli/agent_test.go`** ~:239 — `len(doc.Extractors) != 16` → `21`.

### Fixture Specs (hand-written — exact-oracle; write these BEFORE the tests that read them)

- `job-posting.html`: ld+json block, `@type` JobPosting: title "Senior Go Engineer"; hiringOrganization.name "Acme Robotics"; top-level `jobLocationType` "TELECOMMUTE"; `jobLocation` array of TWO Places (Austin/TX/US and Rotterdam/–/NL — second has NO addressRegion, pinning the join's omission of empty parts); datePosted "2026-09-01"; validThrough "2026-12-01"; employmentType "FULL_TIME"; baseSalary QuantitativeValue{currency USD, value{minValue 120000, maxValue 160000, unitText YEAR}}; description `<p>Build <b>scrapers</b>.</p>`; a decoy `"url":"https:// decoy.example/x"` that must NOT become the record's url.
- `event.html`, `local-business.html`, `article.html`, `feed-rss.xml`, `feed-atom.xml`: per the per-file guidance above (values stated there are the assert targets — transcribe them exactly).
- Captures (`*-live.html` ×4, `rss-live.xml`): V.7 manual pass, one real page each (a Greenhouse/Lever posting, an Eventbrite event, a business homepage, a news article, a blog feed). Blocked by a challenge ⇒ retry `--browser random`; worst case ship hand fixtures only and say so. Never webclaw files.

### Data Model Notes (Go)
- Records are `map[string]any` with flat keys; assert with direct type-assertions (`rec["locations"].([]string)`), never by re-marshaling to JSON.
- **Omission is the contract:** absent source field ⇒ absent record key. Assert with `_, ok := rec[k]; ok ⇒ Errorf` — never against `""`/`0` sentinels.
- Arrays from scalar-or-array JSON-LD: both shapes produce the same `[]string`/`[]any` value (mirror `productMap`'s offers).
- `isOnline` tri-state (deviation 2): true / false / absent — all three pinned in the attendance table.
- `locations` join (deviation 3): non-empty `[locality, region, country]` joined `", "`.
- Nested records: `salary`, `location`, `address`, `geo`, `offers` items are `map[string]any`; `offers` is `[]any` of maps.
- Errors: unexported `fmt.Errorf` with the `vertical: <name>: ` prefix (commerce precedent — no new sentinel, no exit-code arm).

### Success Criteria
- `go test ./vertical/ -v -count=1` green: ≥20 new tests across `jsonld/jobs/events/business/article/rss_test.go` (live tier additional, tagged out)
- `go test ./... -count=1` green; `go test -race ./vertical/ ./mcp/ ./cli/ -count=1` clean
- `go test -tags live ./vertical/ -run Live -v` green after the V.7 capture pass
- `git diff vertical/commerce_test.go` empty; `git diff go.mod` empty; mcp/ + cli/ diffs are exactly the two count bumps
- `git status --porcelain testdata/` shows ONLY the 11 new fixture paths
- `magpie vertical --list` → 21 extractors; `grep "all 15 extractors" README.md` empty; README list contains `upwork_job` + the five new names; spec §10.5 says 21 and mentions `job_posting`
- `go vet ./...`, `gofmt -l .`, `golangci-lint run ./...` clean

### Expected File Structure at End
See phase-V-tests.md §4 — same tree, no extras.

---

## 9. Run Commands

```bash
# Baseline BEFORE writing (non-vacuous gate — testing.md)
go test ./vertical/ -count=1

# Fast hermetic suite (every push)
go test ./... -count=1

# Per-task focus (mirrors the V.x sanity checks)
go test ./vertical/ -run 'Commerce|Shopify' -v -count=1     # V.1 refactor safety (must stay green untouched)
go test ./vertical/ -run JobPosting -v -count=1             # V.2
go test ./vertical/ -run 'TestEvent' -v -count=1            # V.3
go test ./vertical/ -run LocalBusiness -v -count=1          # V.4
go test ./vertical/ -run Article -v -count=1                # V.5
go test ./vertical/ -run 'Rss' -v -count=1                  # V.6
go test ./vertical/ -run 'NeverAutoFires|StrictOnly|OptInNeverSteals|ExactNameSet' -v -count=1  # contract pins

# Live tier (after the V.7 capture pass; still no network)
go test -tags live ./vertical/ -run Live -v

# Registry-count gates
go test ./mcp/ ./cli/ -run 'TestVertical|TestAgent' -v -count=1

# Fixture-drift gate (must show ONLY the 11 new fixture paths)
git status --porcelain testdata/
git diff vertical/commerce_test.go go.mod   # both must be empty

# Race on touched packages
go test -race ./vertical/ ./mcp/ ./cli/ -count=1

# Full gate (mirrors phase-V exit criteria)
go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./... \
  && test -z "$(git diff vertical/commerce_test.go go.mod)" \
  && go run ./cmd/magpie vertical --list | grep -c .   # 21
```
