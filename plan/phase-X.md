# Phase X — Offline benchmark harness (webclaw-style)

**Duration:** 1 day (~5.5 hours)
**Depends on:** Nothing (master @ fee58a8; uses existing `testdata/clean` + `testdata/quality` fixtures and the `clean` package as-is)
**Blocks:** Nothing technical. Feeds marketing docs (README numbers, desktop positioning) with reproducible quality evidence. Follow-up candidate (not this phase): adding `testdata/vertical` rows to the corpus.
**Risk Level:** LOW — test-only package + docs; no product code, no API changes, no new deps. Only real hazard is metric floors set so tight they flake in CI, mitigated by the measure-then-freeze ordering in Task X.2.
**Stack:** go
**Runner:** manual (the `run-phase` skill hard-blocks on `stack: go` — execute via the execution prompt below)

Source item: `plan/competitive-analysis-2026-09-20.md` item 4; gap row in `plan/webclaw-gap-spec.md` §1 ("Extraction benchmarks — add word-count + timing assertions"). Style reference: webclaw's `benchmarks/` (README headline + methodology.md + facts-as-data + committed `results/YYYY-MM-DD.json`), read from source 2026-09-24.

---

## 1. Objective + What Success Looks Like

Build a test-only `benchmarks/` package that runs the real clean pipeline over the
existing fixture corpus and asserts **metric floors** (word-count retention bands,
fact fidelity, reduction, timing ceilings) that are *independent of the goldens* —
so quality regressions can't be silently absorbed by `go test ./clean/ -update`.
Then publish the measured numbers in a `benchmarks/` doc pair (README + methodology)
with a committed results JSON, webclaw-style.

What success looks like (all observable):

1. `go test ./benchmarks/ -v` exits 0: every content fixture passes its frozen word
   band, 100% of curated facts are found in both markdown and llm output, reduction
   floors hold, quality fixtures classify as expected and stay <50 words.
2. `go test ./benchmarks/ -bench . -run XXX` prints `BenchmarkClean/<fixture>` and
   `BenchmarkLLM/<fixture>` rows with ns/op and a reported `words/sec` metric.
3. `go test ./benchmarks/ -write-results=benchmarks/results/<date>.json` writes a
   valid JSON file matching the schema in §2 (loads with `go test`-side
   `json.Valid` or a one-off `jq .` check).
4. `benchmarks/README.md` and `benchmarks/methodology.md` exist, and every number
   in the README table matches the committed `results/<date>.json`.
5. Full gates stay green: `go test ./...`, `go vet ./...`, `gofmt -l .` empty,
   `golangci-lint run ./...`; `CGO_ENABLED=0 go build ./...` unchanged.
6. `AGENTS.md` structure tree and spec §13 structure mention `benchmarks/`
   (one line each).

**Good:** "article: 155 md words (band 120–200), 5/5 facts, llm 118% chars of md, clean 4.1ms"
**Bad:** "benchmarks look fine"

## 2. Key Design Decisions

No ASCII diagram needed: it's one test-only package reading fixtures and calling
`clean`. The decisions that matter:

| Decision | Choice | Why |
|---|---|---|
| Placement | New `benchmarks/` dir at repo root, **only** `*_test.go` files | `go build ./...` ignores test-only packages → zero product surface, zero API freeze; `go test ./...` picks it up like any package. Precedent for flags-in-tests: `clean/clean_test.go -update`. |
| Metric reference | Fixture HTML as denominator, curated facts + frozen bands as floor — **never the goldens** | Goldens can be regenerated past a regression with `-update`; floors survive regeneration. This is the entire "harder than goldens alone" point. |
| Token proxy | `len(s)/4` chars-as-tokens, same convention as `capTokens` and the `TestLLMText_Reduction` comment | No tokenizer dep (banned without asking); consistent with in-repo convention. Documented honestly in methodology.md. Words reported alongside via `clean.WordCount`. |
| Fidelity metric | webclaw's curated-facts match: case-insensitive, `\b` word boundary for single alphanumeric tokens | Simple, explainable, data-driven. `API` must not match `apiece`. |
| Reduction floor | `len(llm) < len(md)` on **article only** — NOT a "≤50%" assert | Measured 2026-09-24 probe on master: article 1042→945 chars (shrinks ~9%); product 440→931 **grows 2.1×** (metadata header + gated structured data outweigh the 70-word body); spa-shell 41→54. This is why `llm_test.go` binds reduction on article only. Report per-row ratios in the doc; assert only the measured contract. |
| Timing | Informational `b.ReportMetric` benchmarks + one **generous** ceiling assert (clean+llm < 1s per fixture, wall clock) | Ceiling catches 3.2s/page-class pathological regressions (see Phase J lesson) without CI flake risk — normal fixture cleans are ~5–20ms. |
| Corpus | `testdata/clean/{article,product,spa-shell}` + `testdata/quality/{challenge-akamai,empty-shell,login-wall,rich-with-marker}` = 7 rows | Item says "fixtures already exist"; this is the clean-quality corpus the item targets. Verticals already have per-extractor asserts — out of scope. |
| Results format | One JSON per run in `benchmarks/results/`, schema below, committed | Webclaw's auditable-history trick; diffable across releases. |

