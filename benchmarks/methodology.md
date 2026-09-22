# Methodology

How `benchmarks/` measures the clean pipeline, what each number means, and what
these benchmarks explicitly are not.

## Corpus

Seven committed fixtures, run through the real `clean.Clean` → `ToLLMText`
pipeline with a per-row `StatusCode` (Classify is status-dependent):

- `testdata/clean/`: `article`, `product`, `spa-shell` — content rows with word
  bands and curated facts.
- `testdata/quality/`: `challenge-akamai` (403), `login-wall` (401),
  `empty-shell` (200), `rich-with-marker` (403) — blocked/thin rows plus the
  guard negative (rich content mentioning a challenge marker under 403 must
  **not** classify `access-denied`).

## Metrics

**Words** — `clean.WordCount` on the extracted markdown. It drops link targets
and fence/marker noise, so it differs from `wc -w` on the goldens (article:
120 measured vs 155 by `wc -w`). The goldens were never the oracle.

**Tokens** — `len(s)/4` chars-as-tokens, the repo convention used by `capTokens`
in `clean`. No tokenizer dependency; treat the number as a rough size proxy,
not a model-specific count.

**Raw words** — words in the fixture HTML after naive tag-stripping
(`<[^>]*>` → space, then the same `clean.WordCount`). Informational
denominator for the reduction column; not a rendering-faithful count.

**Fact fidelity** — curated strings from each fixture's visible body must
appear in **both** the markdown and the llm text, matched case-insensitively as
`regexp.QuoteMeta` literals; single tokens (no space/hyphen) get `\b` word
boundaries so `API` cannot match `apiece`. Selection criteria: visible in the
rendered body, specific (product/section names, prices, stats — "The Widget
Price Guide" qualifies, "Details" does not), stable across runs. Facts are
curated against extracted-text conventions: markdown output escapes `_` (so a
fact spanning `__cf_chl` would never match literally — pick neighboring text
instead).

**Reduction** — per row, md words vs raw words; the JSON aggregates the mean.
`len(llm) < len(md)` is asserted on the article row only: it is the only row
where the measured contract holds (product's metadata header + structured data
outweigh its 70-word body, growing llm output to ~212% of md chars). Every
ratio is reported in the results JSON; only measured contracts are asserted.

**Timing** — a 1s wall ceiling per fixture for clean + ToLLMText combined,
inside the test. Measured steady-state cost is single-digit ms, so the ceiling
cannot flake; it exists to trip the hidden-browser-launch / quadratic-scan
class of regression, not to make a perf claim. Honest per-op numbers live in
the `BenchmarkClean`/`BenchmarkLLM` functions (`ns/op`, `words/sec`,
`-benchmem` allocs). `clean_ns` in the results JSON is warm steady-state cost:
the pipeline is cleaned once untimed before the loop because the first Clean
pays one-time trafilatura init (~1s under test-suite load) that is not
per-page cost.

## Band freezing

1. Probe-run the corpus on master and record `clean.WordCount(markdown)` per
   content row (2026-09-24 probe: 120 / 70 / 7 / 809).
2. Freeze a band of roughly ±25–30% around the measured value into the corpus
   table (`article [90,150]`, `product [52,88]`, `spa-shell [4,12]`,
   `rich-with-marker [560,1000]`); blocked/thin rows assert `<50` words.
3. If a band fails on master after a *clean-side* change, that is a regression.
   If it fails after a deliberate extraction-behavior change, re-probe and
   re-freeze the band in the same PR, with the new measured value in the commit
   message.

Floors live in the Go corpus table, never in the goldens — regenerating
goldens with `-update` cannot absorb a regression past them.

## Results

`results/YYYY-MM-DD.json` per run, written by
`go test ./benchmarks/ -run TestCorpus -write-results=...`: `generated_at`,
`go_version`, `commit` (vcs.revision when the tree is stamped; `""` on
dirty/unstamped builds), `aggregates{fidelity_pct, mean_reduction_vs_raw_pct}`,
and one `per_fixture` row per corpus entry. Runs are committed so numbers stay
diffable across releases. The README table is copied from the committed JSON.

## Limitations

- **Offline and self-curated.** Seven fixture files written for this repo's
  tests. They are stable and readable, but they are not a sample of the live
  web and not independently sourced.
- **Not an independent web benchmark.** This is not a competitor comparison and
  makes no "beats X" claim. What it pins is (a) regression floors that golden
  regeneration cannot silently absorb and (b) published, reproducible numbers
  with the exact commands above.
- **Small corpus.** Seven rows chosen for pipeline semantics (content, thin,
  blocked, guard negative), not statistical coverage of page genres.
