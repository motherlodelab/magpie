# Phase BB — API endpoint suggestions (`--suggest-api`)

**Duration:** ~1.5 days (~8h)
**Depends on:** Nothing — additive on the shipped `--capture-xhr` collector (fetch/xhr.go) and the browser path (fetch/actions.go). Core `master` @ `29b6218` (ahead-1 unpushed — operator pushes, no merge needed). `plan/phase-AA.md` (untracked, parallel session) is a different feature; no file overlap.
**Blocks:** Nothing directly. Enables a follow-up desktop hint (SuggestedEndpoints card after the next core tag) and pairs with AA's `magpie shopify` fast path — suggestion *discovers* the JSON API generically, shopify is that idea specialized to `/products.json`.
**Risk Level:** LOW — opt-in flag, additive omitempty fields, zero default-behavior change (without the flag every existing output stays byte-identical, pinned by test).
**Stack:** go (per AGENTS.md — `run-phase` hard-blocks on this stack; execute manually via the Execution Prompt)

---

## Objective

SPA pages hide their data behind XHR/fetch JSON APIs. Scraping the rendered DOM
of such pages is brittle (selectors die on every redesign) and LLM extraction
on them is expensive. The stable target is the JSON API itself — but today
magpie can only capture XHR bodies when the user **already knows** the endpoint
(`--capture-xhr <regexp>`). The discovery half is missing.

Origin: surveyed REA (github.com/morluto/rea) — its relevant slice is exactly
this: analyze a site's network traffic / JS to find the data API behind the
page. We don't integrate REA (TS agent workbench, wrong layer); we steal the
one idea and ship the smallest version of it.

**P:** a `--suggest-api` flag that observes every XHR/fetch response during the
browser render, ranks JSON payloads by data shape, and surfaces the top-5
candidate endpoints in the result — so the user's next run is
`magpie scrape <api-url>` or `--capture-xhr '<regexp>'`, extracting from
stable JSON instead of DOM.

Zero LLM calls. One collector, one drain, one pure ranking function.

## What Success Looks Like

1. `go test ./fetch/ ./scrape/ ./cli/ -race` passes with the new tests;
   `go build ./... && go vet ./... && golangci-lint run ./... && gofmt -l .`
   all clean; full `go test ./...` stays within budget.
2. Browser-tagged fixture (`go test -tags browser ./fetch/ -run Suggest`):
   an httptest SPA page fires `fetch('/api/items')` on load → the collector in
   `all` mode captures it; the ranker scores it top.
3. `go run . scrape <some-real-spa> --suggest-api --page-format json` prints a
   `suggested_api` array with scored endpoint candidates (site of choice —
   any public SPA that fires XHR; record the output in the PR description).
4. Without `--suggest-api`, every page-format output is **byte-identical** to
   today (Phase-G precedent — an asserting test, not a claim).
5. `magpie scrape <url> --render static --suggest-api` → exit 2 pre-I/O
   (`scrape: suggest-api requires browser rendering …`), zero fetches.
6. `--suggest-api` + `--capture-xhr '<re>'` together: `xhr` key still carries
   ONLY pattern matches; `suggested_api` is computed over ALL captures.
7. README scrape section documents the flag (row + one example).

**Good:** "spa fixture → `suggested_api: [{url: …/api/items, score: 7, items: 42, keys: [id,name,price]}]`; flag absent → output hash unchanged"
**Bad:** "API discovery works"

---

## Architecture / Key Design Decisions

```
--suggest-api ─> scrape.Options.SuggestAPI ──(validated pre-I/O, static rejected)──┐
                        │                                                          │
                        ├─> fetchURL: force browser path (joins capture-xhr arm)   │
                        ↓                                                          │
     fetch.FetchRequest{SuggestAPI: true} ─> RodFetcher.FetchWithActions           │
                        │                                                          │
                        ├─> xhrCollector{all: true, patterns: …}                   │
                        │     admit(): all ⇒ any XHR/Fetch; else matchesAny        │
                        │     drain(): ≤50 bodies × 64 KiB (unchanged caps)        │
                        ↓                                                          │
     FetchResponse.XHR (full drain, in-memory only)                                │
                        ↓                                                          │
     scrape.Run:                                                                   │
       Result.XHR            = captures filtered by patterns (semantics UNCHANGED) │
       Result.SuggestedEndpoints = SuggestEndpoints(targetURL, allCaptures, 5) ────┘
                        ↓
     CLI: page-json envelope `suggested_api` key + markdown section
```