### Data model rules (Go)

Plain structs, table-driven tests — no interfaces, no config files:

```go
type fixture struct {
    name      string   // "article" — file is ../../testdata/<dir>/<name>.html
    dir       string   // "clean" or "quality"
    status    int      // RawPage.StatusCode — Classify is status-dependent (akamai needs 403, login-wall 401)
    band      [2]int   // frozen md word-count floor/ceiling (content rows only)
    facts     []string // curated visible strings (content rows only)
    wantIssue clean.Issue // pinned per row (see anchors below)
    llmShrinks bool    // len(llm) < len(md) floor; true ONLY for article (measured)
}

type result struct { // one row of the results JSON
    Fixture string `json:"fixture"`
    RawWords int `json:"raw_words"`
    MDWords int `json:"md_words"`
    LLMTokens int `json:"llm_tokens"` // len/4 proxy
    MDChars int `json:"md_chars"`
    LLMChars int `json:"llm_chars"`
    FactsKept int `json:"facts_kept"`
    FactsTotal int `json:"facts_total"`
    CleanNS int64 `json:"clean_ns"`
}
```

Results JSON top level: `{generated_at, go_version, commit, aggregates{fidelity_pct, mean_reduction_vs_raw_pct}, per_fixture[]}`. Commit via `runtime/debug.ReadBuildInfo` `vcs.revision`, best-effort (`""` if absent).

**Frozen anchors (probe run 2026-09-24 on master, `StatusCode` as shown):**
article@200 → 120 words, quality `""`; product@200 → 70, `""`; spa-shell@200 → 7,
`""` (body not richer, so the <200-word guard does NOT fire); challenge-akamai@403 →
16, `access-denied`; login-wall@401 → 11, `login-required`; empty-shell@200 → 7,
`""` (reality pin — `TestClassify_Table`'s `IssueEmpty` row feeds Classify a
synthetic 3-word markdown, a different input than the full pipeline produces);
rich-with-marker@403 → 809, `""` (the guard negative: 403 + marker text + rich
content ⇒ no issue). Bands freeze at ±25–30% around these: article [90,150],
product [52,88], spa-shell [4,12], rich [560,1000]; quality rows assert `<50`
words. Keep the `t.Logf` as the regeneration report.

## 3. Tasks

### Task X.1 — Corpus table + loader + fact matcher (~1h)

Create `benchmarks/harness_test.go` (`package benchmarks`, test-only). Corpus
table of the 7 rows above; loader reads `../../testdata/<dir>/<name>.html` (test
binary cwd = package dir). Fact matcher — one small func, no abstraction:

```go
// Confirmed APIs in use (all existing, verified in-tree 2026-09-24):
//   clean.Clean(ctx, clean.RawPage{HTML, URL, StatusCode}) (clean.CleanedPage, error)
//   clean.ToLLMText(p clean.CleanedPage) string
//   clean.WordCount(s string) int   // already drops link targets
//   module path: github.com/motherlodelab/magpie  (go.mod)
func factRe(fact string) *regexp.Regexp {
    q := regexp.QuoteMeta(fact)
    if !strings.ContainsAny(fact, " -") { // single token → word boundary
        q = `\b` + q + `\b`
    }
    return regexp.MustCompile(`(?i)` + q)
}
```

