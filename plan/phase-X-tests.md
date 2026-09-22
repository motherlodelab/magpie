# Phase X — Testing: Offline benchmark harness (webclaw-style)

**Scope:** the new test-only `benchmarks/` package — corpus table, fact matcher, `TestCorpus_CleanQuality` metric floors, `BenchmarkClean`/`BenchmarkLLM`, `-write-results` JSON writer — plus the docs gates for `benchmarks/README.md`, `benchmarks/methodology.md`, `results/2026-09-24.json`, and the AGENTS/spec structure lines.
**Key Pattern:** **No fakes, no mocks** (validation phase — the real `clean` pipeline over committed fixtures IS the subject). Every assertion runs `clean.Clean` on fixture bytes with a per-row `StatusCode`, then pins measured floors: word bands, curated-fact fidelity (case-insensitive, word-boundary), article-only llm-shrink, exact `Quality` per row, `<50`-word caps on blocked/thin rows, and a deliberately generous 1s wall-time ceiling. Floors are **golden-independent by construction** — the harness never reads a `.md` golden.
**Dependencies:** stdlib only: `testing`, `flag`, `os`, `path/filepath`, `regexp`, `strings`, `time`, `encoding/json`, `runtime/debug` + in-repo `clean` (`Clean`, `RawPage`, `ToLLMText`, `WordCount`, `Issue*` constants). **No new deps; `git diff go.mod` empty.**

**Decisions pinned while verifying seams** (test-plan additions to phase-X.md, discovered by probe-running `clean.Clean` over all 7 fixtures on master, 2026-09-24):