### Decisions (locked)

- **D1 — opt-in flag, not always-on.** `SuggestAPI bool` on `scrape.Options`
  and `fetch.FetchRequest`. Always-on capture would change memory/output for
  every browser run and surprise embedders; discovery is something you ask
  for. (`ponytail:` ceiling — a user who doesn't know the flag exists gets no
  suggestion; the README example and the desktop follow-up card are the
  discoverability path.)
- **D2 — one collector, `all` mode, caps unchanged.** `xhrCollector` gains an
  `all bool`; `admit` short-circuits on it (resource-type check only,
  patterns irrelevant). Bodies still cap at 50 × 64 KiB — a capture-heavy SPA
  evicts early captures exactly as today (same ceiling, same fail-visible
  truncation). No new knobs.
- **D3 — `Result.XHR` semantics unchanged.** It keeps meaning "pattern
  matches": with patterns set, the drained captures are filtered by pattern
  before assignment; a suggest-only run leaves `XHR` nil (50 raw bodies in
  every output is not what anyone asked for). Suggestions are computed over
  the full drain inside `scrape`, then bodies are dropped.
- **D4 — the ranker is a pure deterministic function** in
  `scrape/suggest.go` (product logic stays out of `fetch/` — that package is
  transport; `scrape` already imports it):
  `func SuggestEndpoints(targetURL string, caps []fetch.XHRCapture, max int) []EndpointSuggestion`.
  Scoring (documented in the file, one line each):
  - skip non-2xx status, empty bodies, bodies that fail `json.Valid`;
  - decode with `json.Decoder.UseNumber` (bodies are ≤64 KiB already);
  - shape: top-level array of objects **or** top-level object with an
    array-of-objects field (the `{data: […], next: …}` envelope) → +3;
    flat object of scalars (config-ish) → +0; scalar JSON → skip;
  - same host as the target → +2; cross-host → eligible only with a path hint;
  - path hint (`/api`, `/v1`, `/v2`, `/v3`, `.json`) → +1;
  - body ≥ 512 B → +1 (tiny payloads are rarely the data);
  - denylist hosts dropped outright: google-analytics, doubleclick,
    googletagmanager, facebook, segment, sentry, hotjar, amplitude, mixpanel,
    clarity (fixed slice in the file — no config, YAGNI);
  - sort: score desc → items desc → URL asc (deterministic); return top `max`
    (callers pass 5).
