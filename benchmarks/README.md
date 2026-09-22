# magpie offline benchmarks

Quality floors + reproducible extraction numbers for magpie's clean pipeline,
measured over the committed `testdata/` fixtures. **Offline, self-curated
corpus** — this is not an independent web benchmark; see
[limitations](methodology.md#limitations).

## Headline (run `results/2026-09-24.json`)

- **Fact fidelity: 100%** — 16/16 curated facts present in both markdown and
  llm output.
- **Mean word reduction vs raw HTML: 18.1%** (informational aggregate).
- All 7 fixtures clean in well under the asserted 1s wall ceiling; steady-state
  cost is single-digit ms (`go test ./benchmarks/ -bench . -run XXX`).

| fixture | raw words | md words | md vs raw | llm tokens (len/4) | md → llm chars | facts kept | quality |
|---|---|---|---|---|---|---|---|
| article | 135 | 120 | −11% | 236 | 1042 → 945 (91%) | 5/5 | |
| product | 84 | 70 | −17% | 232 | 440 → 931 (212%) | 5/5 | |
| spa-shell | 9 | 7 | −22% | 13 | 41 → 54 (132%) | 1/1 | |
| challenge-akamai | 22 | 16 | −27% | 32 | 110 → 131 (119%) | — | access-denied |
| login-wall | 15 | 11 | −27% | 21 | 63 → 87 (138%) | — | login-required |
| empty-shell | 9 | 7 | −22% | 13 | 41 → 54 (132%) | — | |
| rich-with-marker | 815 | 809 | −1% | 1415 | 5516 → 5661 (103%) | 5/5 | |

Why product's llm output *grows*: `ToLLMText` adds a metadata header and gated
structured data, which outweigh a 70-word body. Only the article row asserts
`len(llm) < len(md)`; every row's ratio is reported, none is asserted.

## What the tests pin

`TestCorpus_CleanQuality` asserts floors that golden regeneration (`clean -update`)
cannot absorb: frozen word bands, fact fidelity in **both** md and llm output,
typed `Quality` per row, `<50`-word caps on blocked/thin pages, the
rich-page-under-403 guard (no false `access-denied`), article-only llm shrink,
and a 1s pathological-regression tripwire. Bands are frozen measurements with
±25–30% slack — see [methodology](methodology.md).

## Reproduce

```bash
# metric floors (rides go test ./...)
go test ./benchmarks/ -v

# perf numbers (informational)
go test ./benchmarks/ -bench . -benchmem -run XXX

# regenerate the results JSON (path resolves from repo root)
go test ./benchmarks/ -run TestCorpus -write-results=benchmarks/results/2026-09-24.json
jq .aggregates benchmarks/results/2026-09-24.json
```

Metrics, token proxy, fact-selection criteria, and the freezing procedure:
[methodology.md](methodology.md).
