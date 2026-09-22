# Phase K2 — Classifier hooks (TypeSafe Jev): agent-action gate, goal crawl, heal verify

**Duration:** 2 days (~17h)
**Depends on:** master @ 3509cbb (module `github.com/motherlodelab/magpie`, `vertical.Register` seam merged, `store.ListRuns`, MIT). Independent of Phase K desktop work — public-repo-only, additive-only, parallel-safe (see competitive-analysis-2026-09-20.md §5).
**Blocks:** Nothing hard. Feeds future GUI "smart crawl" (private repo) and cuts heal LLM cost.
**Risk Level:** MEDIUM — every hook is nil-interface-gated (no key ⇒ byte-identical behavior), no new dependencies, hermetic tests. Residual risk is call-site placement, covered per-task sanity checks.
**Stack:** go
**Runner:** none — `run-phase` hard-blocks on `stack: go` (AGENTS.md); execute manually via the execution prompt in §6.

**Deferred (YAGNI, do not build in this phase):** JS-render escalation in the heuristic's ambiguous band (`fetch/detect.go`) and `--vertical auto` choice-dispatch (`vertical/registries.go`). Both are listed in competitive-analysis-2026-09-20.md §5 as follow-ups; they become tasks when a use case asks.

---

## 1. Objective + What Success Looks Like

Add a cheap typed classifier (TypeSafe's Jev "System One" model) as an **opt-in, key-gated** decision layer at three points in the pipeline where a per-item yes/no decision is either hand-heuristic or costs an LLM call today: (1) the browser **actions DSL gate** — agent-authored action lines are classified before rod executes them (the MCP trust boundary; `eval-js` is arbitrary JS in the user's browser session); (2) **`crawl --goal`** — pre-fetch relevance pruning so the politeness budget (1 rps) is spent only on candidate URLs; (3) **selector heal verification** — a deterministic relocation candidate is semantically verified before it's trusted, falling through to the existing LLM re-synthesis path when rejected.

1. `go test ./...` green with **zero network**: all classifier tests run against `httptest` fakes; `git diff testdata/` is empty.
2. **No key ⇒ no change**: without `MAGPIE_TYPESAFE_API_KEY`, `magpie scrape ... --action 'eval-js ...'`, `crawl`, and heal behave byte-identically to master (existing goldens pass untouched).
3. With a fake classifier returning risk ≥ 0.9: `magpie scrape <url> --action 'eval-js return document.title'` exits **9** naming the refused action line; `--no-action-gate` runs it.
4. `magpie crawl <url> --goal "pricing pages"` without a configured classifier exits **7** with the set-key hint; with a fake classifier, anchors scoring < `--goal-min` (default 0.6) are never enqueued (frontier assertion in test).
5. Healer: accepted relocation ⇒ no LLM call recorded; rejected candidate or classifier error ⇒ existing LLM heal path runs (an `llm_calls` row appears).
6. `--max-cost` counts classifier calls (rows in the existing cost ledger, provider `typesafe`).
7. Live smoke (keyed, manual): 100-call p95 latency + measured cost recorded in README; go/no-go per §5.

## 2. Key Design Decisions

```
                    MAGPIE_TYPESAFE_API_KEY (flag > env > keyring > config file)
                                      │
                        classify.New (nil if unconfigured)
                                      │
      ┌───────────────────────────────┼───────────────────────────────┐
      ▼                               ▼                               ▼
fetch.FetchRequest.Classifier   crawl.Options.Classifier        scrape heal path hook
(actions gate, fail-closed      (goal prune at anchor time,     (verify relocation
 on eval-js; exit 9)             fail-open; exit 7 w/o key)      candidate; fall through
                                                                  to LLM on reject/error)
```

### Data Model Rules (Go — follow exactly)

- **Interface at the seam:** `classify.Classifier` — `Ask(ctx context.Context, state string, qs map[string]Question) (map[string]Answer, error)`. One implementation (`typesafeClient`); the fake lives in tests. No registry, no plugin surface (rule of three: one impl = no abstraction).
- **Nil = disabled is the contract.** Every Deps/Options struct gets `Classifier classify.Classifier` and every call site guards `if c == nil` → existing heuristic path. This mirrors the proven `scrape.Deps.Fetcher` nil-pattern (scrape/scrape.go:41) — same shape, same test style.
- **Plain structs with JSON tags** for `Question`/`Answer` (wire boundary). No reflection, no generics beyond what the SDK-free client needs.
- **Errors:** wrap with `%w`, export `ErrClassifierUnavailable` for transport/auth failures. Call sites — never the client — decide fail-open vs fail-closed.
- **No CGO, no new dependencies** (stdlib `net/http`). AGENTS.md rules all apply.