- **D5 — DTO house style.** `scrape.EndpointSuggestion` flat struct with
  explicit json tags (XHRCapture's style): `url`, `score`, `items`,
  `keys` (first object's keys, ≤12), `reasons` (short human strings, e.g.
  `"array of 42 objects"`, `"same host"`, `"path hint /api/"`). No `method`
  field — captured XHR/fetch are GETs today (YAGNI).
- **D6 — MCP zero churn.** `Result.SuggestedEndpoints` is an additive
  omitempty field; the MCP scrape tool already serializes the result, so the
  12-tool server picks it up for free (same rationale as AA D1).
- **D7 — surfacing, no noise, no auto-fetch.** Page-format json envelope
  gains `suggested_api` (threaded through the existing `xhrJSONEnvelope` call
  site — same UseNumber re-marshal discipline); the markdown doc gains a
  short section. No stderr chatter. A suggestion is **never fetched**
  automatically — observation only; the user's re-run goes through the normal
  fetch path and its SSRF guard like any URL.
- **D8 — browser gating mirrors capture-xhr exactly.** `fetchURL`'s
  force-browser condition gains `|| o.SuggestAPI`; `ValidateOptions` gains the
  matching `render=static` rejection arm. Suggest on a static-only page is
  meaningless by construction (nothing fires XHR without a browser).
- **D9 — cache: nothing to do.** Captures are fetch-time only; a cache hit
  replays stored HTML and never populates the field. Documented here, not
  enforced in code.

### Data Model Rules (Go — house pattern: plain structs, no frameworks)

| Item | Type | Why |
|------|------|-----|
| `scrape.Options.SuggestAPI` | `bool` field, documented like `CaptureXHR` | additive; validated pre-I/O at the options boundary |
| `fetch.FetchRequest.SuggestAPI` | `bool` field beside `CaptureXHR` | one flag crosses the Fetcher interface; the Fetcher never learns *why* |
| `xhrCollector.all` | unexported `bool` field | admit() short-circuit; zero API change |
| `scrape.EndpointSuggestion` | flat struct, explicit json tags | crosses JSON output + MCP + later the desktop bridge; XHRCapture house style |
| `scrape.Result.SuggestedEndpoints` | `[]EndpointSuggestion` `json:",omitempty"` | additive; nil unless the flag was set and something scored |

---

## Tasks

### Task BB.1 — fetch side: `all` mode in the collector (1.5h)

**TDD: extend `fetch/xhr_test.go` first (RED).**

1. `fetch/fetcher.go`: add `SuggestAPI bool` to `FetchRequest` beside
   `CaptureXHR` (comment: "capture ALL XHR/fetch responses (not just
   pattern matches) for endpoint suggestion; ignored by the static path").
2. `fetch/xhr.go`: add `all bool` to `xhrCollector`; `admit` becomes:
   resource-type check first, then `if c.all { return true }`, then
   `matchesAny`. `matchesAny(nil)` still returns false — untouched.
3. `fetch/actions.go:137-141`: build the collector when
   `len(req.CaptureXHR) > 0 || req.SuggestAPI`, passing
   `all: req.SuggestAPI` (patterns compiled as today, may be empty).
4. Tests (hermetic, no browser needed — construct collectors directly):
   - `all:true` admits XHR and Fetch resource types regardless of patterns;
   - `all:false` + nil patterns admits nothing (today's behavior pinned);
   - `all:true` still respects the 50-capture cap (existing cap test pattern);
   - drain still returns metadata-only entries for evicted bodies (unchanged).

**Sanity check:** `go test ./fetch/ -run XHR -race`

### Task BB.2 — the ranker: `scrape/suggest.go` (2.5h)

**TDD: `scrape/suggest_test.go` first (RED), table-driven.**

1. `EndpointSuggestion` DTO + `SuggestEndpoints(targetURL string,
   caps []fetch.XHRCapture, max int) []EndpointSuggestion` per D4/D5.
   Decode into `any` with `json.Decoder.UseNumber`; helpers stay unexported;
   the whole thing is one file, no registry, no interface.
2. Table tests: array-of-objects scores over flat object; envelope shape
   (`{data: [...]}`) recognized; scalar JSON skipped; non-2xx skipped;
   `json.Valid` failure skipped; same-host +2 beats equal-shape cross-host;
   denylist host (google-analytics URL with perfect array shape) dropped;
   path hint +1; ≥512 B +1; tie-break order score→items→URL; `max` truncates;
   `keys` capped at 12 and taken from the first object; empty/nil caps → nil.

**Sanity check:** `go test ./scrape/ -run Suggest -race`

### Task BB.3 — scrape side: options, plumbing, result field (1h)

**TDD: extend `scrape/scrape_test.go` first (RED).**

1. `scrape.Options.SuggestAPI bool` (documented comment beside CaptureXHR).
2. `ValidateOptions` (:161-169): new arm beside the capture-xhr one —
   `o.SuggestAPI && render == "static"` → `OptionsError{"scrape: suggest-api
   requires browser rendering (render auto|browser, not static)"}`.
3. `fetchURL` (:463): condition gains `|| o.SuggestAPI`; the fetch call sites
   pass `SuggestAPI: o.SuggestAPI` into `FetchRequest` (mirror how CaptureXHR
   reaches the fetcher — grep it and touch each site additively).
4. After the fetch, where `XHR: page.XHR` lands in the result (:390 region):
   - patterns set → `Result.XHR` = captures filtered by pattern
     (compile once via `fetch.ValidateXHRPatterns`, reuse the validated
     regexps from validation when possible);
   - `o.SuggestAPI` → `Result.SuggestedEndpoints =
     SuggestEndpoints(rawURL, allCaptures, 5)`.
5. Tests: static rejection (exit-2 class); suggest-only run leaves `XHR` nil
   and populates `SuggestedEndpoints`; combined run filters `XHR` by pattern
   while suggestions see everything; flag absent → both fields nil.

**Sanity check:** `go test ./scrape/ -race`

### Task BB.4 — CLI surfacing (1.5h)

**TDD: extend `cli/scrape_test.go` first (RED).**

1. `cli/scrape.go`: `--suggest-api` bool flag beside `--capture-xhr` (:85),
   help: "rank XHR/fetch responses and suggest the JSON API endpoints behind
   the page (browser rendering only)". Wire into all three option
   constructions (:60, :158, :195 region) the way `captureXHR` is.
2. Page-format json: thread `res.SuggestedEndpoints` into the envelope call
   site (:231-234); `xhrJSONEnvelope` (or its caller) sets
   `env["suggested_api"]` only when non-empty. Absent flag → **byte-identical
   output** — pin with the Phase-G style test (marshal with and without,
   compare bytes).
3. Markdown doc (`markdownDoc` :283): add the suggestions as a short
   "Suggested API endpoints" section (url — score — items — first keys),
   only when non-empty.
4. Browser-tagged end-to-end proof (`//go:build browser`, copy the harness
   from `fetch/xhr_browser_test.go` / `actions_browser_test.go`): httptest
   SPA fixture whose page JS fires `fetch('/api/items')`; assert the scrape
   result's top suggestion is that URL with the right item count.

**Sanity check:** `go run . scrape https://quotes.toscrape.com --suggest-api --page-format json | head -20` (server-rendered page → `suggested_api` simply absent; proves the quiet path)

### Task BB.5 — docs + full gates (0.5h)

- README scrape section: flag row + one example (SPA → suggested_api →
  re-run against the endpoint); one sentence: "suggestions are observations —
  magpie never fetches them for you".
- Gates: `go build ./... && go vet ./... && golangci-lint run ./... &&
  gofmt -l .` (print nothing) + `go test ./fetch/ ./scrape/ ./cli/ -race` +
  full `go test ./...` within budget + `go test -tags browser ./fetch/
  ./scrape/ -run Suggest` (Chrome present).
- Manual live smoke (criterion 3) → paste output into the PR description.

---

## Deliverables

```
magpie/
├── fetch/
│   ├── fetcher.go            # +1 field: FetchRequest.SuggestAPI
│   ├── xhr.go                # xhrCollector.all + admit() short-circuit
│   ├── xhr_test.go           # all-mode admit/cap/eviction tests
│   └── xhr_suggest_browser_test.go  # -tags browser SPA fixture e2e
├── scrape/
│   ├── scrape.go             # Options.SuggestAPI, ValidateOptions arm, fetchURL, result fields
│   ├── suggest.go            # EndpointSuggestion + SuggestEndpoints ranker (pure)
│   └── suggest_test.go       # table-driven ranker tests
├── cli/
│   └── scrape.go             # --suggest-api flag, suggested_api envelope key, markdown section
└── README.md                 # flag row + example
```

---

## Exit Criteria

- [ ] `go test ./fetch/ -run XHR -race` — all-mode admits XHR/Fetch, nil-pattern non-all mode admits nothing (today pinned), 50-cap holds, eviction metadata-only entries unchanged
- [ ] `go test ./scrape/ -run Suggest -race` — ranker table green: shape scoring, envelope detection, same-host, denylist drop, path/size hints, deterministic tie-break, max truncation, nil-safety
- [ ] `go test ./scrape/ -race` — static+suggest → OptionsError; suggest-only leaves XHR nil; combined run filters XHR but suggests over all; flag absent → both fields nil
- [ ] `go test ./cli/ -race` — `suggested_api` key present with the flag; **byte-identical** page-json and markdown without it; `--render static --suggest-api` exits 2 pre-I/O
- [ ] `go test -tags browser ./fetch/ -run Suggest` — httptest SPA fixture's `/api/items` surfaces as top suggestion with correct item count
- [ ] `go run . scrape <real-spa> --suggest-api --page-format json` → plausible `suggested_api` output recorded in the PR description
- [ ] `go build ./... && go vet ./... && golangci-lint run ./...` clean; `gofmt -l .` prints nothing; `go test ./...` within budget
- [ ] README documents `--suggest-api` (row + example + "never fetched for you" note)

---

## Execution Prompt

Copy everything between the `---` lines into a new pi session (run from `~/projects/magpie`):

---
You are building Phase BB of magpie — API endpoint suggestions (`--suggest-api`).

### What This Project Is
magpie (`~/projects/magpie`, module `github.com/motherlodelab/magpie`, Go 1.26+)
is a Go CLI web scraper: fetch → clean → extract. Read `AGENTS.md`,
`.pi/rules/go.md`, and `.pi/rules/testing.md` first — they are binding. Lazy
senior-dev standard: smallest diff that works, no new deps (stdlib + go.mod
only — this phase needs NOTHING new: encoding/json, net/url, sort, regexp are
all in go.mod's reach), errors = package-prefixed OptionsError at the options
boundary, tests hermetic (httptest, no live network; browser tests gated
`//go:build browser`), touched packages pass `-race`. Mark deliberate
shortcuts with a `ponytail:` comment.

### Established Facts (verified against the tree @ 29b6218 — do not re-derive)
- `scrape.Options.CaptureXHR []string` (`scrape/scrape.go:77-79`); validated in
  `ValidateOptions` at `:161-169` (render=static rejected + patterns compiled
  via `fetch.ValidateXHRPatterns`).
- `scrape.Result.XHR []fetch.XHRCapture` (`:118-120`, omitempty); populated at
  the result build site near `:390` (`XHR: page.XHR`).
- `fetchURL` (`:463`): browser path forced when
  `render == "browser" || len(o.Actions) > 0 || len(o.CaptureXHR) > 0`;
  auto-escalation via `fetch.ScoreJSRequired`/`NeedsBrowser` below it.
- `fetch.FetchRequest.CaptureXHR []string` (`fetch/fetcher.go:33-36`);
  `FetchResponse.XHR []XHRCapture` (`:54`).
- The ONLY collector construction site: `fetch/actions.go:137-141` in
  `RodFetcher.FetchWithActions` — `caps = &xhrCollector{patterns: patterns}`
  when `len(req.CaptureXHR) > 0`; subscribe before navigation, drain AFTER the
  settle and BEFORE `page.Close()` (`:159`).
- `fetch/xhr.go`: caps 50 captures × 64 KiB bodies (`:13-16`);
  `XHRCapture{URL, Status, MIMEType, Body, Truncated}` with explicit json tags
  (`:24-30`); `admit` (`:68-74`) = resource-type XHR/Fetch + `matchesAny`
  patterns (`:148-155`, nil-safe → false); drain returns metadata-only entries
  for evicted bodies (fail-visible, never fatal).
- `cli/scrape.go`: flag defined at `:85`, wired at `:60/:158/:195`;
  page-json envelope injection at `:231-234` via `xhrJSONEnvelope`
  (`:291-306`, `json.Decoder.UseNumber` re-marshal — keep that discipline for
  the added key); `markdownDoc` at `:283-287`. Phase-G precedent: output
  without capture-xhr is byte-identical — there is an asserting test; extend
  that pattern for the absent-flag case.
- Browser tests live under `//go:build browser` (e.g. `fetch/xhr_browser_test.go`,
  `fetch/actions_browser_test.go`) — copy their harness for the SPA fixture.
- Core gates: `go build ./... && go vet ./... && golangci-lint run ./... &&
  gofmt -l .` all clean + `go test ./...` hermetic and fast.

### Data Model Rules
- Plain structs only, no frameworks, no interfaces with one implementation.
- DTO crossing JSON output: flat struct with explicit json tags (copy
  XHRCapture's style). `EndpointSuggestion{URL string `json:"url"`;
  Score int `json:"score"`; Items int `json:"items,omitempty"`;
  Keys []string `json:"keys,omitempty"`; Reasons []string `json:"reasons,omitempty"`}`.
- One bool crosses the Fetcher interface (`FetchRequest.SuggestAPI`); the
  fetcher never learns why.
- The ranker is ONE pure function in `scrape/suggest.go` — unexported helpers,
  no registry, no strategy pattern.

### Scoring Spec (implement exactly; one-line comments in the file)
skip: status outside 2xx; empty Body; `!json.Valid(Body)`;
denylist hosts (fixed slice: google-analytics, doubleclick, googletagmanager,
facebook, segment, sentry, hotjar, amplitude, mixpanel, clarity — match on
host suffix of the capture URL);
+3: top-level `[]any` with ≥1 object element, OR top-level object with a
top-level field that is an `[]any` with ≥1 object element (envelope; record
items = that array's length);
+0: top-level object of scalars only; skip: scalar JSON;
+2: capture host == target host;
+1: path contains `/api`, `/v1`, `/v2`, `/v3`, or ends `.json`
    (cross-host captures are eligible ONLY with this hint);
+1: len(Body) ≥ 512;
items: array length (top-level or envelope array);
keys: first object's keys, capped at 12;
reasons: short strings, e.g. "array of 42 objects", "same host",
"path hint /api/", "envelope data[]";
sort: score desc → items desc → URL asc; truncate to max (callers pass 5);
nil/empty caps → nil result.

### Files to Touch
- `fetch/fetcher.go` — `FetchRequest.SuggestAPI bool` beside CaptureXHR (documented).
- `fetch/xhr.go` — `xhrCollector.all bool`; admit: resource-type check, then
  `if c.all { return true }`, then matchesAny.
- `fetch/actions.go` — build the collector when
  `len(req.CaptureXHR) > 0 || req.SuggestAPI` with `all: req.SuggestAPI`.
- `scrape/suggest.go` (NEW) — DTO + `SuggestEndpoints(targetURL string,
  caps []fetch.XHRCapture, max int) []EndpointSuggestion`.
- `scrape/scrape.go` — `Options.SuggestAPI`; ValidateOptions arm (static →
  `OptionsError{"scrape: suggest-api requires browser rendering (render
  auto|browser, not static)"}`); fetchURL condition `|| o.SuggestAPI`;
  pass `SuggestAPI` into every FetchRequest construction (grep CaptureXHR and
  mirror each site); at the result build site: with patterns set, filter
  Result.XHR by the validated regexps (semantics unchanged); when SuggestAPI,
  set Result.SuggestedEndpoints = SuggestEndpoints(rawURL, allCaptures, 5).
  Result field: `SuggestedEndpoints []EndpointSuggestion` with
  `json:",omitempty"`, placed after XHR.
- `cli/scrape.go` — `--suggest-api` bool flag + wiring (all three option
  constructions); thread `res.SuggestedEndpoints` into the page-json envelope
  as key `suggested_api` (only when non-empty; keep the UseNumber discipline);
  markdown doc section "Suggested API endpoints" (only when non-empty).
- `fetch/xhr_test.go`, `scrape/suggest_test.go`, `scrape/scrape_test.go`,
  `cli/scrape_test.go` — TDD first (RED), then implement.
- `fetch/xhr_suggest_browser_test.go` (NEW, `//go:build browser`) — httptest
  SPA fixture firing `fetch('/api/items')`; top suggestion asserted.
- `README.md` — scrape flag row + example + "suggestions are observations;
  magpie never fetches them for you".

### Hard Requirements (Exit Criteria)
1. All tests listed in the phase plan pass (`-race`), browser-tagged test
   passes with Chrome.
2. Without `--suggest-api`, page-json and markdown outputs are byte-identical
   to before the change (asserting test, not a claim).
3. `--render static --suggest-api` → exit 2 pre-I/O, zero fetches.
4. Combined `--suggest-api --capture-xhr '<re>'`: `xhr` = pattern matches
   only, suggestions computed over everything.
5. Gates clean: build/vet/lint/gofmt + full hermetic `go test ./...` in budget.

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available — the whole phase builds on the shipped `--capture-xhr` collector; every wiring site was verified against core HEAD `29b6218` with line anchors (scrape.go :77/:118/:161/:390/:463, fetcher.go :33/:54, actions.go :137-141/:159, xhr.go :13/:24/:68/:148, cli/scrape.go :60/:85/:231/:283/:291).
- [PASS] Every sub-task has a clear, testable completion condition — each task ends in a sanity-check command; Exit Criteria map to observable test/CLI behavior.
- [PASS] Execution prompt is self-contained — (a) established facts with verified anchors, (b) no new libraries so no snippets needed (stdlib only), (c) Data Model Rules + exact scoring spec, (d) per-file guidance, (e) hard requirements restated.
- [PASS] Exit criteria map 1:1 to deliverables — every touched file has at least one asserting criterion; the byte-identical-absent case is pinned by test, not prose.
- [PASS] Heavy external dependency strategy — none: no new deps, no LLM calls; the only optional external is Chrome for the `-tags browser` test, which follows the existing gated-suite convention and is not on the hermetic path.
- [PASS] New libraries have confirmed usage snippets — N/A (stdlib `encoding/json` / `sort` / `regexp` only; all already in use in this tree).
- [note] Operator words needed: none for building (branch + commit are local); the eventual public PR/merge/tag follows the standard cross-repo path and is outside this phase.
