# Phase R — Deterministic Element Relocation (zero-LLM heal middle step)

**Duration:** 1 day (~8–10h)
**Depends on:** master only (independent of Phase K desktop work — one-way: desktop imports core)
**Blocks:** nothing hard; feeds the zero-LLM-verticals positioning and the GUI "no key needed for steady state" story.
**Risk Level:** MEDIUM — additive pass that can only *decline* back to today's exact code path; the only shared-state touch is `healField`, pinned by a zero-LLM-call assertion.
**Stack:** go

> Note (AGENTS.md): `run-phase` expects `stack: python|nextjs|react|typescript` and will
> hard-block on this file. Execute manually via the execution prompt below.

**Source:** competitive-analysis-2026-09-20.md §5 item 1 (Scrapling steal: "adaptive element
relocation — deterministic, SQLite"). Scrapling relocates a remembered element by fingerprint
similarity instead of re-resolving it; we apply the same idea to per-field cached selectors,
with our own twist: validate relocations against ground truth we have *already paid for*.

---

## Objective

Today, when a site redesign breaks a cached selector, every page pays a per-page LLM fill
until the null-rate trips (window 50, trigger 0.30, min evidence 10), and then `healField`
pays one more "synth" LLM call plus `Propose` calls to re-synthesize. The redesign case is
the *common* case and it is pure DOM churn — the element still exists, only its selector
rotted (`product-A` → `product-B`: `#price` renamed to `#cost-now`, everything else intact).

This phase inserts a zero-LLM middle step before LLM re-synthesis:

1. **Fingerprint** (persisted per field in the SelectorDoc): what the element the cached
   selector matched *looks like* — tag, classes, id, whitelisted attrs, parent/grandparent
   signature, and whether its text was numeric.
2. **Relocate** on fresh pages: score every candidate node against the fingerprint; the
   best node above threshold yields a new selector (reusing the existing `emitForSel`).
3. **Validate for free**: the new selector must agree with the retained new-template
   samples' truth — records the per-page LLM fill *already produced and retained*. Zero
   additional LLM calls on the whole path.
4. Only when relocation declines (no fingerprint, too few samples, ambiguous, or below
   threshold) does today's `healField` run unchanged.

## What Success Looks Like

1. `Synthesize` now emits fingerprints: a doc synthesized from `product-A` samples has
   `doc.Fingerprints["price"]` with `Tag=span`, `Parent` containing `buybox`, `NumText=true`;
   jsonld/Multiple fields are absent from the map. Round-trips through
   `json.Marshal`/`Unmarshal` + `store.PutSelectors`/`GetSelectors` unchanged (additive JSON).
2. THE test: doc synthesized on `product-A` ×3 → apply `product-B` (old selector `#price`
   nulls) → `Relocate` returns a selector that extracts `12.99` on B-pages **with 0 new LLM
   calls** (a `propose` func that increments a counter is passed and must stay at 0).
3. End-to-end crawl test: cached doc + redesigned pages → heal fires and `fakeExtractor`
   `synth`-purpose call count during heal is **0** (vs ≥1 today); record values unchanged.
4. Decline paths all fall back cleanly: EngineVersion-1 doc (nil fingerprints), <2 usable
   new samples, ambiguous top-2 (score margin < 0.08) → today's behavior, byte for byte.
5. Gates green: `go test ./...`, `go test -race ./selector/... ./crawl/...`, `go vet ./...`,
   `gofmt -l .` empty, `golangci-lint run ./...` clean; existing heal tests
   (`TestHeal_BrokenFieldOnly`, `TestCacheCmd_Heal*`) untouched and passing.

---

## Key Design Decisions

### Current flow (verified on master)

```
extractPage (crawl.go, hasDoc branch)
  └─ Applier.Apply → nulls[] → Healer.Observe        (window 50, trigger 0.30)
       └─ trigger → healField(ctx, st, …, triggered)
            ├─ per-null page (earlier): extractOne "extract" fill
            │    └─ validRequired → healer.Retain(SynthSample{HTML, Truth})   ← truth ALREADY paid
            ├─ extractOne "synth" on current page        ← 1 LLM call to re-learn truth
            └─ Synthesize(samples, only=triggered)       ← css_hint → heuristic → Propose (LLM)
```

The waste: new-template samples with paid-for truth sit in the ring while heal pays a synth
call to re-derive the same truth, then pays `Propose` to re-find the element.

### New flow

```
healField
  ├─ tryRelocate(st, domain, triggered)      ← NEW, zero LLM
  │    per field f (skip: no fingerprint | jsonld | Multiple):
  │      Relocate(ring samples, f, old sel, fp, schema)
  │        ├─ partition: samples where OLD selector nulls  (new template)
  │        ├─ need ≥2 of them with non-null Truth[f]       (else decline)
  │        ├─ per sample: score all nodes vs fp → best (threshold + margin)
  │        ├─ emitForSel(bestNode) → candidates            (reuse synth.go)
  │        └─ winner: FieldAgreement(newSamples, f, cand) ≥ need(n)
  │             AND extracts non-null on every new sample
  │    success → st.doc.Fields[f] + st.doc.Fingerprints[f] updated, persisted
  ├─ triggered = remaining; if empty → return               (0 LLM this heal)
  └─ …existing body unchanged (synth → Synthesize → merge)…
```

### Decisions

1. **Fingerprints live in the SelectorDoc, not just memory.** Most real heals happen in a
   *new* run (the trigger needs ~10 null pages; desktop crawls are short) — a memory-only
   fingerprint dies with the process and never fires. Additive `omitempty` JSON field on
   `SelectorDoc`; old docs unmarshal with nil map → relocation declines → old behavior. No
   migration, no store change. `EngineVersion` 1 → 2 marks fingerprint-bearing docs.
2. **Fingerprint = role, not instance.** Unlike Scrapling (relocate *the* button), we
   relocate *the field's element* — the text is exactly what churns. So the fingerprint
   stores structure (tag/classes/id/attrs/parent chain) plus a `NumText` flag (was the
   matched text numeric?), not the text itself. `NumText` is what separates `12.99` from
   the `Price:` label sitting next to it in the same `.buybox` — without it the top-2
   margin guard would decline the A→B fixture.
3. **Validation = FieldAgreement against already-paid truth.** No shape-only acceptance in
   v1: a wrong relocation that silently corrupts values is worse than paying for a synth.
   The both-null-counts-as-hit quirk of `FieldAgreement` is closed by requiring the winner
   to extract non-null on every new-template sample explicitly.
4. **Decline is the default.** Any of: nil fingerprints, <2 new samples, <2 non-null
   truths, below-threshold best, ambiguous margin (<0.08), Multiple field, jsonld field →
   leave the field in `triggered` and let the existing path run. No new error sentinels —
   relocation failing is normal control flow, not an error.
5. **Keep the two maps in sync.** Wherever `healField` merges or deletes
   `st.doc.Fields[f]`, it must now also write/delete `st.doc.Fingerprints[f]`
   (delete when a field degrades to per-page LLM; `Synthesize` repopulates on full resynth).
6. **Data model (Go):** plain structs with JSON tags on `SelectorDoc` (additive);
   `ElementFP` is a value struct, no methods beyond helpers; no interfaces — single
   implementation; scoring is a pure function `(node, fp, field, hints) → float64`.
   No new dependency (goquery/cascadia already sanctioned).

### Scoring weights (max 14.5, normalize by it)

| Component | Points | Notes |
| :-- | :-- | :-- |
| tag equal | +2 | |
| classes | both empty +1.5, else Jaccard×3 | redesigns rename classes — partial overlap still scores |
| id equal (non-empty) | +1.5 | if the id survived the old selector usually still works |
| whitelisted attrs | both empty +1, else matchedFraction×2 | name, itemprop, data-testid, data-test, data-id |
| parent sig equal | +2 | sig = `tag` + `.class`es + `#id` |
| grandparent sig equal | +1 | |
| numeric agreement | match +2, mismatch −2 | `NumText` vs candidate text via `^[$€£]?-?\d[\d,.]*%?$` |
| `convertText(field, text, hints)` ok | +1 ok, −1 fail | reuses validate.go; only bites when hints constrain |

Accept per-sample best when normalized score ≥ 0.65 **and** top-2 margin ≥ 0.08; ties
break by document order (goquery `Each` order). If a sample yields no acceptable node,
that sample is skipped; <2 usable samples declines.

`ponytail:` weights are calibrated by hand against the product-A/B fixtures only — the
ceiling is synthetic-redesign coverage; retune when a real-world mis-relocation shows up
(upgrade path: per-component tuning corpus, healthy-field proximity anchor for list pages).

### Out of scope (deliberate)

- Multiple/list fields (relocation of a repeating-record child is a different problem).
- Shape-only acceptance (no-key case is unchanged: without a key there are no truths and
  heal was already impossible).
- Healthy-field proximity anchoring (upgrade path noted above).
- MCP/UI exposure of fingerprints (rides along in fields JSON; `cache inspect` untouched).

---

## Tasks

### Task R.1 — Fingerprint type + synthesis hookup (1.5h)

Add `ElementFP` and compute it where agreement is already proven.

- `selector/validate.go`: add to `SelectorDoc`:
  `Fingerprints map[string]ElementFP \`json:"fingerprints,omitempty"\`` and bump
  `EngineVersion` to 2 in `synth.go` (line ~40).
- `selector/relocate.go` (new, target ≤200 lines):
  - `type ElementFP struct { Tag string; Classes []string; ID string; Attrs map[string]string;
    Parent, Grandparent string; NumText bool }` with JSON tags.
  - `nodeSig(s *goquery.Selection) string` — `tag#id.class1.class2`, layout classes filtered
    with the existing `layoutClassRe`.
  - `fingerprintSel(doc *goquery.Document, sel FieldSelector, hints extract.FieldHints)
    (ElementFP, bool)` — `doc.Find(sel.Expr).First()`, empty → false; `NumText` from the
    node's trimmed text.
- `selector/synth.go`: after the final validation loop, for each remaining **css** field
  walk samples in reverse, first sample where `ExtractFieldValue` hits → `fingerprintSel`
  → store. jsonld fields and fields absent from the doc get no entry.

**Sanity check:** `go test ./selector/ -run TestSynthesize` — existing synth tests still
pass (fingerprints are additive).

### Task R.2 — Scoring + Relocate (3.5h) — the core

**Depends on:** R.1

In `selector/relocate.go`:

- `numText(s string) bool` — the numeric-shape regex above.
- `scoreNode(n *goquery.Selection, fp ElementFP, field string, hints extract.FieldHints) float64`
  — the weighted table above, normalized to [0,1]. Pure function.
- `Relocate(samples []SynthSample, field string, old FieldSelector, fp ElementFP,
  sch *extract.Schema) (FieldSelector, ElementFP, bool)`:
  1. Partition: parse each sample once (`goquery`); "new" = `ExtractFieldValue` on `old`
     misses. Need ≥2 new samples with non-null `Truth[field]`, else `(…, false)`.
  2. Per new sample: scan `doc.Find("*")`, score, keep best above 0.65 with margin 0.08
     over runner-up. Skip the sample if none.
  3. Candidates: `emitForSel(bestNode)` per usable sample, dedup, in order.
  4. Winner = first candidate with `Regex/Coerce` from `sch.Hints` copied onto it,
     `FieldAgreement(newSamples, field, cand, sch) >= need(len(newSamples))` **and**
     non-null extraction on every new sample. `need()` already exists in synth.go.
  5. Return the winning selector + `fingerprintSel` of the relocated node on the *newest*
     usable sample (so the fingerprint tracks the element the *current* selector matches).
- Tests in `selector/relocate_test.go` (reuse `fakeExtractor`, `testTruth`,
  `loadSelectorFixture`, `mustParseTestSchema` from selector_test.go):
  - `TestRelocate_RenamesIDKeepsStructure` — A×2 + B×2 samples, old `#price` sel →
    returns B-valid selector, truth-exact on both B samples; propose counter == 0;
    returned fp has `NumText=true`, Parent containing `buybox`.
  - `TestRelocate_Declines_NoFingerprint` — zero-value fp → false.
  - `TestRelocate_Declines_NoNewSamples` — all samples old-template → false.
  - `TestRelocate_Declines_Ambiguous` — hand-built page with two isomorphic candidate
    nodes → false (margin guard).
  - `TestFingerprint_SynthesizeWritesParity` — synth on A fixtures: every css field in
    `doc.Fields` has an fp, jsonld fields don't; marshal/unmarshal round-trip preserves it.

**Sanity check:** `go test ./selector/ -run 'Relocate|Fingerprint' -v`.

### Task R.3 — Wire into healField (2h)

**Depends on:** R.2

- `crawl/crawl.go`: add `tryRelocate(st *domainState, domain string, triggered []string)
  []string` (≈30 lines, per the pseudocode in Key Design Decisions). Persist the merged
  doc once when anything relocated (`PutSelectors`, same warning style as neighbors).
  First line of `healField`: `triggered = c.tryRelocate(st, domain, triggered);
  if len(triggered) == 0 { return }`. One `warnf`-style stderr line on success
  (`selector: field %q relocated without LLM`) — matches existing observability.
  Keep `Fields`/`Fingerprints` in sync in the existing merge/delete blocks below.
- `crawl/crawl_test.go`: `TestCrawl_HealRelocationZeroLLM` — follow the
  `TestCrawl_QualityCountedNotCached` wiring (httptest mux + `openCrawlDB` +
  `fakeExtractor`): pre-seed `selector_cache` via `PutSelectors` with a doc synthesized
  from A-pages, serve B-pages, run `Run(...)`, assert: records non-null on B pages,
  `fx.count("synth")` during heal == 0, and `GetSelectors` shows the relocated field.
  (Note the existing suite has no direct `healField` test — this is the missing pin.)

**Sanity check:** `go test ./crawl/ -run Heal -v` and `go test -race ./crawl/ ./selector/`.

### Task R.4 — Spec + gates (1h)

**Depends on:** R.3

- `spec.md` §4.4 (doc format): add the `fingerprints` field, one sentence, plus a
  `"fingerprints"` line in the example JSON block. (Its example already says
  `"engine_version": 2` — pre-existing spec/code drift this bump fixes; don't revert it.)
- `spec.md` §4.3 (Partial failure handling — where the self-healing prose lives; there
  is no §4.5): one paragraph — relocation pass before LLM re-synthesis, decline
  semantics, zero-additional-LLM property. Spec is source of truth (AGENTS.md).
- Full gates: `go test ./... && go vet ./... && golangci-lint run ./... && gofmt -l .`
  (must print nothing), `go test -race ./selector/... ./crawl/...`.

---

## Deliverables

```
magpie/
├── selector/
│   ├── relocate.go          # ElementFP, nodeSig, fingerprintSel, scoreNode, Relocate (~200 lines)
│   ├── relocate_test.go     # 5 selector-level tests incl. the zero-LLM A→B money test
│   ├── validate.go          # + Fingerprints field on SelectorDoc
│   └── synth.go             # EngineVersion 2; fingerprint computation after validation
├── crawl/
│   ├── crawl.go             # tryRelocate + 2-line healField hook + map-sync in merge blocks
│   └── crawl_test.go        # TestCrawl_HealRelocationZeroLLM
├── spec.md                  # §4.4 fingerprints field + §4.3 relocation paragraph
└── plan/phase-R.md          # this file
```

## Exit Criteria

- [ ] `TestRelocate_RenamesIDKeepsStructure` passes with propose-counter == 0 (zero-LLM proof)
- [ ] `TestFingerprint_SynthesizeWritesParity` passes; `EngineVersion == 2`; JSON round-trip
      through `store.PutSelectors`/`GetSelectors` preserves fingerprints
- [ ] All three decline tests pass and existing tests are untouched-green
      (`go test ./selector/ ./crawl/ ./cli/`)
- [ ] `TestCrawl_HealRelocationZeroLLM` passes: heal with 0 synth-purpose calls, records intact
- [ ] `go test ./...` green, `-race` clean on touched packages, vet/lint/gofmt clean
- [ ] spec.md §4.4/§4.3 describe relocation (source of truth updated)

---

## Execution Prompt

Copy everything between the `---` lines into a new pi session to implement this phase:

---
You are building Phase R of **magpie** — deterministic element relocation, a zero-LLM middle
step in selector self-healing.

### What This Project Is
magpie is a Go 1.26 CLI web scraper (fetch → clean → extract), CGO-free, module name
`magpie`. Its differentiator: synthesize CSS/JSON-LD field selectors once from LLM samples,
cache them per (domain, schema-hash) in SQLite, and self-heal on null-rate so steady-state
pages cost 0 LLM calls. Source of truth: `spec.md` §4. Read `AGENTS.md` and `.pi/rules/go.md`
+ `.pi/rules/testing.md` first. Ethos: laziest working diff, decline-don't-fail, no new deps.

### Established on master (verified)
- `selector/validate.go`: `SelectorDoc{SchemaHash, Domain, Fields map[string]FieldSelector,
  SynthesizedAt, SamplesUsed, EngineVersion}`; `FieldSelector{Type, Expr, Regex, Coerce,
  NullRate}`. `ExtractFieldValue(doc, sidecar, field, sel, hints)` applies one selector;
  `FieldAgreement(samples, field, sel, sch) (hits, total)` counts both-null as a hit.
- `selector/synth.go`: `Synthesize(ctx, samples, sch, propose, only)` — css_hint → heuristic
  → LLM `Propose` last; final validation (N-1)/N; `emitForSel(s *goquery.Selection) []string`
  emits candidate selectors for one element (id → classes → attrs → positional);
  `need(n)` = unanimous except n≥3 allows one miss; `layoutClassRe` filters layout classes;
  `warnf` writes stderr warnings; `EngineVersion` hardcoded 1 at line ~40.
- `selector/heal.go`: `Healer` — `Observe(field, wasNull)` returns triggered fields at
  >0.30 null-rate after ≥10 evidence pages, then resets the ring; `Retain(SynthSample)`
  keeps the last 3 validated samples; `FullResynth(total, broken)` = ≥50% broken.
- `crawl/crawl.go` `extractPage` (hasDoc branch): `Applier.Apply` → nulls → `Observe` →
  `healField`. Per-null page runs an "extract" LLM fill and Retains the sample when
  `validRequired` — so at trigger time the ring mixes old-template samples (old selector
  works, old truth) with NEW-template samples (old selector nulls, fresh truth already paid).
  `healField` then pays one "synth" LLM call for fresh truth and calls
  `Synthesize(samples, sch, propose, only=triggered)`, merging results into `st.doc.Fields`
  and persisting via `db.PutSelectors(domain, hash, raw, n)`. Fields still failing are
  `delete`d from Fields (→ per-page LLM).
- Fixtures: `testdata/selector/product-A.html` (price at `<span id="price">` inside
  `<div class="buybox">`), `product-B.html` (byte-identical to A except the id renamed
  to `cost-now` — no wrapper/structure diff), `product-C.html`. Selector-package helpers
  `testTruth`, `cannedPropose`, `mustParseTestSchema`, `loadSelectorFixture` live in
  `selector/selector_test.go`; `crawl/crawl_test.go` has its own `fakeExtractor` copy
  (scripted map[string]map[string]any keyed by PromptExtra, counts calls by purpose)
  plus `crawlTruth` and `mustTestSchema` — no testTruth/cannedPropose/loadSelectorFixture
  there. Reuse per package; fakes are copied by convention (no testutil).
- Tests are hermetic; mock the LLM with `fakeExtractor`; touched packages must pass
  `go test -race`. Gates: `go test ./... && go vet ./... && golangci-lint run ./...`,
  `gofmt -l .` prints nothing.

### Your Goal
When a cached selector breaks in a redesign, re-find its element on fresh pages by
fingerprint similarity and validate the replacement selector against ground truth already
retained — zero additional LLM calls — declining to today's `healField` path whenever
anything is missing or ambiguous.

### Data Model Rules (Go — follow exactly)
- `ElementFP`: plain struct with JSON tags, value semantics, no methods except package-level
  helpers. Fields: `Tag string`, `Classes []string`, `ID string`, `Attrs map[string]string`,
  `Parent string`, `Grandparent string`, `NumText bool`.
- `SelectorDoc` gains one additive field: `Fingerprints map[string]ElementFP
  json:"fingerprints,omitempty"` — old docs unmarshal with nil map and must behave exactly
  as today. Bump `EngineVersion` to 2.
- No interfaces, no options structs, no new dependency. goquery/cascadia only (already used).

### Architecture
```
healField
  ├─ tryRelocate(st, domain, triggered)          NEW — zero LLM
  │    per field (skip: nil fp | jsonld | Multiple):
  │      Relocate(ring samples, f, old sel, fp, schema)
  │        ├─ new-template samples = old selector NULLS there; need ≥2 with non-null truth
  │        ├─ per sample: score all nodes vs fp; accept ≥0.65 normalized AND top-2 margin ≥0.08
  │        ├─ candidates = emitForSel(bestNode)   (reuse synth.go)
  │        └─ winner: FieldAgreement ≥ need(n) AND non-null on every new sample
  │    success → update Fields[f] AND Fingerprints[f]; persist doc once
  ├─ triggered = remaining; if empty → return    (0 LLM this heal)
  └─ existing body unchanged
```

Scoring (max 14.5, normalize): tag==+2; classes both-empty +1.5 else Jaccard×3; id equal
(non-empty) +1.5; whitelisted attrs (name, itemprop, data-testid, data-test, data-id)
both-empty +1 else matchedFraction×2; parent sig equal +2; grandparent sig equal +1;
numeric agreement +2 / mismatch −2 (regex `^[$€£]?-?\d[\d,.]*%?$` on trimmed text);
`convertText(field, text, hints)` ok +1 / fail −1. Sig = `tag` + `#id` + `.class`es with
`layoutClassRe` filtered. Ties by document order. `ponytail:` weights calibrated on the
A/B fixtures only.

### Files to Create / Touch

#### selector/relocate.go (new, ≤200 lines)
`ElementFP`, `nodeSig`, `fingerprintSel(doc, sel, hints) (ElementFP, bool)`
(Find(expr).First(), NumText from node text), `numText`, `scoreNode` (pure), and
`Relocate(samples, field, old, fp, sch) (FieldSelector, ElementFP, bool)` exactly as
architected. Copy Regex/Coerce from `sch.Hints` onto winning selectors. Return the fp of
the relocated node on the newest usable sample. Decline returns `(FieldSelector{},
ElementFP{}, false)` — never an error.

#### selector/validate.go
Add the Fingerprints field to SelectorDoc. Nothing else.

#### selector/synth.go
EngineVersion 2. After the final validation loop, for each css field walk samples in
reverse, first hit via ExtractFieldValue → fingerprintSel → store. No entry for jsonld /
uncached fields.

#### crawl/crawl.go
`tryRelocate(st, domain, triggered) []string` ≈30 lines; skip fields without fp, with
Type jsonld, or with `sch.Hints.Multiple[f]`; call Relocate; on success set BOTH
`st.doc.Fields[f]` and `st.doc.Fingerprints[f]`; persist once via PutSelectors if anything
changed; keep Fields/Fingerprints in sync in the existing merge and `delete` blocks of
healField. First lines of healField: relocate, return early when nothing remains. One
stderr warning on success (existing warnf style).

#### Tests
- `selector/relocate_test.go`: the five tests named in the phase plan. The money test
  (`TestRelocate_RenamesIDKeepsStructure`) synthesizes from product-A samples (truth =
  testTruth via fakeExtractor), builds samples A,A,B,B with testTruth, and Relocates the
  broken `#price` field with a propose counter that must stay 0.
- `crawl/crawl_test.go`: `TestCrawl_HealRelocationZeroLLM` — httptest mux serving A then B
  pages, pre-seed selector_cache via `store.PutSelectors` with a doc synthesized from A,
  `Run(...)` with `fakeExtractor`, assert B-page records are non-null, synth-purpose call
  count during heal is 0, and the persisted doc carries the relocated selector.

#### spec.md
§4.4: one sentence on the fingerprints field plus a `"fingerprints"` line in the example
JSON block. §4.3 (Partial failure handling — the self-healing section; do not invent a
§4.5): one paragraph on the relocation pass, decline semantics, and the
zero-additional-LLM property.

### Success Criteria
- `go test ./selector/ ./crawl/ -run 'Relocate|Fingerprint|Heal' -v` — all new tests green
- `go test ./...` green and untouched tests unmodified; `go test -race ./selector/... ./crawl/...` clean
- `go vet ./...`, `golangci-lint run ./...` clean; `gofmt -l .` prints nothing
- The A→B relocation extracts `12.99` with zero LLM calls (propose counter 0, synth count 0)

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available — master has everything
  (`Synthesize`/`need`/`emitForSel` at synth.go:169/257, `Healer.Retain` ring,
  `store.GetSelectors/PutSelectors` at sqlite.go:384/397, per-package `fakeExtractor`,
  product-A/B fixtures); no in-flight branch touches `selector/` or `crawl/healField`.
- [PASS] Every sub-task has a clear, testable completion condition — each task ends in a
  named test or a gate command; the zero-LLM property is asserted by counter, not eyeballed.
- [PASS] Execution prompt is self-contained — (a) prior-phase facts are the verified
  flow description with function/file anchors, (b) no new libraries so no snippets needed
  (goquery APIs cited from existing call sites in synth.go), (c) Data Model Rules section
  present (Go structs, additive JSON, no interfaces), (d) per-file guidance for all 7
  touched files, (e) observable success criteria with exact commands.
- [PASS] Exit criteria map 1:1 to deliverables — every file in the tree is exercised by at
  least one criterion; nothing untested.
- [PASS] Heavy external dependency fakes — LLM is `fakeExtractor` (existing, per-package
  convention); no network (fixtures inline / httptest); no browser suite touched.
- [PASS] New libraries — none (goquery/cascadia already sanctioned and in use in this
  package); no snippet research required.