### Fail-safe rules (per call site)

| Site | Classifier nil | Classifier error | Decision rule |
| :-- | :-- | :-- | :-- |
| Actions gate | ungated (today's behavior) | **fail-closed for `eval-js`** (refuse, exit 9); other verbs unaffected | refuse whole batch if risk ≥ 0.9 (`--gate-threshold`) |
| `crawl --goal` | exit 7 + set-key hint (`--goal` without a classifier is a usage error) | **fail-open** — enqueue anyway (advisory gate; recall over precision) | prune when p(relevant) < `--goal-min` (default 0.6) |
| Heal verify | straight to existing LLM path | fall through to existing LLM path | accept relocation when p(match) ≥ 0.7 |

### State minimization (privacy)

The classifier is a third-party API; enabling it sends snippets off-machine. States are **minimized by construction**: actions gate sends the action lines only (never page content); goal prune sends anchor text (≤200 chars) + URL; heal verify sends the candidate element's outer HTML (≤2 KiB). Never the full page. This table goes into the README privacy note verbatim.

### Cost ledger

Classifier calls reuse the existing `llm_calls` table with `provider = "typesafe"`, `model = "jev-latest"`, cost from the API's usage fields (0 when absent). `--max-cost` counts them like any provider. No new table, no schema migration beyond what exists (ponytail: if usage fields are absent from the response, cost stays 0 and rows still count toward `--max-cost` call accounting — ceiling noted in code).

## 3. Tasks

### Task K2.1 — `classify/` package: types, interface, typesafe client (3h)

Types + client + contract tests, nothing else.

```go
// Confirmed against docs.typesafe.ai/api + quickstart (2026-09-20)
// Endpoint:
//   POST https://api.typesafe.ai/v1/systemone
//   Authorization: Bearer <API_KEY>
//   Content-Type: application/json
// Request:
//   {"model":"jev-latest",
//    "state":"<context text>",
//    "questions":{
//      "is_risky":{"type":"noul","instructions":"Does this action sequence do something destructive or exfiltrate data?"},
//      "vertical":{"type":"choice","instructions":"Which extractor fits?","criteria":{"shopify_product":"Shopify product page","ecommerce_product":"Generic product page"}}}}
// Response (per docs; SDK reads response.answers[...]):
//   {"answers":{"is_risky":{"type":"noul","noul":0.999},
//               "vertical":{"type":"choice","choice":"technical","confidence":0.97}}}
```

- `classify/classify.go`: `Classifier` interface, `Noul`/`Choice` question builders, `Answer` (Type, Noul, Choice, Confidence float64s), `ErrClassifierUnavailable`. **Omit `score` questions** — no call site needs them (add when one does).
- `classify/typesafe.go`: stdlib client — 10s `context` deadline, single batched request (all questions ride one call — that's the economics), JSON decode, non-200 → `ErrClassifierUnavailable` (body snippet, redacted auth).
- `classify/typesafe_test.go`: `httptest` server as the fake; contract tests for request shape, noul/choice decode, error mapping. Live-shape caveat: the docs show `{"answers":{...}}`, the blog sample was unwrapped — the httptest fixture locks OUR contract; Task K2.8's first live call is the reconciliation point (fix fixture + client together if the wrapper differs).

**Sanity check:** `go test ./classify/ -v` — 5+ tests, no network.

### Task K2.2 — Config + Deps wiring (2h)

**Depends on:** K2.1

- `config/config.go`: resolve the key via the existing chain (flag > `MAGPIE_TYPESAFE_API_KEY` env > keyring > config file) — same path as LLM provider keys; `config show` redacts it.
- Deps plumbing following the `scrape.Deps.Fetcher` pattern: `Classifier` field on `scrape.Deps`, `crawl.Options`, and `fetch.FetchRequest`. Production constructors populate it from config; **nil when unconfigured**. `cli/shared.go` (or root persistent pre-run) is the single wiring point.

**Sanity check:** `MAGPIE_TYPESAFE_API_KEY=k ./magpie config show` shows the redacted key; unset shows empty. `go test ./config/ ./cli/` green.

### Task K2.3 — Actions gate (3h)

**Depends on:** K2.1, K2.2

- `fetch/actions.go` (or a small `fetch/actiongate.go` — same package): before the rod executor runs a parsed action batch, if `req.Classifier != nil` and the batch contains `eval-js` (confirmed verb list: `click type scroll wait wait-for screenshot eval-js`), call `Ask` once with the joined action lines as state and one noul: *"Does this browser action sequence do something destructive, or exfiltrate page data to a third party?"* Risk ≥ threshold (default 0.9, `--gate-threshold`) ⇒ typed error `fetch: action gate refused (risk 0.97): eval-js return …` → **exit 9** (new code; 2–8 are taken per README).
- Fail-closed: classifier error while `eval-js` is present ⇒ refuse with the underlying error named (this is the security hook; erring toward refusal is the point).
- `--no-action-gate` (scrape + MCP `scrape_url`) skips the gate entirely. MCP: gate applies identically to agent-supplied `actions`; refusal surfaces as a tool error string with exit-9 semantics in the envelope.
- Non-`eval-js` batches never trigger a classifier call (zero-latency default path).

**Sanity check:** test with fake gate at 0.95 ⇒ exit 9; at 0.3 ⇒ executes; `--no-action-gate` ⇒ executes at 0.95; classifier error + eval-js ⇒ exit 9 naming the transport error.

### Task K2.4 — `crawl --goal` pre-fetch pruning (3h)

**Depends on:** K2.1, K2.2

- `crawl.Options`: `Goal string`, `GoalMin float64`, `Classifier`. Validation in `crawl.Run` (follow `ValidateSitemapOnly`'s pattern): `Goal != "" && Classifier == nil` ⇒ error → **exit 7** with the set-key hint (matches search's missing-key semantics). `Goal + --sitemap-only` ⇒ exit 2 (contradiction — sitemap-only never follows anchors, so there is nothing to prune).
- At anchor-extraction time (before frontier push): one `Ask` per **batch of anchors from the same page** — state = page URL, questions = one noul per anchor keyed by index (this is the batched-economics win; keep batches ≤ 25 anchors, chunk beyond). Instructions: *"This anchor is likely to lead to a page about: <goal>"*. p < `--goal-min` ⇒ not enqueued; classifier error ⇒ enqueue anyway (fail-open, advisory gate). Pruned counts surface in the run record (`pruned` counter) so users can see the gate working.
- Flags: `--goal <text>`, `--goal-min` (crawl + MCP `crawl_site`).

**Sanity check:** fake classifier returning 0.1 for anchors matching `/archive/` ⇒ those never appear in `crawl --status` counts or output; error-injecting fake ⇒ everything enqueued; missing key ⇒ exit 7.

### Task K2.5 — Heal verification (3h)

**Depends on:** K2.1, K2.2

- Where the heal path produces a relocation/re-synthesis candidate (`selector/` heal + `scrape` extraction path), accept the candidate only if a noul — state = candidate element's outer HTML (≤2 KiB) + field name, question: *"Is this element the <field> value on the page?"* — returns ≥ 0.7. Reject or error ⇒ existing LLM re-synthesis path runs unchanged.
- The check rides the existing `scrape.Deps.Classifier`; nil ⇒ today's path exactly.
- Record the verification outcome in the run envelope's heal diagnostics (`heal.verified: true|false`) — cheap observability, no new table.

**Sanity check:** fake at 0.9 ⇒ candidate used, zero `llm_calls` rows; fake at 0.2 ⇒ LLM path invoked (row present); erroring fake ⇒ same as 0.2.

### Task K2.6 — Cost ledger + run_history (1h)

**Depends on:** K2.1

- Every `Ask` records a row: `provider = "typesafe"`, `model = "jev-latest"`, tokens/cost from response usage (0 when absent). `--max-cost` counts classifier rows; hitting the ceiling behaves like today (exit 6).

**Sanity check:** fake server reporting usage ⇒ row visible in the run record; `--max-cost` trip test mirrors the existing cost tests.

### Task K2.7 — Docs (1h)

- README: new "Classifier (Jev)" section under Network & security — key resolution, the three hooks + flags, **the state-minimization table from §2 verbatim**, and an explicit "off unless keyed" statement.
- spec.md: short § under the pipeline (numbering per current §10.6 style); exit code 9 added to the exit-code table; `MAGPIE_TYPESAFE_API_KEY` in the env table.

**Sanity check:** `magpie scrape --help`, `magpie crawl --help` show the new flags; README renders the table.

### Task K2.8 — Live smoke + micro-benchmark (1h, keyed, manual)

**Depends on:** K2.1–K2.7

- With a real key: 100 calls against a fixed corpus (10 real action batches ×10, anchor sets from 3 saved pages). Record p50/p95 latency and measured cost into the README table.
- First live response is also the shape reconciliation point for K2.1's fixture.

**Sanity check:** numbers land in README; no test depends on them.

## 4. Deliverables

```
magpie/
├── classify/
│   ├── classify.go            # Classifier interface, Question/Answer types, ErrClassifierUnavailable
│   ├── typesafe.go            # stdlib REST client (batched questions, 10s deadline, redacted errors)
│   └── typesafe_test.go       # httptest-fake contract tests (hermetic)
├── config/config.go           # key resolution (flag > env > keyring > file), redaction in `config show`
├── fetch/
│   ├── actiongate.go          # eval-js gate: batched noul, threshold, fail-closed, typed refusal error
│   └── actiongate_test.go     # gate matrix: risk hi/lo, --no-action-gate, classifier error
├── crawl/
│   ├── crawl.go               # Options.{Goal,GoalMin,Classifier} + Run validation (exit 7 / exit 2 rules)
│   ├── goal.go                # anchor batching + prune-at-enqueue, fail-open, `pruned` counter
│   └── goal_test.go
├── selector/heal.go           # verify-hook after candidate, ≥0.7 accept, fall-through on reject/error
├── scrape/scrape.go           # Deps.Classifier (nil-pattern), heal diagnostics field
├── cli/
│   ├── scrape.go              # --no-action-gate, --gate-threshold; classifier wiring
│   └── crawl.go               # --goal, --goal-min; classifier wiring
├── mcp/tools.go               # scrape_url/crawl_site accept + surface the same flags/errors
├── store/sqlite.go            # (no schema change; cost rows reuse llm_calls)
├── README.md                  # "Classifier (Jev)" section, exit 9, env var, benchmark table
└── spec.md                    # pipeline §, exit-code row, env row
```

## 5. Exit Criteria

- [ ] `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` all green; `git diff testdata/` empty (hermetic rule holds)
- [ ] Without key: `magpie scrape <url> --action 'eval-js ...'` behaves exactly as master (gate inert); all three hooks' nil-paths covered by tests asserting the existing behavior
- [ ] Actions gate matrix green: risk ≥ 0.9 ⇒ exit 9 naming the line; `--no-action-gate` ⇒ executes; classifier error + eval-js ⇒ exit 9 naming the error
- [ ] `crawl --goal` matrix green: no classifier ⇒ exit 7 + set-key hint; pruned anchors never enqueued (`pruned` counter); classifier error ⇒ all enqueued; `--goal` + `--sitemap-only` ⇒ exit 2
- [ ] Heal matrix green: accepted candidate ⇒ 0 LLM rows; rejected/errored ⇒ LLM path row present
- [ ] Cost rows with `provider=typesafe` appear and `--max-cost` trips on them (exit 6)
- [ ] README/spec docs landed, including the state-minimization table
- [ ] `[ ] Live smoke done: p95 and cost/1k calls recorded in README. If p95 > 500 ms ⇒ the actions gate becomes opt-in (`--action-gate`, off by default) instead of default-on-when-keyed; note the decision in README. If cost > $0.10 per 1k calls ⇒ goal pruning keeps its explicit `--goal` opt-in and the README says so. Otherwise ship defaults as designed.`

## 6. Execution Prompt

Copy everything between the `---` lines into a new pi session to implement this phase:

---

You are building Phase K2 of **magpie** — a Go CLI web scraper (fetch → clean → extract), module `github.com/motherlodelab/magpie`, binary `magpie`. Pure Go, **zero CGO, no runtime deps, no new Go module dependencies without asking**. Default test suite is hermetic: **no network, no browser, no keys** (`go test ./...` must stay green untouched). Browser tests are gated behind `//go:build browser`. Coding ethos: lazy senior dev — minimum code that works, no abstractions with one caller, nil-interface fallbacks over config flags, comments explain *why*. `stack: go` means the `run-phase` skill will hard-block — implement manually, test-first per task.

### Established in prior phases (facts, not suggestions)

- `scrape.Deps` carries seams as nil-able fields, e.g. `Fetcher vertical.Fetcher` — "nil = production default; tests inject a fake" (scrape/scrape.go:41). **You must follow this exact pattern** for the new classifier.
- `crawl.Options` + `crawl.Run(ctx, opts)` exist with validators like `ValidateSitemapOnly` (contradictions exit 2); `crawl --status run_id` exists; anchors are extracted per page before frontier enqueue.
- Actions DSL verbs are exactly: `click type scroll wait wait-for screenshot eval-js` (fetch/actions.go). Malformed action lines exit 2. The actions executor runs inside the rod fetch path; `fetch.FetchRequest`/`fetch.Fetcher` (fetch/fetcher.go) are the transport seam.
- Exit codes: 2 usage/malformed, 3 sitemap-only-empty, 4 unknown run_id, 6 cost ceiling, **7 credentials missing (with set-key hint)**, 8 quality-blocked. **Exit 9 is free — you are claiming it** for action-gate refusal.
- Cost rows live in an existing `llm_calls` table; `--max-cost` trips exit 6. Keys resolve as `--api-key` flag > `MAGPIE_<PROVIDER>_API_KEY` env > OS keyring > config file, redacted in `config show`.
- `selector.Healer` (heal.go) has `Observe/Retain/SynthSample/NullRate`; the LLM re-synthesis path is triggered from the scrape extraction path when selectors break.

### Your Goal

Add an opt-in TypeSafe Jev classifier behind a nil-able interface with three hooks: (1) `eval-js` action gate (fail-closed, new exit 9), (2) `crawl --goal` pre-fetch anchor pruning (fail-open, exit 7 without key), (3) heal-candidate verification (≥0.7 accept, else existing LLM path). No key ⇒ byte-identical behavior to master.

### Confirmed API (docs.typesafe.ai/api + quickstart, 2026-09-20 — do not re-research)

```
POST https://api.typesafe.ai/v1/systemone
Authorization: Bearer <API_KEY>
Content-Type: application/json

{"model":"jev-latest","state":"<context>",
 "questions":{"is_risky":{"type":"noul","instructions":"..."},
              "v":{"type":"choice","instructions":"...","criteria":{"a":"when A","b":"when B"}}}}

→ {"answers":{"is_risky":{"type":"noul","noul":0.999},
              "v":{"type":"choice","choice":"a","confidence":0.97}}}
```

All questions in one request are evaluated in parallel — always batch. The response wrapper (`{"answers":{…}}`) comes from the docs; the first live call (Task K2.8) is the reconciliation point — if the real shape differs, fix the httptest fixture and decoder together in one commit. Non-200 ⇒ `ErrClassifierUnavailable` (include a body snippet, never the auth header).

### Data Model Rules (follow exactly)

- `classify/classify.go`: `type Classifier interface { Ask(ctx context.Context, state string, qs map[string]Question) (map[string]Answer, error) }`. Plain JSON-tagged structs for `Question` (oneof Noul/Choice via `type` field) and `Answer`. No `score` type. One client implementation; fake in tests only.
- Deps injection: `Classifier` fields on `scrape.Deps`, `crawl.Options`, `fetch.FetchRequest`. **Nil ⇒ existing path, no exceptions.** Production wiring in one place (cli shared setup); constructors never panic on nil.
- Errors: `%w`-wrapped, exported sentinels; call sites decide fail-open/closed per the table in the phase plan (§2) — do not move that policy into the client.

### Per-file guidance

- **classify/{classify,typesafe,typesafe_test}.go** — interface + types; client with 10s ctx deadline, single POST, batched map; httptest contract tests (request shape, decode, error mapping). Redact auth from errors.
- **config/config.go** — resolve `MAGPIE_TYPESAFE_API_KEY` through the existing key chain; redact in `config show`. Nothing else.
- **fetch/actiongate.go + _test** — gate only batches containing `eval-js`; one noul ("destructive or exfiltrates data?"); default threshold 0.9; ≥ threshold or classifier-error ⇒ typed refusal error consumed by cli as exit 9; `--no-action-gate` skips. Non-eval-js batches make zero classifier calls.
- **crawl/goal.go + crawl.go edits + goal_test.go** — `Options.{Goal, GoalMin, Classifier}`; validation: goal-without-classifier ⇒ error → exit 7 with set-key hint; goal + sitemap-only ⇒ exit 2. At anchor extraction, batch anchors (≤25/page) as indexed noul questions, state = page URL + goal; p < min ⇒ skip enqueue; classifier error ⇒ enqueue. Add a `pruned` counter to the run record.
- **selector/heal.go + scrape/scrape.go** — after a relocation/re-synth candidate, noul on (outer HTML ≤2 KiB + field name); ≥0.7 ⇒ accept and record `heal.verified:true`; reject/error ⇒ existing LLM path unchanged. Nil ⇒ today's path.
- **cli/{scrape,crawl}.go, mcp/tools.go** — new flags (`--goal`, `--goal-min`, `--no-action-gate`, `--gate-threshold`), MCP `scrape_url`/`crawl_site` accept the same; exit-9 refusals surface as tool errors in the envelope.
- **store/** — no schema change; cost rows reuse `llm_calls` with `provider=typesafe` (usage from response, 0 when absent) and count toward `--max-cost`.
- **README.md / spec.md** — "Classifier (Jev)" section with the state-minimization table (actions: lines only; goal: anchor ≤200 chars + URL; heal: element HTML ≤2 KiB; never full page), exit 9 row, env var row. Numbers from the live smoke go in a small table.

### Hard rules

- No new Go dependencies. No CGO. No live-network tests in the default suite (httptest fakes only). Never log the API key. `gofmt -l .` must print nothing; `golangci-lint run ./...` green.
- Don't touch: `clean/` goldens, `testdata/`, the rod executor's non-action paths, `vertical/` (dispatch is explicitly deferred).

### Success criteria

1. `go test ./... && go vet ./... && gofmt -l . && golangci-lint run ./...` green; `git diff testdata/` empty.
2. Without `MAGPIE_TYPESAFE_API_KEY`: gate inert, `--goal` exits 7 with set-key hint, heal identical to master.
3. Gate matrix (fake): 0.95 ⇒ exit 9 naming the line; 0.3 ⇒ runs; error+eval-js ⇒ exit 9; `--no-action-gate` ⇒ runs.
4. Goal matrix (fake): prune works via `pruned` counter; error ⇒ all enqueued; contradiction ⇒ exit 2.
5. Heal matrix (fake): 0.9 ⇒ 0 LLM rows; 0.2 or error ⇒ LLM row present.
6. Cost rows (`provider=typesafe`) counted by `--max-cost` (exit 6 trip test).

### Expected file structure at end

See §4 Deliverables in `plan/phase-K2.md` — same tree, no extras.

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available (master @ 3509cbb: Register seam, ListRuns, MIT, Deps nil-pattern, exit-code table, action verbs — all verified in source today)
- [PASS] Every sub-task has a clear, testable completion condition (per-task sanity checks + §5 matrices)
- [PASS] Execution prompt is self-contained: (a) prior-phase facts inline, (b) confirmed API snippets with endpoint/auth/response, (c) Go data-model rules (nil-pattern contract), (d) per-file guidance, (e) observable success criteria
- [PASS] Exit criteria map 1:1 to deliverables (every deliverable file has a matrix or doc criterion; deferred items excluded from scope by name)
- [PASS] Heavy external dependency has a fake/stub strategy (httptest fake locks the contract; live smoke is manual and keyed, reconciles response shape)
- [PASS] New libraries have a confirmed usage snippet (no new Go libraries; TypeSafe REST API confirmed from docs.typesafe.ai/api + quickstart with exact endpoint, auth, and request/response shapes)
