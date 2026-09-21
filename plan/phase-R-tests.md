# Phase R — Testing: Deterministic Element Relocation (zero-LLM heal middle step)

**Scope:** `selector/relocate.go` (NEW: `ElementFP`, `nodeSig`, `fingerprintSel`, `numText`, `scoreNode`, `Relocate`), `selector/validate.go` (+`SelectorDoc.Fingerprints`), `selector/synth.go` (EngineVersion 2 + fingerprint computation after final validation), `crawl/crawl.go` (`tryRelocate` + `healField` hook + Fields/Fingerprints map-sync), `spec.md` (docs — grep gate, no tests)
**Key Pattern:** **Zero new fakes — the zero-LLM property is asserted by counters that already exist.** `fakeExtractor.count("synth")`/`count("extract")` is the LLM-cost oracle (propose-counter via `cannedPropose`), `testTruth`/`crawlTruth` (12.99 eur_decimal) is the truth oracle, product-A/B (byte-identical except the id, verified) is the redesign fixture, and decline paths are pinned as "counters ≥ 1 + records intact" — today's exact behavior. Every new test is hermetic loopback/literal work; no browser tier exists in this phase's blast radius.
**Dependencies:** stdlib `testing`, `context`, `encoding/json`, `net/http/httptest`, `strings`, `reflect`, `os`, `path/filepath`, `fmt` only — plus in-repo helpers: `fakeExtractor` (selector/selector_test.go:23, crawl/crawl_test.go:42 — per-package copies by convention), `testTruth` (selector_test.go:122), `cannedPropose` (selector_test.go:71), `loadSelectorFixture` (selector_test.go:124), `mustParseTestSchema` (selector_test.go:133), `threeSamples` (selector_test.go:141), `openCrawlDB` (crawl/crawl_test.go:111), `crawlTruth` (crawl_test.go:100), `mustTestSchema` (crawl_test.go:102). No new test deps. **The jsonld negative schema is an inline literal** (existing pattern at selector_test.go:506–512: `ean: {type: string, x-magpie: {jsonld_path: "$.gtin13"}}`) — not a named const.

**Deviations from plan/phase-R.md (sanctioned additions found while wiring the suite):**
1. **Three tests added beyond the phase plan's five-plus-one** — the phase plan pins Relocate's contract and the crawl success path, but leaves three cheap, high-value pins unpinned: `TestScoreNode_Weights` + `TestNumTextAndNodeSig` (the scoring table is the phase's calibrated heart — the ponytail note says weights WILL be retuned; a table makes retuning safe instead of blind), `TestCrawl_HealRelocationDeclinesOldDoc` (Decision 1's compat claim — "old docs unmarshal with nil map → relocation declines → old behavior" — is otherwise asserted by nothing: existing heal tests synthesize FRESH docs, which post-R.1 all carry fingerprints), and `TestTryRelocate_SkipsJSONLDAndMultiple` (the two dangerous skips live in ~5 lines of crawl glue with no other coverage; `crawlContext{db, opts, schemaHash}` is directly constructible in-package — crawl.go:113).
2. **Store round-trip folds into `TestCrawl_HealRelocationZeroLLM`** rather than a standalone test: the money crawl test already does `PutSelectors`(fps in) → `Run` → `GetSelectors` (sqlite.go:397/384), so the exit-criterion round-trip is two assertions inside it, not a new file.
3. **The stderr success line is NOT pinned** — the existing suite pins no `warnf` text anywhere (no captureStderr equivalent lives in crawl_test.go), and copying a pipe-swapping helper for one log line is overhead without a failure mode. Review the line by eye in the money crawl test's output.

---