Curate 5 facts per content fixture (article, product) and 3 for spa-shell, per
webclaw's criteria: visibly in fixture body HTML, specific (product/section names,
prices, stats — h1s like "The Widget Price Guide" / "Widget Pro" qualify; "Details"
does not), stable. Facts live in the Go table (they ARE data for a 7-row corpus;
webclaw's facts.json format is the upgrade path if external PRs ever materialize).

**Sanity check:** `go test ./benchmarks/ -run TestFactMatching -v` (tiny table test: "API" vs "apiece" negative).

### Task X.2 — Metric assertions, measure-then-freeze (~1.5h)

`TestCorpus_CleanQuality`: for each row — `clean.Clean` with the row's `status`,
assert `band[0] ≤ clean.WordCount(md) ≤ band[1]` (content rows), every fact
matches md AND `clean.ToLLMText(p)`, `len(llm) < len(md)` (article only —
measured: product's llm output grows), `p.Quality == wantIssue` (all rows),
wall time < 1s; quality rows additionally `WordCount(md) < 50`.

Bands are already frozen from the 2026-09-24 probe (see §2 anchors) — keep the
`t.Logf` as the regeneration report. If a floor fails on master, the floor is
wrong — fix the floor, not the code (this phase ships no product changes).

**Sanity check:** `go test ./benchmarks/ -v` green with bands frozen.

### Task X.3 — Benchmarks + results writer (~1h)

`BenchmarkClean/<fixture>` and `BenchmarkLLM/<fixture>` via `b.Run` over the
content rows, `b.ReportMetric(float64(words)/b.Elapsed().Seconds(), "words/sec")`.
Flag `writeResults = flag.String("write-results", "", ...)` (same pattern as
`-update`): re-runs the corpus measurements, writes the §2 JSON schema, `os.WriteFile`
+ indent. No CLI surface, no cmd/ change.

**Sanity check:** `go test ./benchmarks/ -bench . -run XXX` prints rows;
`go test ./benchmarks/ -run TestCorpus -write-results=/tmp/r.json && jq . /tmp/r.json`.

### Task X.4 — Run, freeze numbers, write docs (~1.5h)

Run the writer on master → `benchmarks/results/2026-09-24.json`. Write:

- `benchmarks/README.md` — headline table (fidelity %, md words vs raw words
  reduction, llm-vs-md chars, per-fixture rows) copied **from the committed JSON**,
  plus repro commands and a link to methodology.
- `benchmarks/methodology.md` — what is measured (words, len/4 token proxy,
  curated facts, bands, generous timing ceiling), fact-selection criteria,
  and an explicit limitations section: offline self-curated fixture corpus,
  NOT an independent web benchmark — do not borrow webclaw's "beats Firecrawl"
  framing; our claims are (a) pinned regression floors, (b) published reproducible
  numbers. (spec.md §969 already warns about vendor-claim inflation — stay honest.)

**Sanity check:** spot-check 3 README numbers against the JSON.

### Task X.5 — Structure lines + full gates (~0.5h)

One line in `AGENTS.md` structure tree (`benchmarks/  # offline quality harness (test-only)`)
and the matching one-liner in the spec §13 structure tree if it lists dirs. Then
all gates: `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...`,
plus `CGO_ENABLED=0 go build ./...`.

Total: ~5.5h.

## 4. Deliverables

```
magpie/
├── benchmarks/
│   ├── harness_test.go       # corpus table, fact matcher, metric assertions, benchmarks, -write-results
│   ├── README.md             # headline numbers + per-fixture table + repro (numbers == committed JSON)
│   ├── methodology.md        # metrics, token proxy, fact criteria, limitations
│   └── results/
│       └── 2026-09-24.json   # first committed run (schema in §2)
├── AGENTS.md                 # +1 structure line
└── spec.md                   # +1 structure line (§13 tree), only if it lists dirs
```

No product code changes. No go.mod changes.

## 5. Exit Criteria

- [ ] `go test ./benchmarks/ -v` exits 0 with frozen bands, 7/7 rows passing (5 content metrics + quality classify/word-cap)
- [ ] `go test ./benchmarks/ -bench . -run XXX` prints BenchmarkClean/BenchmarkLLM rows with ns/op + words/sec
- [ ] `-write-results=...` emits JSON matching the §2 schema; committed `results/2026-09-24.json` present
- [ ] README + methodology exist; README table numbers match the committed JSON
- [ ] Fact matcher negative case pinned: single-token "API" does not match "apiece" (test in harness_test.go)
- [ ] `go test ./...`, `go vet ./...`, `gofmt -l .` (empty), `golangci-lint run ./...` all clean; `CGO_ENABLED=0 go build ./...` succeeds
- [ ] AGENTS.md (+ spec if applicable) structure updated

## 6. Execution Prompt

Copy everything between the `---` lines into a fresh pi session opened in
`~/projects/magpie` (`run-phase` will hard-block on `stack: go` — implement manually):

---

You are building Phase X of magpie — the offline benchmark harness (webclaw-style).

### What This Project Is

magpie is a Go 1.26 CLI web scraper (fetch → clean → extract), module
`github.com/motherlodelab/magpie`, single static binary, CGO-free. Coding ethos:
lazy senior dev — minimum code that works, no new deps without asking, no
abstractions with one implementation. Read `AGENTS.md` first. This phase adds a
test-only `benchmarks/` package that pins extraction-quality metrics over the
existing `testdata/` fixtures and publishes the numbers as docs. It ships NO
product code changes.

### Established in Prior Phases (verified in-tree)

- `clean.Clean(ctx context.Context, raw clean.RawPage) (clean.CleanedPage, error)`;
  `RawPage{HTML []byte, URL, FinalURL string, StatusCode int, ContentType string, ...}`.
  Clean never fails on quality — it attaches `CleanedPage.Quality clean.Issue`
  (`""`/`empty`/`access-denied`/`unavailable`/`login-required`).
- `clean.ToLLMText(cleanedPage) string`, `clean.ToText(md) string`,
  `clean.WordCount(s string) int` (drops link targets — differs from `wc -w`).
- Repo token-proxy convention: tokens ≈ `len(s)/4` (see `capTokens` and the
  `TestLLMText_Reduction` comment in `clean/llm_test.go`). That test binds
  reduction (`len(llm) < len(md)`) on content-scale fixtures only — small shells
  carry a metadata header that outweighs the body. Respect that: your reduction
  floor is also content-rows-only.
- Golden tests use a `-update` flag pattern in `clean/clean_test.go` — precedent
  for flags registered in test files.
- Fixtures: `testdata/clean/{article,product,spa-shell}.html` (+ `.md` goldens),
  `testdata/quality/{challenge-akamai,empty-shell,login-wall,rich-with-marker}.html`.
  Current golden word counts (`wc -w`): article 155, product 70, spa-shell 7.
- CI has no network in the default suite; hermetic tests only. Chrome-gated tests
  use `//go:build browser` — you need none of that.

### Your Goal

A `benchmarks/` directory containing one test-only Go package and two markdown
docs, asserting metric floors that survive golden regeneration, plus a committed
results JSON with today's measured numbers.

### Data Model Rules (Go)

- Plain structs only (see `fixture` and `result` structs in plan §2). No
  interfaces, no config files, no JSON fixture format — the corpus table lives in
  the Go test file.
- Table-driven tests; `t.Run` per fixture.
- One `flag.String("write-results", "", ...)` registered in the test file.

### Architecture

`benchmarks/harness_test.go` (package `benchmarks`, ONLY test files in the dir —
`go build ./...` must not see it) → reads `../../testdata/<dir>/<name>.html` →
`clean.Clean` → asserts floors → Benchmark funcs + `-write-results` emit
`benchmarks/results/<date>.json` → README/methodology quote those numbers.

### Files to Create

#### benchmarks/harness_test.go
Corpus: 7 rows (3 clean content + 4 quality) with per-row `band [2]int`, `facts []string`,
`wantIssue clean.Issue` (from `clean/quality_test.go:47-50`: challenge-akamai→`access-denied`,
login-wall→`login-required`, empty-shell→`empty`, rich-with-marker→`""`). Functions: fixture loader (`os.ReadFile`, paths relative to
package dir), `factRe` (quote-meta; `\b` boundaries only for single tokens;
case-insensitive), `TestFactMatching` (incl. negative: "API" vs "apiece"),
`TestCorpus_CleanQuality` (bands + fidelity in md AND llm + `len(llm) < len(md)`
on content rows + `p.Quality == wantIssue` and `<50` words on quality rows +
per-fixture wall-time ceiling of 1 second — generous on purpose: it catches
3-seconds-per-page-class regressions, not perf tuning; leave a `ponytail:` comment
saying exactly that), `BenchmarkClean`/`BenchmarkLLM` with `b.Run` per content
fixture and `b.ReportMetric(words/sec)`, and the `-write-results` writer emitting:
`{generated_at, go_version, commit, aggregates{fidelity_pct, mean_reduction_vs_raw_pct}, per_fixture[]}`
with per-fixture `raw_words, md_words, llm_tokens, md_chars, llm_chars, facts_kept,
facts_total, clean_ns`. Commit via `runtime/debug.ReadBuildInfo` (vcs.revision,
best-effort). First run: `t.Logf` measured word counts, set bands ±25%, freeze.

#### benchmarks/README.md + benchmarks/methodology.md
README: headline table + per-fixture rows copied from the committed JSON, repro
commands (`go test ./benchmarks/ -v`, `-bench .`, `-write-results=...`). Methodology:
metrics definitions, the len/4 token proxy and why (no tokenizer dep, repo
convention), fact-selection criteria, band-freezing procedure, and a limitations
section: offline, self-curated 7-fixture corpus — NOT an independent web benchmark;
no competitor comparisons. Honest framing only.

#### AGENTS.md / spec.md
One structure-tree line each (`benchmarks/  # offline quality harness (test-only)`);
skip spec.md if its §13 tree doesn't list package dirs.

### Success Criteria

- `go test ./benchmarks/ -v` green (frozen bands, 7/7 rows)
- `go test ./benchmarks/ -bench . -run XXX` prints ns/op + words/sec rows
- `benchmarks/results/2026-09-24.json` committed; README numbers match it
- Full gates: `go test ./... && go vet ./... && gofmt -l .` (empty) `&& golangci-lint run ./...`; `CGO_ENABLED=0 go build ./...`
- Zero changes outside `benchmarks/`, `AGENTS.md`, `spec.md`

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available — fixtures (`testdata/clean/`, `testdata/quality/`) and the `clean` API (`Clean`, `RawPage`, `ToLLMText`, `WordCount`, `-update` flag precedent) verified in-tree 2026-09-24; golden word counts measured (`wc -w`: 155/70/7).
- [PASS] Every sub-task has a clear, testable completion condition — each task ends in a sanity-check command; no task says "investigate".
- [PASS] Execution prompt is self-contained — (a) prior-phase facts inline (API signatures, token-proxy convention, reduction-binds-on-content-rows caveat, module path), (b) confirmed snippets (`factRe`, result structs, JSON schema) from in-tree source rather than web search — no new libraries exist in this phase, so the confirmed-snippet rule is satisfied by in-tree verification, (c) "Data Model Rules (Go)" section present, (d) per-file guidance for all four deliverables, (e) observable success criteria with exact commands.
- [PASS] Exit criteria map 1:1 to deliverables — harness_test.go → criteria 1/2/5; results JSON → 3; README+methodology → 4; AGENTS/spec lines → 7; gates → 6. Nothing in the file tree is untested.
- [PASS] Heavy external dependency strategy — n/a: zero new dependencies (stdlib `testing`/`flag`/`encoding/json`/`regexp`/`os`/`runtime/debug` + existing `clean` package); tokenizer deliberately replaced by the repo's len/4 proxy to avoid a dep.
- [PASS] Known hazard handled — metric floors could flake or wrongly fail; mitigated by measure-then-freeze ordering (Task X.2), ±25% band slack, content-rows-only reduction floor matching `TestLLMText_Reduction`'s documented caveat, and a deliberately generous 1s timing ceiling (`ponytail:` comment required in code).