1. **Measured anchors are known — bands freeze NOW, no measure-then-freeze round trip.** Probe output: article@200 → 120 words, `""`; product@200 → 70, `""`; spa-shell@200 → 7, `""`; challenge-akamai@403 → 16, `access-denied`; login-wall@401 → 11, `login-required`; empty-shell@200 → 7, `""`; rich-with-marker@403 → 809, `""`. Frozen bands: article [90,150], product [52,88], spa-shell [4,12], rich [560,1000]; quality rows assert `<50`. (`wc -w` on the goldens said 155/70/7 — article's real count is 120 because `clean.WordCount` drops link targets; the goldens were never the oracle.)
2. **The llm-shrink floor holds for article ONLY.** article md 1042 → llm 945 chars (~9% shrink); product 440 → 931 (**grows 2.1×** — the metadata header + gated structured data outweigh a 70-word body); spa-shell 41 → 54. This retro-explains why `TestLLMText_Reduction` (clean/llm_test.go) binds on article alone. Assert `len(llm) < len(md)` via a per-row `llmShrinks bool`; report ratios for every row in the results JSON/doc, assert nothing unmeasured.
3. **`empty-shell` classifies `""` through the real pipeline** — not `empty`. `TestClassify_Table` (clean/quality_test.go:50) feeds `Classify` a synthetic 3-word markdown, so its body IS richer; the pipeline's extraction of the same fixture (7 words) is not. The corpus pins **pipeline reality**; the synthetic-input taxonomy stays pinned where it lives (TestClassify_Table). Do not "fix" either to match the other — different inputs.
4. **`StatusCode` is a required corpus field.** `Classify` is status-dependent: akamai yields `access-denied` only at 403, login-wall only at 401. `RawPage.StatusCode` flows through `Clean` → `CleanedPage.Quality`.
5. **rich-with-marker runs at 403** — it doubles as (a) the `<200`-word guard negative (403 + "Just a moment" marker text + 809 rich words ⇒ `""`, the exact A4 false-positive regression) and (b) a fourth content row for bands/facts.
6. **Timing ceiling is 1s per fixture for clean + ToLLMText combined** — measured cost is single-digit ms, so the ceiling can never flake in CI, but it trips the "3.2s/page hidden-Chrome launch" class of regression (Phase J lesson). `ponytail:` comment in code names this ceiling. `Benchmark*` funcs report honest ns/op + words/sec separately; the ceiling is not a perf claim.
7. **Docs + results JSON are verified by commands, not tests.** A test asserting the committed `results/2026-09-24.json` would just be a second golden — the opposite of this phase's point. README↔JSON agreement is a grep/jq spot-check in the run commands.

---

## User Stories

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | As a maintainer regenerating goldens after a formatting change, I want quality floors that `-update` cannot silently absorb, so content loss is caught with numbers even when goldens are rewritten | `TestCorpus_CleanQuality` content rows + `grep -c golden benchmarks/harness_test.go` | All 7 rows green on master; the harness source contains **zero** references to golden files (floors derive from fixture HTML + frozen bands only); a dropped fact or shrunken word count fails with `fixture: metric=value, want band/fact` |
| US-2 | As a dev comparing extractors via `benchmarks/README.md`, I want the published fidelity/precision numbers reproducible from the committed results JSON with one command | `-write-results` run + `jq` schema check + README spot-check | `go test ./benchmarks/ -run TestCorpus -write-results=…` emits JSON with `generated_at/go_version/commit/aggregates{fidelity_pct, mean_reduction_vs_raw_pct}/per_fixture[7]`; committed `results/2026-09-24.json` parses with `jq .`; 3 spot-checked README numbers equal the JSON's |
| US-3 | As an agent author, I want the corpus to prove blocked/thin pages stay thin (`quality` typed, <50 words) so the "typed errors, never garbage markdown" claim is measured, not vibes | `TestCorpus_CleanQuality` quality rows | akamai@403 → `access-denied`, login-wall@401 → `login-required`, empty-shell@200 → `""` + 7 words, all three `<50` words (measured 16/11/7) |
| US-4 | As the A4 quality-gate owner, I want the "rich page mentioning a challenge marker under a 403" false-positive pin at corpus level, so no future Classify change can regress the guard | rich-with-marker row assertions | 809 words (band [560,1000]), `Quality == ""`, facts preserved — **while StatusCode is 403** |
| US-5 | As CI, I want a tripwire for pathological per-page cost (hidden browser launch, O(n²) scan) without flake risk, plus visible perf numbers | 1s ceiling assertions + `go test ./benchmarks/ -bench . -benchmem` output | All rows < 1s wall (clean+llm combined); benchmark output prints `BenchmarkClean/<fixture>` and `BenchmarkLLM/<fixture>` with ns/op and a `words/sec` `b.ReportMetric` |

---

## 1. Component Mock Strategy

Phase type: **validation** (the harness IS the test; zero network, zero browser, zero new deps in every tier). Mock strategy in one sentence: **no component is mocked — each corpus row drives the real `clean.Clean` → `ToLLMText` pipeline against committed fixture bytes, and every "expected" value is either a frozen measurement from the 2026-09-24 probe or a curated fact from the fixture's visible HTML.**

| Component | Mock Strategy | What to Assert | User Story |
|-----------|--------------|----------------|------------|
| `article` content row | Real pipeline, `status:200`; band + 5 facts | words ∈ [90,150] (measured 120); 5/5 facts in md **and** llm; `llmShrinks` → `len(llm)=945 < len(md)=1042`; `Quality == ""`; wall < 1s | US-1, US-2 |
| `product` content row | Real pipeline, `status:200`; band + 5 facts, **no shrink assert** | words ∈ [52,88] (measured 70); 5/5 facts both outputs; `Quality == ""`; llm/md char ratio *reported* (931/440) never asserted | US-1, US-2 |
| `spa-shell` thin row | Real pipeline, `status:200`; band + 1 fact | words ∈ [4,12] (measured 7 — islands hook must NOT fire on it, islands_test.go:203 pins 7 too); fact `"Please enable JavaScript to use this app."` present; `Quality == ""` (guard does not fire: body not richer) | US-1, US-3 |
| `challenge-akamai` quality row | Real pipeline, `status:403` | `Quality == access-denied`; words < 50 (measured 16) | US-3 |
| `login-wall` quality row | Real pipeline, `status:401` | `Quality == login-required`; words < 50 (measured 11) | US-3 |
| `empty-shell` quality row | Real pipeline, `status:200` | `Quality == ""` (reality pin, decision 3); words < 50 (measured 7) | US-3 |
| `rich-with-marker` guard row | Real pipeline, `status:403`; band + 5 facts | `Quality == ""` **at 403** (A4 guard); words ∈ [560,1000] (measured 809); 5/5 facts | US-4 |
| Fact matcher `factRe` | Table test, no pipeline | `"API"` regex does **not** match `"apiece"`; multi-word fact matches case-insensitively; regex metachars in facts are quoted (`regexp.QuoteMeta`) | US-2 |
| `BenchmarkClean`/`BenchmarkLLM` | Real pipeline in `b.Run` loop, `b.ReportMetric` | Rows exist per content fixture; `words/sec` metric printed; allocs via `-benchmem` (informational) | US-5 |
| Timing ceiling | `time.Since` around Clean+ToLLMText per row | Every row < 1s wall; `ponytail:` comment names the ceiling + upgrade path | US-5 |
| Results writer (`-write-results`) | Flag-gated run over the same corpus | JSON has exactly: `generated_at`, `go_version` (`runtime.Version()`), `commit` (`debug.ReadBuildInfo` vcs.revision, `""` fallback), `aggregates{fidelity_pct, mean_reduction_vs_raw_pct}`, `per_fixture` length 7 with `fixture/raw_words/md_words/llm_tokens/md_chars/llm_chars/facts_kept/facts_total/clean_ns` | US-2 |
| Docs gates (README, methodology, structure lines) | grep/jq commands, NOT tests | README numbers match committed JSON (3 spot-checks); methodology contains the len/4 token-proxy section and the limitations section; `grep benchmarks/ AGENTS.md` ≥ 1 | US-2 |

---

## 2. Test Tier Table

| Tier | Dependencies | Speed | When to Run |
|------|-------------|-------|-------------|
| Default (`go test ./benchmarks/`) | Real `clean` + committed fixtures; **no network, no browser, no build tags, no fakes** | <2s (7 pipeline runs + matcher table) | Every push; the only CI gate (rides `go test ./...`) |
| Benchmarks (manual) | Same, plus `-bench` timers | ~5s | Before publishing doc numbers; after any `clean/` perf-relevant change |
| Docs/release gate (manual commands) | Committed `results/*.json` + `jq` + `grep` | seconds | When regenerating results JSON or touching README/methodology |

No live/network tier exists in this phase — that is the point of "offline harness".

## 3. No Fake Implementations (Validation Phase)

This phase IS the test: its subject is the real `clean` pipeline's measured behavior on committed fixtures. Faking trafilatura/goquery would make every metric self-referential (the fake would define the numbers), and the only external inputs — fixture HTML files — are already committed bytes, cheaper than any fake. The single "oracle" inputs are the frozen probe measurements in decision 1 and the curated fact strings; both are data in the corpus table, not code to stub.

## 4. Test File List

```
magpie/
├── benchmarks/
│   ├── harness_test.go        # corpus table (7 rows w/ status), loader, factRe + TestFactMatching,
│   │                          # TestCorpus_CleanQuality (all floors, US-1..US-5), BenchmarkClean/LLM,
│   │                          # -write-results writer. ONLY file in the dir → go build ./... ignores it
│   ├── README.md              # headline + per-fixture tables copied from results JSON; repro commands
│   ├── methodology.md         # metrics defs, len/4 token proxy, fact criteria, limitations (no competitor claims)
│   └── results/
│       └── 2026-09-24.json    # committed first run (writer output, schema in §1 writer row)
├── AGENTS.md                  # +1 structure line: benchmarks/ # offline quality harness (test-only)
└── spec.md                    # +1 structure line, only if §13 tree lists package dirs
```

No changes to `clean/`, `go.mod`, or any existing test. The harness must not import the goldens.

## 5. Test Bootstrap (Go — no conftest)

Go has no conftest; all shared state lives at the top of `benchmarks/harness_test.go` (`package benchmarks` — test-only dir). The `-write-results` flag registers via the stdlib `flag` package, same pattern as `clean/clean_test.go`'s `-update`; `go test ./benchmarks/ -write-results=…` forwards it to the test binary.

```go
var writeResults = flag.String("write-results", "", "write run JSON to this path (results/YYYY-MM-DD.json)")

type fixture struct {
    name, dir  string
    status     int         // RawPage.StatusCode — Classify is status-dependent
    band       [2]int      // frozen md word floor/ceiling; {0,0} ⇒ assert <50 cap only
    facts      []string    // curated visible strings from fixture HTML
    wantIssue  clean.Issue
    llmShrinks bool        // measured; true only for article (decision 2)
}

var corpus = []fixture{
    {"article", "clean", 200, [2]int{90, 150}, nil, "", true},   // facts curated at build time (5)
    {"product", "clean", 200, [2]int{52, 88}, nil, "", false},   // 5 facts
    {"spa-shell", "clean", 200, [2]int{4, 12}, nil, "", false},  // 1 fact
    {"challenge-akamai", "quality", 403, [2]int{}, nil, clean.IssueAccessDenied, false},
    {"login-wall", "quality", 401, [2]int{}, nil, clean.IssueLoginRequired, false},
    {"empty-shell", "quality", 200, [2]int{}, nil, "", false},
    {"rich-with-marker", "quality", 403, [2]int{560, 1000}, nil, "", true}, // facts + shrink optional; measure first
}
```

Helpers (all ~5 lines, all in this one file): `loadHTML(t, dir, name)` reading `../../testdata/<dir>/<name>.html`; `factRe(f string) *regexp.Regexp` (QuoteMeta; `\b` boundaries only for single tokens; `(?i)`); `runRow(t, f) (cleanedPage, llm string, elapsed time.Duration)` used by both the test and the benchmarks; `result` struct for the JSON row. Facts are filled into the table during implementation after reading the fixture bodies (criteria: visible in rendered body, specific, stable — h1s like "The Widget Price Guide"/"Widget Pro" qualify, "Details" does not).

## 6. Key Testing Decisions

| Decision | Approach | Rationale |
|----------|----------|-----------|
| Floors never read goldens | Harness loads only `.html` fixtures; `grep -c golden harness_test.go` must be 0 | Golden-independent floors are the entire point (US-1); `-update` regeneration cannot absorb a regression past them |
| Bands frozen from probe, not `wc -w` | Measured 120/70/7/809 ≠ golden `wc -w` 155/70/7 | `clean.WordCount` drops link targets; the golden file was never the oracle |
| llm-shrink asserted on article only | `llmShrinks` per-row bool | Measured: product grows 2.1×; asserting shrink there would fail on master. Report ratios, assert only measured contracts |
| Pipeline reality over synthetic taxonomy | empty-shell pins `""` via Clean, `IssueEmpty` stays in TestClassify_Table | `TestClassify_Table` feeds synthetic 3-word markdown — a different input. Pinning `empty` in both places would make one of them lie |
| Quality rows carry real StatusCode | `status` field per row (403/401/200) | Classify is status-dependent; a 200 akamai row would pin the wrong world |
| Timing ceiling at 1s, benchmarks separate | Ceiling inside TestCorpus; honest ns/op in Benchmark funcs | Ceiling = regression tripwire (Phase J class), never a perf claim; generous so CI never flakes |
| Docs verified by commands | jq/grep in run commands, no doc-reading test | A test on the committed JSON is a second golden — anti-goal |
| No `t.Parallel()` | Sequential rows | Timing ceiling + shared fixture reads; parallelism buys nothing at 7 rows and noisy-floats the ceiling |

## 7. Example Test Case

The core loop of `TestCorpus_CleanQuality` (representative: table-driven, per-row subtests, all five metric kinds, fail message carries the number):

```go
func TestCorpus_CleanQuality(t *testing.T) {
	for _, f := range corpus {
		t.Run(f.name, func(t *testing.T) {
			start := time.Now()
			p, llm, err := runRow(t, f) // clean.Clean + ToLLMText; fails on Clean error
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Clean: %v", err)
			}
			if p.Quality != f.wantIssue {
				t.Errorf("Quality = %q, want %q", p.Quality, f.wantIssue)
			}
			words := clean.WordCount(p.Markdown)
			t.Logf("%s: words=%d md=%dB llm=%dB quality=%q %s", f.name, words, len(p.Markdown), len(llm), p.Quality, elapsed)
			if f.band != [2]int{} {
				if words < f.band[0] || words > f.band[1] {
					t.Errorf("words=%d outside frozen band %v", words, f.band)
				}
			} else if words >= 50 {
				t.Errorf("blocked/thin row kept %d words, want <50", words)
			}
			for _, fact := range f.facts {
				if !factRe(fact).MatchString(p.Markdown) {
					t.Errorf("fact %q missing from markdown", fact)
				}
				if !factRe(fact).MatchString(llm) {
					t.Errorf("fact %q missing from llm text", fact)
				}
			}
			if f.llmShrinks && len(llm) >= len(p.Markdown) {
				t.Errorf("llm=%dB >= md=%dB, want shrink", len(llm), len(p.Markdown))
			}
			// ponytail: 1s ceiling is a regression tripwire (hidden browser launch /
			// quadratic scan class), not a perf claim — measured cost is ~10ms.
			// Upgrade path: move to Benchmark-only timing if fixtures grow >100×.
			if elapsed > time.Second {
				t.Errorf("clean+llm took %s, want <1s — pathological regression?", elapsed)
			}
		})
	}
}

func TestFactMatching(t *testing.T) {
	if factRe("API").MatchString("the apiece marker") {
		t.Error(`single-token fact "API" must not match "apiece" (word boundary)`)
	}
	if !factRe("Widget Pro").MatchString(" Meet the WIDGET PRO suite") {
		t.Error("multi-word fact must match case-insensitively")
	}
	if factRe("a.b (v2)").MatchString("a X b (v2)") { // QuoteMeta: '.' and parens literal
		t.Error("regex metachars in facts must be quoted")
	}
}
```

## 8. Execution Prompt

Copy everything between the `---` lines into a fresh pi session opened in `~/projects/magpie`:

---

You are writing the test suite for Phase X of magpie — the offline benchmark harness. The phase plan is `plan/phase-X.md`; this prompt is self-contained but the plan's §2 anchors table is canonical.

### What This Project Is

magpie is a Go 1.26 CLI web scraper (module `github.com/motherlodelab/magpie`, CGO-free). Ethos: lazy senior dev — minimum code, no new deps, no abstractions with one implementation. Read `AGENTS.md` first. This phase adds ONLY a test-only `benchmarks/` package + docs. It ships zero product code changes; `go build ./...` must not see the new dir.

### Acceptance Criteria (from User Stories)

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | Golden regeneration cannot silently absorb content loss | `TestCorpus_CleanQuality` + `grep -c golden benchmarks/harness_test.go` | 7/7 rows green on master; zero golden references in harness source |
| US-2 | Published numbers reproducible from committed JSON | `-write-results` + jq + README spot-check | JSON schema exact (aggregates + per_fixture[7]); 3 README numbers == JSON |
| US-3 | Blocked/thin pages stay thin, typed | quality rows | akamai@403 `access-denied`, login-wall@401 `login-required`, empty-shell@200 `""`, all <50 words |
| US-4 | A4 guard false-positive pinned at corpus level | rich-with-marker@403 row | 809-word band [560,1000], `Quality == ""` at 403 |
| US-5 | Pathological-cost tripwire + visible perf numbers | 1s ceilings + `-bench` output | all rows <1s; ns/op + words/sec printed |

### Why No Fakes

Validation phase — the real `clean` pipeline over committed fixture bytes IS the subject; a fake would define the metrics it claims to measure. All inputs are local files; no network, no browser, no build tags.

### What NOT to Test

- Don't test trafilatura/goquery themselves — `clean/*_test.go` already pins their behavior; our floor tests treat `Clean` as the unit.
- Don't re-pin `Classify`'s synthetic-input taxonomy (`TestClassify_Table` owns it) — the corpus pins pipeline reality, including empty-shell → `""` (decision 3 in the test plan).
- Don't write a test that reads the committed `results/*.json` — that's a second golden. Docs/JSON agreement is jq/grep commands only.
- Don't add CLI surface, cmd/ changes, or product code. `git diff go.mod` must be empty.

### Confirmed APIs (verified in-tree 2026-09-24, incl. a live probe over all 7 fixtures)

```go
clean.Clean(ctx, clean.RawPage{HTML []byte, URL, FinalURL string, StatusCode int, ContentType string}) (clean.CleanedPage, error)
clean.CleanedPage{Markdown string, Quality clean.Issue, ...}   // Clean never fails on quality; Classify attaches
clean.ToLLMText(p clean.CleanedPage) string
clean.WordCount(s string) int                                   // drops link targets
// Issue constants: clean.IssueNone (""), IssueEmpty, IssueAccessDenied, IssueUnavailable, IssueLoginRequired
// Module: github.com/motherlodelab/magpie ; fixtures live at ../../testdata/<dir>/<name>.html from the package dir
```

**Measured anchors (frozen — do not re-derive):** article@200 120 words, md 1042B → llm 945B, `""`; product@200 70 words, 440→931B (llm GROWS), `""`; spa-shell@200 7 words, 41→54B, `""`; challenge-akamai@403 16 words `access-denied`; login-wall@401 11 words `login-required`; empty-shell@200 7 words `""`; rich-with-marker@403 809 words `""`.

### Files to Create

#### benchmarks/harness_test.go  (package `benchmarks` — the ONLY file in the dir)
Bootstrap exactly as test-plan §5: `writeResults` flag, `fixture` struct (name, dir, status, band, facts, wantIssue, llmShrinks), `corpus` (7 rows with the frozen bands/statuses/qualities above), `loadHTML`, `factRe`, `runRow`, `result` struct. Tests: `TestFactMatching` (the 3 cases in §7 verbatim), `TestCorpus_CleanQuality` (§7 verbatim shape). Curate facts by reading the fixture bodies: 5 each for article/product/rich-with-marker, 1 for spa-shell ("Please enable JavaScript to use this app."), none for pure quality rows; criteria: visible in body, specific, stable. Benchmarks: `BenchmarkClean`/`BenchmarkLLM` with `b.Run` per content fixture + `b.ReportMetric(words/sec)`. Writer: `TestCorpus_WriteResults`-style flag handling — when `-write-results` is set, re-run rows, emit `{generated_at, go_version, commit, aggregates{fidelity_pct, mean_reduction_vs_raw_pct}, per_fixture[{fixture,raw_words,md_words,llm_tokens,md_chars,llm_chars,facts_kept,facts_total,clean_ns}]}` (tokens = len/4 per repo convention), indent 2 spaces, `os.WriteFile` to the flag path. Commit via `runtime/debug.ReadBuildInfo` vcs.revision, `""` fallback. Timing ceiling carries a `ponytail:` comment naming the ceiling and upgrade path.

#### benchmarks/README.md + benchmarks/methodology.md + benchmarks/results/2026-09-24.json
Run the writer on master, commit its output as `results/2026-09-24.json`. README: headline + per-fixture table copied from that JSON + repro commands. Methodology: metric definitions, len/4 token proxy (no tokenizer dep — repo convention from `capTokens`), fact criteria, and a limitations section: offline self-curated 7-fixture corpus, NOT an independent web benchmark, no competitor comparisons. Then `AGENTS.md` structure tree: add `benchmarks/  # offline quality harness (test-only)`; mirror in spec §13 tree only if it lists package dirs.

### Data Model Notes

Plain structs only — `fixture` and `result` as specified; no interfaces, no JSON config files, no `t.Parallel()`. Table-driven with `t.Run(f.name, …)`.

### Success Criteria

- `go test ./benchmarks/ -v` exits 0, 7/7 rows + matcher table green
- `go test ./benchmarks/ -bench . -benchmem -run XXX` prints ns/op + words/sec rows
- `go test ./benchmarks/ -run TestCorpus -write-results=benchmarks/results/2026-09-24.json` writes schema-exact JSON; `jq .aggregates benchmarks/results/2026-09-24.json` parses
- `grep -c golden benchmarks/harness_test.go` prints 0 (or only matches in comments — prefer zero entirely)
- Full gates: `go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./...`; `CGO_ENABLED=0 go build ./...`; `git diff go.mod` empty

---

## 9. Run Commands

```bash
# Full suite (the only CI gate — rides go test ./...)
go test ./benchmarks/ -v

# Perf numbers (manual, informational)
go test ./benchmarks/ -bench . -benchmem -run XXX

# Regenerate the committed results JSON
go test ./benchmarks/ -run TestCorpus -write-results=benchmarks/results/2026-09-24.json
jq .aggregates benchmarks/results/2026-09-24.json

# Docs gates (README numbers == committed JSON; spot-check 3)
jq -r '.per_fixture[] | "\(.fixture) \(.md_words)"' benchmarks/results/2026-09-24.json
grep -n "120" benchmarks/README.md   # article md words, adjust after read

# Golden-independence pin
grep -c golden benchmarks/harness_test.go   # 0

# Full gate (mirrors phase-X exit criteria)
go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./... \
  && CGO_ENABLED=0 go build ./... && test -z "$(git diff go.mod)"
```