## User Stories

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | As a crawl operator, I want a redesigned page's broken field re-found by structure and validated against ground truth I already paid for, so that steady-state heals cost 0 LLM calls | `TestRelocate_RenamesIDKeepsStructure` (selector level) + `TestCrawl_HealRelocationZeroLLM` (pipeline level) | propose counter **== 0** through synth AND relocate; `fx.count("synth") == 0` for the whole run; every record's price == 12.99; persisted doc carries a selector that extracts 12.99 on product-B |
| US-2 | As an operator with existing caches, I want fingerprint-less (EngineVersion-1-era) docs to heal exactly as today, so that the upgrade is invisible | `TestCrawl_HealRelocationDeclinesOldDoc` + untouched-green gate on `TestHeal_BrokenFieldOnly` (selector_test.go:411) / `TestCacheCmd_Heal*` (cli/cmd_test.go:360/479) | pre-seeded doc with `Fingerprints: nil` → `fx.count("synth") == 1` (the existing heal synth), records intact, `git diff` shows **zero** existing test bodies modified |
| US-3 | As an operator, I want relocation to decline rather than guess, so that an ambiguous page never silently corrupts values | `TestRelocate_Declines_NoFingerprint` / `_NoNewSamples` / `_Ambiguous` + the non-null-on-every-new-sample guard inside the money test | all three return `(FieldSelector{}, ElementFP{}, false)`; margin-guard fixture (two isomorphic nodes) declines; no winner is ever accepted below `need(n)` agreement |
| US-4 | As a user resuming crawls across runs, I want fingerprints to ride the selector cache, so that relocation works in the *next* run where heals actually fire | `TestFingerprint_SynthesizeWritesParity` (JSON round-trip) + money-test `GetSelectors` assertions | `EngineVersion == 2`; every css field in `doc.Fields` has an fp, jsonld fields don't; `json.Marshal`→`Unmarshal` preserves fps (`reflect.DeepEqual`); store round-trip: fps visible after `GetSelectors`, and `keys(Fingerprints) ⊆ keys(Fields)` |
| US-5 | As a maintainer retuning weights (the plan's declared upgrade path), I want the scoring table pinned by units, so that recalibration is a reviewed diff, not a silent behavior change | `TestScoreNode_Weights` + `TestNumTextAndNodeSig` | identical node scores 1.0; the `Price:`-label false positive scores ≥ 0.3 below the true node; layout classes (`row-2`) filtered from sigs; score always within [0,1]; numText table exact |

**Traceability:** US-1 → relocate/money-crawl rows; US-2 → declines-old-doc row + untouched-green gate; US-3 → three decline rows; US-4 → parity + store-round-trip rows; US-5 → scoreNode/numText/nodeSig rows. Every row in §1 maps to at least one story; every story traces to §1 rows.

---

## 1. Component Mock Strategy

Phase type: **pure logic (scoring, partitioning) + one pipeline-integration tier through the real crawl Run**. Mock strategy in one sentence: **the LLM is the only heavy dependency and it is already faked — `fakeExtractor`'s purpose counters (selector_test.go:57, crawl_test.go:76) are the zero-LLM oracle, `cannedPropose`'s counter is the propose oracle, and everything else is pure functions over Go-literal HTML or the existing product-A/B/C fixtures.**

| Component | Mock Strategy | What to Assert | User Story |
|-----------|--------------|----------------|------------|
| `Relocate` — A→B redesign (the money path) | `relocate_test.go`: synth a doc from `threeSamples("product-A.html")` via `Synthesize` + `cannedPropose(&n)`; build samples A,A,B,B with `testTruth`; call `Relocate(samples, "price", doc.Fields["price"], doc.Fingerprints["price"], sch)` | counter `n == 0` after Synthesize AND after Relocate (both free); returned sel extracts `12.99` (eur_decimal→float64) non-null on BOTH B samples via `ExtractFieldValue`; returned fp has `NumText == true` and `strings.Contains(Parent, "buybox")`; proposal counter is the zero-LLM proof, not an eyeball | US-1, US-3 |
| `Relocate` — decline: no fingerprint | zero-value `ElementFP{}` → call → `(FieldSelector{}, ElementFP{}, false)`; never an error, never a panic on the empty map/attrs | US-3 |
| `Relocate` — decline: no new-template samples | all-A samples (old selector works everywhere) → partition finds 0 new → `false` even though truth is non-null throughout | US-3 |
| `Relocate` — decline: ambiguous (margin guard) | Go-literal page with two isomorphic candidates — `<div class="offer"><span class="amt">5.00</span></div>` + `<div class="offer"><span class="amt">7.00</span></div>` (identical tag/classes/parent-sig/NumText/convert-ok → equal scores → margin 0 < 0.08) ×2 samples → `false`; pins that the guard, not luck, declines | US-3 |
| `scoreNode` — weight table | `relocate_test.go` direct units (package selector — unexported fn): identical-node == 1.0; `Price:`-label vs true node gap ≥ 0.3; partial class Jaccard {a,b}∩{a,c} beats zero overlap; layout-filtered parent sigs score equal; adversarial node clamped into [0,1] | US-5 |
| `numText` / `nodeSig` / `fingerprintSel` | same file, small tables: numText on `12.99`/`$1,299.00`/`€12,99`/`12%`/`-3`/`4001234567890` ✓ and `""`/`Price:`/`12 dollars`/`N/A` ✗; nodeSig: `div class="row-2 buybox"` → `div.buybox` (layoutClassRe), `div class="col-12"` → `div`, `span#cost-now` → `span#cost-now`; fingerprintSel: hit populates Tag/ID/NumText/Parent, `#nope` → `false` | US-5 |
| `Synthesize` fingerprint parity | synth from A×3 (`mustParseTestSchema` — no jsonld) → `EngineVersion == 2`, every field in `doc.Fields` has an fp entry, marshal/unmarshal `DeepEqual`; then synth with the inline jsonld schema (selector_test.go:506 pattern) → `ean` present in Fields as `jsonld` type, **absent from Fingerprints** | US-4 |
| Crawl pipeline — relocation fires end-to-end | `crawl_test.go` APPEND `TestCrawl_HealRelocationZeroLLM`: `TestCrawl_QualityCountedNotCached` wiring (crawl_test.go:720) — httptest mux serving product-B for every path, `openCrawlDB`, pre-seed `PutSelectors` with the A-synthesized doc (fps ride it in — the store round-trip starts here), `Run` with `FetchWorkers: 1`, `MaxPages: 12` (trigger fires at exactly page 10: minEvidence = window/5 = 10, 10/10 nulls > 0.30) | `fx.count("synth") == 0` and `fx.count("extract") == 10` (fills stop at the trigger — relocation took over); all 12 records price == 12.99; `GetSelectors` doc: `Fields["price"]` extracts 12.99 on product-B, `Fingerprints["price"].ID == "cost-now"`, `EngineVersion == 2`, `keys(Fingerprints) ⊆ keys(Fields)` (map-sync invariant) | US-1, US-4 |
| Crawl pipeline — fingerprint-less doc declines | same wiring, but strip `Fingerprints` (set nil) before `PutSelectors` → `fx.count("synth") == 1` (exactly one heal synth — today's path), records intact, re-synthesized price serves the remaining pages | US-2 |
| `tryRelocate` — dangerous skips | `crawl_test.go` direct unit: `&crawlContext{db: openCrawlDB(t), opts: Options{Schema: mustTestSchema(t)}, schemaHash: …}` + `domainState{healer, doc}` (crawl.go:101) whose doc has fps for a jsonld field, a `multiple: true` field (inline schema with `x-magpie: {multiple: true}` — hint key verified at extract/schema.go:120), and price; ring = A samples (price's old selector still works → declines) | `tryRelocate` returns ALL THREE fields in `triggered` (jsonld and Multiple skipped even though fingerprinted; nothing relocated, nothing persisted) | US-3 |
| Existing heal behavior | NO new tests — the gate: `TestHeal_BrokenFieldOnly`, `TestCacheCmd_HealTooFewSamples/HealSuccess`, all `TestSynthesize*`, `TestCrawl_QualityCountedNotCached` pass with **unmodified bodies** | US-2 |

---

## 2. Test Tier Table

| Tier | Dependencies | Speed | When to Run |
|------|-------------|-------|-------------|
| Unit + Integration (default `go test ./...`) | Product fixtures (static files), Go-literal HTML, `openCrawlDB` (temp-dir SQLite), loopback httptest — **no network, no browser, no LLM** | <5s added | Every push; the only gate this phase needs |
| Race (`go test -race ./selector/ ./crawl/`) | Same, plus `-race` | ~15s | Every push on touched packages (phase plan exit criterion) |
| Manual | None meaningful — no new user-facing surface (CLI output unchanged except one stderr line) | — | Eyeball the money crawl test's stderr for the `relocated without LLM` warning; no live-site tier (that's the plan's declared calibration ceiling, not a test) |

No browser tier: this phase never touches `fetch/` — relocation runs on HTML the pipeline already has.

---

## 3. Fake / Mock Implementations

**No new fakes.** Every oracle exists and is reused verbatim (per-package fake copies are the established convention — no testutil):

- `fakeExtractor` (selector/selector_test.go:23, crawl/crawl_test.go:42) — scripted `map[string]map[string]any` keyed by `PromptExtra`, `count(purpose)` = the zero-LLM oracle
- `cannedPropose(t, canned, *calls)` (selector_test.go:71) — propose counter; `canned: nil` is fine when the free paths must win
- `testTruth` (selector_test.go:122) / `crawlTruth` (crawl_test.go:100) — `{price: 12.99, title: "Widget", ean: "4001234567890"}`
- `mustParseTestSchema` (selector_test.go:133) — price `coerce: eur_decimal` (this is what makes the `Price:`-label convertText penalty real), title trim, ean plain string
- `openCrawlDB` (crawl_test.go:111) + `store.PutSelectors/GetSelectors` (sqlite.go:397/384) — the cache seam
- Fixtures `testdata/selector/product-{A,B,C}.html` — **untouched**; every other page in the suite is a Go literal

Full source of the one fake the execution prompt needs pasted (selector copy — the crawl copy is identical minus the fixture helpers):

```go
type fakeExtractor struct {
	mu     sync.Mutex
	calls  []llmCall
	script map[string]map[string]any
	err    error
}

func (f *fakeExtractor) Extract(ctx context.Context, in extract.ExtractInput) (extract.ExtractResult, error) {
	if err := ctx.Err(); err != nil {
		return extract.ExtractResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	purpose := in.Purpose
	if purpose == "" {
		purpose = "extract"
	}
	f.calls = append(f.calls, llmCall{purpose: purpose, url: in.PromptExtra})
	rec := f.script[in.PromptExtra]
	if rec == nil {
		rec = f.script["default"]
	}
	raw, merr := json.Marshal(rec)
	if merr != nil {
		return extract.ExtractResult{}, merr
	}
	return extract.ExtractResult{Record: rec, Raw: raw, Provider: "fake", Model: "fake", Attempts: 1}, nil
}

func (f *fakeExtractor) count(purpose string) int { /* lock; count calls by purpose */ }
```

---

## 4. Test File List

```
magpie/
├── selector/
│   ├── relocate.go                # DELIVERABLE (impl): ElementFP, nodeSig, fingerprintSel, numText, scoreNode, Relocate
│   ├── relocate_test.go           # NEW (package selector — internal, so the unexported
│   │                              #   scoring helpers are directly testable):
│   │                              #   TestRelocate_RenamesIDKeepsStructure   (money, US-1)
│   │                              #   TestRelocate_Declines_NoFingerprint    (US-3)
│   │                              #   TestRelocate_Declines_NoNewSamples     (US-3)
│   │                              #   TestRelocate_Declines_Ambiguous        (US-3, margin guard)
│   │                              #   TestFingerprint_SynthesizeWritesParity (US-4, jsonld negative + round-trip)
│   │                              #   TestScoreNode_Weights                  (US-5, ADDED per deviation 1)
│   │                              #   TestNumTextAndNodeSig                  (US-5, ADDED per deviation 1)
│   └── selector_test.go           # UNTOUCHED — helpers reused; TestHeal_BrokenFieldOnly must stay green
├── crawl/
│   ├── crawl.go                   # DELIVERABLE (impl): tryRelocate + healField hook + map-sync
│   └── crawl_test.go              # APPEND (package crawl):
│                                  #   TestCrawl_HealRelocationZeroLLM       (money crawl, US-1+US-4, store round-trip)
│                                  #   TestCrawl_HealRelocationDeclinesOldDoc (US-2, ADDED per deviation 1)
│                                  #   TestTryRelocate_SkipsJSONLDAndMultiple (US-3, ADDED per deviation 1)
├── spec.md                        # DELIVERABLE (docs) — gate: grep fingerprints/§4.4/§4.3, no tests
└── testdata/                      # ZERO new fixtures, ZERO golden changes — porcelain must be empty
```

Existing tests that must stay green **untouched**: `TestHeal_BrokenFieldOnly` (selector_test.go:411), `TestCacheCmd_HealTooFewSamples`/`HealSuccess` (cli/cmd_test.go:360/479), every `TestSynthesize*`/`TestEmit*` (EngineVersion/parity are additive), `TestCrawl_QualityCountedNotCached` (crawl_test.go:720), the full CLI + MCP suites (cache format is additive JSON).

---

## 5. Test Helper Structure (Go — no `conftest.py`)

| Helper | Home | Used for | New? |
|--------|------|----------|------|
| `loadSelectorFixture(t, name)` | selector_test.go:124 | product-A/B/C bytes for samples and literal-page alternatives | reuse |
| `threeSamples(t, name)` | selector_test.go:141 | A×3 for the synth step of the money test and parity test | reuse |
| `mustParseTestSchema(t)` | selector_test.go:133 | css-only schema (price eur_decimal drives the convertText penalty) | reuse |
| `cannedPropose(t, canned, *calls)` | selector_test.go:71 | propose counter — the zero-LLM assertion at selector level | reuse |
| `captureStderr(t, fn)` | selector_test.go:152 | available, but NOT used (deviation 3 — no warnf-text pins in this suite's convention) | reuse-or-skip |
| `openCrawlDB(t)` | crawl_test.go:111 | temp-dir store for pre-seed + `GetSelectors` asserts | reuse |
| `crawlTruth` / `mustTestSchema` | crawl_test.go:100/102 | crawl run scripting + schema | reuse |
| `fakeExtractor` (both copies) | selector_test.go:23 / crawl_test.go:42 | extract fills during the broken window; synth counter | reuse |
| A/B sample builder `aabbSamples(t)` | relocate_test.go | `[]SynthSample{A,A,B,B}` with `testTruth` — 6 lines, local to the money test's file | **new, trivial** |
| fp-strip helper for the old-doc test | crawl_test.go (inline in the test) | `doc.Fingerprints = nil` before `PutSelectors` — one line, no helper | inline |

Go has no conftest; per-package helper copies are the convention (AGENTS/testing rules). Nothing here justifies a shared testutil package.

---

## 6. Key Testing Decisions

| Decision | Approach | Rationale |
|----------|----------|-----------|
| Counters, not mock-verification theater | `cannedPropose`'s int counter and `fakeExtractor.count("synth")` are the zero-LLM proof, asserted as `== 0` (money paths) and `== 1` (decline path) | The phase's entire value proposition is a *countable* cost property; eyeballed "looks fast" proves nothing. Exact `==` (not `<=`) so a silent synth/propose addition fails loudly |
| Synthesize's counter asserted too | money test asserts `n == 0` after `Synthesize` on A×3 AND after `Relocate` | If the A-synthesis needed propose, the counter would be pre-poisoned and the Relocate assertion vacuous. Both-must-be-zero keeps the proof honest |
| `TestCrawl_HealRelocationDeclinesOldDoc` exists at all | pre-seed with `Fingerprints: nil`, assert synth == 1 + records intact | Decision 1's compat claim ("old docs → old behavior") is the riskiest silent break in the phase: existing heal tests synthesize fresh docs and would NOT catch a crash/regression on nil-map docs. This is the pin |
| Decline = exact old-path counts, not just "no panic" | old-doc test asserts synth == **1** (the heal synth still fires, exactly once — ring resets after trigger, re-synthesized selector serves the rest) | `synth >= 1` would pass even if relocation half-fired; `== 1` proves the trigger fired once and relocation didn't double-handle it |
| Scoring pinned by a weight table, not only integration | `TestScoreNode_Weights` asserts the published weights' *discriminating* properties (identical = 1.0, label gap ≥ 0.3, [0,1] clamp) rather than every point value | Point-value asserts would make the planned weight retune (ponytail note) a test-rewrite; property asserts survive retuning while still catching a broken normalize/clamp |
| `FetchWorkers: 1` in both crawl tests | deterministic page order → trigger at exactly page 10, extract == 10, synth == 0/1 exact | With workers > 1 the null-page interleaving makes the counters ranges, not values — and ranges can't prove "0 LLM this heal" |
| Map-sync pinned as an invariant, not a scenario | money test asserts `keys(Fingerprints) ⊆ keys(Fields)` on the persisted doc | Arranging a *delete-side* scenario (field degrades during heal) needs a second bespoke fixture for one `delete()` line; the invariant catches an orphaned fingerprint in any future scenario for free |
| Ambiguity fixture uses identical siblings, not near-misses | two nodes with *identical* sigs → margin exactly 0 | A near-miss tests the margin constant; an exact tie tests the guard mechanism. The mechanism is what prevents corruption; the constant is calibrated by the ponytail note |
| `Multiple`/jsonld skips pinned at the glue, not via a second full Run | direct `tryRelocate` unit with a hand-built `domainState` | A Run-based version needs a Multiple schema + fixture + 12 more pages to cover one `if` each; `crawlContext{db, opts, schemaHash}` is three fields (crawl.go:113) and same-package tests construct it freely |
| No warnf-text pins (deviation 3) | money crawl test's stderr reviewed by eye | The suite has zero warning-text assertions today; adding pipe-swapping machinery for one line buys flake risk, not regression protection |

---

## 7. Example Test Case

The money test — the phase's core proof, in full (new file `selector/relocate_test.go`):

```go
func TestRelocate_RenamesIDKeepsStructure(t *testing.T) {
	sch := mustParseTestSchema(t)
	calls := 0
	propose := cannedPropose(t, nil, &calls)

	// 1. Synthesize on product-A (the old template). The free paths must win
	//    with zero proposals — otherwise the counter below is pre-poisoned.
	base, err := Synthesize(context.Background(), threeSamples(t, "product-A.html"), sch, propose, nil)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if calls != 0 {
		t.Fatalf("synthesize made %d propose calls; fixture must synth for free", calls)
	}
	fp, ok := base.Fingerprints["price"]
	if !ok {
		t.Fatal("no fingerprint for price after synth")
	}
	if base.EngineVersion != 2 {
		t.Errorf("EngineVersion = %d, want 2", base.EngineVersion)
	}

	// 2. The redesign: A,A,B,B. Old selector #price works on A, nulls on B
	//    (id renamed cost-now). Truth rides in the samples — already paid for.
	a := loadSelectorFixture(t, "product-A.html")
	b := loadSelectorFixture(t, "product-B.html")
	samples := []SynthSample{
		{URL: "http://ex.com/a1", HTML: a, Truth: testTruth},
		{URL: "http://ex.com/a2", HTML: a, Truth: testTruth},
		{URL: "http://ex.com/b1", HTML: b, Truth: testTruth},
		{URL: "http://ex.com/b2", HTML: b, Truth: testTruth},
	}

	sel, newFP, ok := Relocate(samples, "price", base.Fields["price"], fp, sch)
	if !ok {
		t.Fatal("Relocate declined the A→B redesign; want success")
	}
	if calls != 0 { // THE assertion: zero LLM through the whole path
		t.Errorf("propose calls = %d, want 0 (relocation must be free)", calls)
	}

	// 3. The winner must extract truth-exact on every new-template sample.
	for _, s := range samples[2:] {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(s.HTML))
		if err != nil {
			t.Fatal(err)
		}
		got, found := ExtractFieldValue(doc, s.Sidecar, "price", sel, sch.Hints)
		if !found || got == nil {
			t.Fatalf("relocated selector %q null on %s", sel.Expr, s.URL)
		}
		if CanonicalJSON(got) != CanonicalJSON(12.99) {
			t.Errorf("extracted %v, want 12.99", got)
		}
	}

	// 4. The returned fingerprint tracks the element the NEW selector matches.
	if !newFP.NumText {
		t.Error("newFP.NumText = false, want true (12.99 is numeric)")
	}
	if !strings.Contains(newFP.Parent, "buybox") {
		t.Errorf("newFP.Parent = %q, want it to contain buybox", newFP.Parent)
	}
}
```

Note what is asserted by *behavior* (extraction == truth) rather than by selector literal (`#cost-now`): `emitForSel`'s candidate ordering is synth.go's business, not Relocate's contract. The fp `Parent`/`NumText` checks are the structural identity pins.

---

## 8. Execution Prompt

Copy everything between the `---` lines into a new pi session to write this test suite (alongside the phase-R implementation):

---
You are writing the tests for **Phase R of magpie** — Deterministic Element Relocation, a zero-LLM middle step in selector self-healing. magpie is a Go 1.26 CLI web scraper (module `magpie`, CGO-free) at `/home/domidex/projects/magpie`. Read `AGENTS.md`, `.pi/rules/go.md`, `.pi/rules/testing.md`, `plan/phase-R.md`, and `plan/phase-R-tests.md` first. Default suite is hermetic (fixtures + Go literals + loopback httptest only); no new deps; the LLM is always `fakeExtractor`.

### Acceptance Criteria (from User Stories)

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | Redesigned field re-found and validated against already-paid truth — 0 LLM | `TestRelocate_RenamesIDKeepsStructure` + `TestCrawl_HealRelocationZeroLLM` | propose counter == 0 after synth AND relocate; `fx.count("synth") == 0`, `fx.count("extract") == 10`; all records price == 12.99; persisted selector extracts 12.99 on product-B |
| US-2 | Fingerprint-less docs heal exactly as today | `TestCrawl_HealRelocationDeclinesOldDoc` + untouched-green existing heal tests | nil-fps pre-seeded doc → `synth == 1`, records intact; zero existing test bodies modified |
| US-3 | Decline rather than guess | three `TestRelocate_Declines_*` + `TestTryRelocate_SkipsJSONLDAndMultiple` | all declines return `(FieldSelector{}, ElementFP{}, false)`; identical-sibling fixture declines via margin guard; jsonld/Multiple fields stay in `triggered` despite having fps |
| US-4 | Fingerprints ride the cache across runs | `TestFingerprint_SynthesizeWritesParity` + money-test store asserts | `EngineVersion == 2`; css fields fingerprinted, jsonld not; JSON round-trip `DeepEqual`; after `GetSelectors`: `Fingerprints["price"].ID == "cost-now"`, `keys(Fingerprints) ⊆ keys(Fields)` |
| US-5 | Scoring table pinned for safe retuning | `TestScoreNode_Weights` + `TestNumTextAndNodeSig` | identical node == 1.0; label-vs-true gap ≥ 0.3; `row-2` filtered from sigs; scores within [0,1]; numText table exact |

### Why There Are No New Fakes
The only heavy dependency is the LLM and `fakeExtractor` already fakes it with per-purpose call counters — those counters ARE the zero-LLM oracle. Truth (`testTruth`/`crawlTruth`), fixtures (product-A/B/C — byte-identical except the id), the store seam (`openCrawlDB` + `PutSelectors`/`GetSelectors`), and the propose counter (`cannedPropose`) all exist. New test material is limited to Go-literal HTML (ambiguous siblings, skip-guard doc) and a 6-line A,A,B,B sample builder.

### What NOT to Test
- **goquery/cascadia semantics** — they're the platform; pin OUR scoring/sig/relocate logic.
- **warnf stderr text** — the suite pins no warning text anywhere; deviation 3 in the tests plan. Eyeball it.
- **Multiple-field relocation itself** — out of scope by design (plan §Out-of-scope); only the skip guard is pinned.
- **store/SQLite internals** — `openCrawlDB` is the seam; the round-trip assert inside the money test is the contract.
- **Exact score point-values per weight** — pin discriminating properties (1.0 identical, ≥0.3 label gap, [0,1] bounds); the plan explicitly reserves the right to retune weights.
- **Which sample `fingerprintSel` reads during synthesis** (reverse-walk first-hit) — unobservable on identical fixtures; implementation detail.
- **`TestHeal_BrokenFieldOnly`, `TestCacheCmd_Heal*`, `TestSynthesize*`, `TestCrawl_QualityCountedNotCached`** — must pass UNTOUCHED. If one fails, the implementation broke compat; fix the implementation, never the old test.

### Critical: The Harness You Must Reuse (selector/selector_test.go)

```go
type fakeExtractor struct {
	mu     sync.Mutex
	calls  []llmCall // {purpose, url string}
	script map[string]map[string]any
	err    error
}
// Extract: records (purpose default "extract"), returns script[PromptExtra] (fallback "default"),
// Record + Raw json. count(purpose) / total() under mutex. — full source in the tests plan §3.

var testTruth = map[string]any{"price": 12.99, "title": "Widget", "ean": "4001234567890"}

func cannedPropose(t *testing.T, canned map[string]string, calls *int) ProposeFunc // increments *calls per invocation, returns canned[field]
func loadSelectorFixture(t *testing.T, name string) string                                    // ../testdata/selector/<name>
func mustParseTestSchema(t *testing.T) *extract.Schema                                        // price coerce:eur_decimal, title trim, ean string
func threeSamples(t *testing.T, name string) []SynthSample                                    // 3× fixture with testTruth
```

crawl/crawl_test.go: identical `fakeExtractor` copy (:42), `crawlTruth` (:100), `mustTestSchema` (:102), `openCrawlDB(t) *store.DB` (:111), and the wiring precedent `TestCrawl_QualityCountedNotCached` (:720: httptest mux + `Run(context.Background(), Options{SeedURL, Schema, MaxPages, MaxDepth, SameHost, FetchWorkers, Rate, Format, Out, DB, Extractor}))`.

Key production anchors (verified on master): `Synthesize(ctx, samples, sch, propose, only)` and `EngineVersion: 1`→2 at synth.go:12/40; `need` synth.go:169; `emitForSel` synth.go:257; `ExtractFieldValue`/`FieldAgreement`/`CanonicalJSON` validate.go; `Relocate(samples, field, old FieldSelector, fp ElementFP, sch) (FieldSelector, ElementFP, bool)` + `scoreNode(n, fp, field, hints) float64` + `numText(s) bool` + `nodeSig(s) string` + `fingerprintSel(doc, sel, hints) (ElementFP, bool)` are the NEW relocate.go surface; crawl glue: `healField` (crawl.go:768), `tryRelocate(st, domain, triggered) []string` (new), `domainState{mu, doc, loaded, healer, cachedPages}` (crawl.go:101), `crawlContext{db, opts, schemaHash, …}` (crawl.go:113); store: `GetSelectors`/`PutSelectors` sqlite.go:384/397; heal math: window 50, strict >0.30, minEvidence = window/5 = 10, retain 3.

### Test Files to Create / Edit

- **NEW `selector/relocate_test.go`** (`package selector` — internal, the unexported helpers must be reachable):
  - `TestRelocate_RenamesIDKeepsStructure` — full source in tests plan §7. Synthesize A×3 (counter must be 0 after), assert `EngineVersion == 2` + fp present, Relocate over A,A,B,B, counter still 0, winner extracts 12.99 on both B samples, newFP `NumText` + `Parent∋buybox`.
  - `TestRelocate_Declines_NoFingerprint` — zero-value `ElementFP{}` → `(FieldSelector{}, ElementFP{}, false)`.
  - `TestRelocate_Declines_NoNewSamples` — all-A samples → false (partition empty despite non-null truth).
  - `TestRelocate_Declines_Ambiguous` — literal page: `<div class="offer"><span class="amt">5.00</span></div><div class="offer"><span class="amt">7.00</span></div>` (×2 identical samples); fp from the first amt span; identical sigs ⇒ margin 0 ⇒ false.
  - `TestFingerprint_SynthesizeWritesParity` — synth A×3: every field in `doc.Fields` has an fp, `EngineVersion == 2`, `json.Marshal→Unmarshal→reflect.DeepEqual`; second synth with the inline jsonld schema (selector_test.go:506 pattern: `ean: {type: string, x-magpie: {jsonld_path: "$.gtin13"}}`): `ean` in Fields as jsonld, absent from Fingerprints.
  - `TestScoreNode_Weights` — with `mustParseTestSchema(t).Hints`, field "price": (a) fp from a node `<span id="p" class="amt" data-testid="price">12.99</span>` scored against itself == 1.0 (epsilon 1e-9); (b) the true `<span id="cost-now">12.99</span>` vs its `Price:`-label sibling: `score(true) - score(label) ≥ 0.3` and `score(label) < 0.4`; (c) classes {price-tag,bold} vs {price-tag,big} outscores vs {unrelated}; (d) parent `div class="row-2 buybox"` scores equal to fp parent `div.buybox` (layout filter); (e) adversarial node (numeric text vs `NumText:false` fp) → `0 ≤ score ≤ 1`.
  - `TestNumTextAndNodeSig` — numText: `12.99`/`$1,299.00`/`€12,99`/`12%`/`-3`/`4001234567890` true; ``/`Price:`/`12 dollars`/`N/A` false. nodeSig: `div class="row-2 buybox"` → `div.buybox`; `div class="col-12"` → `div`; `span#cost-now` → `span#cost-now`. fingerprintSel: hit fills Tag/ID/NumText/Parent; `#nope` → false.
- **APPEND `crawl/crawl_test.go`** (`package crawl`):
  - `TestCrawl_HealRelocationZeroLLM` — mux serving product-B bytes for EVERY path; seed page linking /p1../p12; `openCrawlDB`; synth a doc from A×3 exactly like the selector money test, `json.Marshal`, `db.PutSelectors(host, hash, raw, 3)`; `Run` with `FetchWorkers: 1, MaxPages: 12, MaxDepth: 1, SameHost, Rate: 1000`, `Extractor: &fakeExtractor{script: {"default": crawlTruth}}`. Assert: `fx.count("synth") == 0`; `fx.count("extract") == 10`; `res.Records == 12` and every output record's price == 12.99; `GetSelectors` → doc has `EngineVersion == 2`, `Fingerprints["price"].ID == "cost-now"`, `Fields["price"]` extracts 12.99 on product-B, `keys(Fingerprints) ⊆ keys(Fields)`.
  - `TestCrawl_HealRelocationDeclinesOldDoc` — same wiring but `doc.Fingerprints = nil` before PutSelectors. Assert: `fx.count("synth") == 1` (heal synth fired once — today's path), `fx.count("extract") >= 10`, records' price all 12.99, run succeeds.
  - `TestTryRelocate_SkipsJSONLDAndMultiple` — direct unit: `c := &crawlContext{db: openCrawlDB(t), opts: Options{Schema: sch}, schemaHash: selector.SchemaHash(sch)}`; `h := selector.NewHealer(50, 0.30, 3)` + 2× `h.Retain` A-samples; build `st := &domainState{healer: h, doc: …}` whose Fields+Fingerprints cover price (css), ean (jsonld), gallery (`x-magpie: {multiple: true}` inline schema — hint key at extract/schema.go:120); `got := c.tryRelocate(st, "ex.com", []string{"price","ean","gallery"})`; assert set(got) == set(input) — nothing relocated (A samples: old selectors work ⇒ price declines; jsonld/Multiple skipped despite fps), `st.doc` untouched.
- **`spec.md`** — no tests; gate is the phase plan's R.4 (grep fingerprints in §4.4 example + §4.3 paragraph exists).

### Data Model Notes (Go)
- `ElementFP{Tag string; Classes []string; ID string; Attrs map[string]string; Parent, Grandparent string; NumText bool}` — compare with `reflect.DeepEqual` after round-trips, never string-format.
- `SelectorDoc.Fingerprints map[string]ElementFP \`json:"fingerprints,omitempty"\`` — nil map after unmarshal is the compat contract; assert presence via the two-value map read, not len alone.
- Extraction comparisons go through `CanonicalJSON(got) == CanonicalJSON(12.99)` (eur_decimal coerces to float64 — compare apples to apples).
- Scores are float64: use epsilon (`math.Abs(got-want) < 1e-9`) for the 1.0 assert; inequalities elsewhere.
- `Relocate` declines return zero values + false — NEVER errors; assert with the comma-ok form.

### Success Criteria
- `go test ./selector/ ./crawl/ -run 'Relocate|Fingerprint|ScoreNode|NumText|Heal|TryRelocate' -v -count=1` — all new tests green
- `go test ./... -count=1` green with ZERO existing test bodies modified (`git diff --stat` shows only new/append files + impl)
- `go test -race ./selector/... ./crawl/...` clean
- `go vet ./...`, `gofmt -l .` empty, `golangci-lint run ./...` clean
- `git status --porcelain testdata/` prints NOTHING (zero new fixtures, zero golden changes)
- The zero-LLM property holds by counter: money tests report propose == 0 and synth == 0

---

## 9. Run Commands

```bash
# Baseline BEFORE writing (non-vacuous gate — testing.md)
go test ./selector/ ./crawl/ -count=1

# New tests, focused (mirrors R.1–R.3 sanity checks)
go test ./selector/ -run 'Relocate|Fingerprint|ScoreNode|NumText' -v -count=1
go test ./crawl/    -run 'Heal|TryRelocate'                      -v -count=1

# Money proofs (the two zero-LLM pins, alone)
go test ./selector/ -run TestRelocate_RenamesIDKeepsStructure -v -count=1
go test ./crawl/    -run TestCrawl_HealRelocationZeroLLM      -v -count=1

# Compat + untouched-green gate
go test ./selector/ ./crawl/ ./cli/ -count=1                  # includes TestHeal_BrokenFieldOnly, TestCacheCmd_Heal*
git diff --stat                                               # must show NO existing test bodies modified
git status --porcelain testdata/                              # must print NOTHING

# Race on touched packages
go test -race ./selector/... ./crawl/... -count=1

# Full gate (mirrors phase-R exit criteria)
go test ./... -count=1 && go vet ./... && golangci-lint run ./... \
  && test -z "$(gofmt -l .)" && echo ALL GREEN
```
