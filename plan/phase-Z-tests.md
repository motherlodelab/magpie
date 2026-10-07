# Phase Z — Testing: Credential seam for API-first verticals

**Scope:** the new `vertical/credentials.go` (`fetchJSONAuth`/`fetchBytesAuth` header-carrying helpers, `githubAuthHeaders`, `redditToken`), the GitHub token passthrough in `vertical/github.go`, the Reddit OAuth-first path in `vertical/social.go` (incl. the NEW single-listing `.json` decoder — no listing fixture exists today; the current listing path is HTML-only), and the Z.5 credential-profile CLI (`cli/profile.go` save/ls/rm + `scrape --as`) — plus the additive test infrastructure they require (extended `fakeVerticalFetcher`, new `export_test.go` endpoint bridge).
**Key Pattern:** **No real network in the default suite** — extractor-level tests use the extended `fakeVerticalFetcher` (now recording full `fetch.FetchRequest`s so Authorization/UA headers are assertable); the Reddit token endpoint is faked with `httptest` reached through a test-only `export_test.go` bridge that overrides the unexported endpoint vars; one wire-level test proves headers reach the real `StaticFetcher` via httptest (localhost). Live Reddit smoke is `//go:build live` + env-gated, never default.
**Dependencies:** stdlib only (`testing`, `os`, `net/http`, `net/http/httptest`, `encoding/json`, `strings`) + in-repo `fetch` (`FetchRequest`, `StaticFetcher` with explicit `AllowPrivate` for httptest origins) + existing `vertical_test` fakes. **No new deps; `git diff go.mod` empty; `git diff --stat fetch/` empty.**

**Decisions pinned while verifying seams** (test-plan additions to phase-Z.md, discovered by reading `vertical/vertical_test.go` / `vertical/live_test.go` / `fetch/fetcher.go` on master, 2026-09-24):

1. **All `vertical/` tests are `package vertical_test` (external) — unexported endpoint vars are unreachable from tests without a bridge.** No `export_test.go` exists today (grep confirmed). The plan adds one: `package vertical` file exposing `OverrideRedditEndpoints(tokenURL, oauthHost string) (restore func())`. Test-only file, zero production API surface — this is how phase-Z.md's "unexported vars for hermetic tests" is actually reached.
2. **`fetch.FetchRequest` is GET-shaped (no Method/Body), so `redditToken` cannot ride the `Fetcher` — it uses `net/http` directly for the token POST.** `fetch/` stays untouched (phase-Z.md promise holds). Consequence: the token endpoint is faked by httptest (real HTTP through the bridge), NOT by the fake fetcher; the fake fetcher only serves the `oauth.reddit.com{path}.json` GETs.
3. **The shared `fakeVerticalFetcher` records URL order only (`order`/`requests()`). This phase's load-bearing asserts are headers** (Authorization Bearer, namespaced UA, basic-auth on the token call). Extension is additive: new `reqLog []fetch.FetchRequest` field + `reqLog()` accessor; `order`/`requests()` frozen for the ~40 existing call sites — zero churn, no consolidation until rule-of-three.
4. **Header asserts happen at two layers, deliberately:** (a) `FetchRequest` level via the fake for every vertical path (cheap, exact), (b) ONE wire-level test through real `StaticFetcher` + httptest proving the whole chain reaches HTTP headers. `fetch/` already owns "Headers get applied to the request" (existing crates-UA test, `vertical_test.go` ~line 344) — we do not re-test its internals, only our passthrough.
5. **Env in tests = `t.Setenv`** (house-approved: `config`, `plugin/exec`, `mcp` all use it). `t.Setenv` tests must not use `t.Parallel()` — the env is process-global; pin this so a "helpful" parallelize PR doesn't land a flake.
6. **Token-response JSON lives as inline consts** (2 lines each: happy `{"access_token":"…","expires_in":3600,"token_type":"bearer"}` + malformed). Fixture *files* are only reused for page bodies — existing reddit fixtures in `testdata/vertical/` feed the `.json` parse asserts. A third golden file for a 2-line payload is negative value.
7. **Live Reddit smoke: `//go:build live` AND skip-unless-all-three-env-vars-set.** The `live` tag's existing role is fixture-recapture quarantine (no network); this is the tag's first real-network use — acceptable because it's opt-in and credential-gated, but it must never fire in the default suite (AGENTS.md hard rule).

---

## User Stories

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | As a magpie user with no credentials configured, I want reddit/github extraction to behave exactly as before, so the feature can never regress my current workflows | `TestReddit_Tokenless_Unchanged` + `TestGithub_Tokenless_NoAuthHeader` + full existing suite green | No env → zero auth headers in `reqLog()`, old.reddit-first ladder order intact, existing tests pass **unmodified** |
| US-2 | As a GitHub user with a PAT, I want my token on the API requests so I stop hitting 60 req/h, so bulk repo lookups work | `TestGithubAuthHeaders` + `TestGithub_TokenOnRequest` | `MAGPIE_GITHUB_TOKEN` set → `Authorization: Bearer <tok>` present in the `FetchRequest` the github vertical issues; unset → absent |
| US-3 | As a Reddit user with app credentials, I want reliable `.json` extraction from any host, so datacenter IPs stop getting blocked | `TestReddit_OAuthFirst_Permalink` + `TestReddit_OAuthFirst_Subreddit` | Creds set → token POST carries basic-auth `client_id:secret` + `grant_type=client_credentials` + namespaced UA; subsequent GET hits `oauth.reddit.com…json` with `Authorization: Bearer <token from POST>`; record parses through the EXISTING JSON decoders (fixture-reused) |
| US-4 | As a user with a typo'd or partial credential, I want a loud error naming the problem, so a silent anonymous fallback never masks a misconfiguration | `TestReddit_PartialCreds` + `TestReddit_TokenFailure` | 1–2 of 3 vars set → error names the missing var(s); token endpoint 401/malformed → error names the grant step; **no fallback GET to old.reddit in `reqLog()`** in any failure case |
| US-5 | As an embedder of the `vertical` package, I want auth headers to survive the real fetch stack to the wire, so OAuth works with any `Fetcher` implementation | `TestAuthHeadersReachTheWire` (real `StaticFetcher` + httptest, `AllowPrivate`) | Server-side `r.Header.Get("Authorization")` == injected bearer; response parses via `fetchJSONAuth` |
| US-6 | As a user scraping a site whose login is just a session cookie, I want to save that credential once (`magpie profile save`) and reuse it with `scrape --as <name>`, so secrets never sit in shell history or command lines | `TestProfile_SaveLsRm` + `TestScrape_AsResolvesProfile` + `TestProfile_LsMasksSecrets` | profile file created 0600 under injected dir; `--as` injects cookies/headers like the flags; explicit flags win on conflict; `ls` never prints a full secret; unknown name errors listing known names |

## 1. Component Mock Strategy

Phase type: **integration** (external HTTP services: Reddit token grant, authed JSON APIs). Mock strategy in one sentence: **services are faked at two altitudes — `fakeVerticalFetcher` (extended to log `FetchRequest`s) for all extractor logic, `httptest` servers behind the `export_test.go` bridge for the token POST and the wire-level chain test — with zero live network outside `//go:build live`.**

| Component | Mock Strategy | What to Assert | User Story |
|-----------|--------------|----------------|------------|
| `fetchJSONAuth` / `fetchBytesAuth` | `fakeVerticalFetcher` (header-logging extension) | Headers land in `FetchRequest.Headers` verbatim (`"Authorization: Bearer t"`, order preserved); nil headers → request identical to today's `fetchJSON`; non-2xx → error names URL + status (existing contract) | US-2, US-5 |
| `githubAuthHeaders()` | Pure function + `t.Setenv`; table: unset / empty-string / set | Unset or `""` → nil; set → exactly `["Authorization: Bearer <v>"]` (empty string is NOT a credential — no `Bearer ` empty header on the wire) | US-2, US-1 |
| `redditToken(ctx, f)` | httptest server via `OverrideRedditEndpoints` bridge + real `net/http` (decision 2) | POST body `grant_type=client_credentials`; `Authorization: Basic b64(client_id:secret)`; UA header is the namespaced `MAGPIE_REDDIT_UA`; 200 → bearer string from `access_token`; 401 → error contains "token" + status; malformed JSON → error contains "decode" | US-3, US-4 |
| `extractReddit` OAuth-first | `fakeVerticalFetcher` serving fixture `.json` bodies under `oauth.reddit.com` keys + creds via `t.Setenv` + bridge | First request is `https://oauth.reddit.com{RequestURI}.json` (not old.reddit); Bearer header == token served by the (httptest) token URL; permalink → EXISTING `redditThreadFromJSON` (fixture reuse); listing → NEW single-listing decoder (small inline fixture — children t3/t5; **no existing listing `.json` fixture to reuse**, the current listing path is HTML-only) | US-3 |
| Tokenless reddit regression | Existing fixtures, no env, `reqLog()` order assert | Ladder order unchanged: old.reddit HTML first, `.json` retry for permalinks; zero auth headers anywhere | US-1 |
| Partial creds | `t.Setenv` 3 table rows (id only / id+secret / secret only) | Error message names EACH missing var (`MAGPIE_REDDIT_SECRET`, `MAGPIE_REDDIT_UA`); zero requests issued (`reqLog()` empty — fail before fetching) | US-4 |
| Token failure no-fallback | httptest 401 + creds set | Error from Extract names the token step; `reqLog()` contains ONLY the token POST's absence — zero page GETs attempted | US-4 |
| Wire-level chain | Real `fetch.StaticFetcher` (explicit `AllowPrivate`) + httptest echoing `r.Header.Get` back in the body | Server saw `Authorization` == injected value; `fetchJSONAuth` decodes the echoed JSON — proves Headers survive `profile`→`Cookies`→`Headers` application order | US-5 |
| Profile store (`cli/profile.go`) | Pure functions over an injected config dir (`t.TempDir()` subdirs, phase-Y init `dirs` pattern — `DefaultConfigDir()` unreachable from unit tests) | Save → file 0600 (GOOS-guarded), decodes to name→{cookies,headers}; upsert overwrites own key; `rm` missing → error; hostile JSON (`profiles": "x"`, invalid entry) → error, file bytes unchanged (sentinel) | US-6 |
| `profile save/ls/rm` command | Real cobra tree + `captureOutput` + injected dirs (house pattern) | `ls` output: names present, values MASKED (assert full secret string absent from stdout); `rm` then `--as` → unknown-name error listing remaining names | US-6 |
| `scrape --as <name>` resolution | Options-boundary unit test: profile + conflicting/explicit flags via `t.Setenv`-free injected profile file | Profile values fill ONLY fields the flags didn't set; explicit `--cookies`/`--header` win field-by-field; no profile → error names it + lists saved names; resolved values pass the SAME `ValidateOptions` policing as flags | US-6, US-1 |
| Live smoke (`live` tag) | Real reddit + real creds, env-gated skip | Permalink extract returns `kind=post` non-empty title; listing returns `kind=subreddit` | US-3 (drift alarm) |

## 2. Test Tier Table

| Tier | Dependencies | Speed | When to Run |
|------|-------------|-------|-------------|
| Default (`go test ./vertical/`) | Fakes + httptest (localhost) + `t.Setenv`; **no external network** | <2s | Every push; rides `go test ./...` |
| Full suite + gates | `go test ./...`, `go vet`, `gofmt`, `golangci-lint`, `git diff go.mod` + `git diff --stat fetch/` (must be empty) | ~60s | Every push (unchanged gate + one new diff guard) |
| Live smoke (`-tags live`) | Real `MAGPIE_REDDIT_*` creds + network | ~2s | Manual, before release touching social.go; skips cleanly without creds |

No E2E tier: the CLI flag plumbing for env vars doesn't exist (env is read per-call); the vertical Extract surface IS the user surface.

## 3. Fake / Mock Implementations

### Extension: `fakeVerticalFetcher` — in `vertical/vertical_test.go` (additive; existing fields/methods untouched)

```go
// NEW fields + accessor. Fetch() additionally appends a snapshot of the
// whole request — headers are this phase's subject under test.
type fakeVerticalFetcher struct {
	mu     sync.Mutex
	bodies map[string]fakeResp
	order  []string
	reqLog []fetch.FetchRequest // NEW: full-request snapshots, append order
}

func (f *fakeVerticalFetcher) reqLog() []fetch.FetchRequest { // NEW
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetch.FetchRequest(nil), f.reqLog...)
}

// inside Fetch(), right after the existing mu.Lock():
f.reqLog = append(f.reqLog, req) // NEW line; everything below unchanged
```

### New: `vertical/export_test.go` — `package vertical` bridge (decision 1)

```go
package vertical

import "net/http"

// OverrideRedditEndpoints points the token POST and the oauth .json GETs at
// test servers. Returns a restore func — call as: defer restore().
// Production code reads the vars normally; only _test.go can touch them.
var redditTokenURL, redditOAuthHost = "https://www.reddit.com/api/v1/access_token", "https://oauth.reddit.com"

func OverrideRedditEndpoints(tokenURL, oauthHost string) func() {
	oldT, oldH := redditTokenURL, redditOAuthHost
	redditTokenURL, redditOAuthHost = tokenURL, oauthHost
	return func() { redditTokenURL, redditOAuthHost = oldT, oldH }
}

// Exposed for the wire-level chain test's handler assertions, if needed.
var _ = http.MethodPost // keep import list honest when implementation lands
```

### New: shared consts in `vertical/credentials_test.go` (decision 6)

```go
const okTokenJSON = `{"access_token":"t0k","expires_in":3600,"token_type":"bearer"}`
const badTokenJSON = `{"error":"unauthorized"}` // no access_token → decode-shape error path
```

**Matches real call:** the fake satisfies `vertical.Fetcher` exactly (existing contract); httptest stands in for `https://www.reddit.com` and `https://oauth.reddit.com` via the bridge — production code is never aware which hostnames it talked to.

## 4. Test File List

```
magpie/vertical/
├── credentials.go          # deliverable Z.1: fetchJSONAuth/fetchBytesAuth, githubAuthHeaders, redditToken,
│                           #   unexported redditTokenURL/redditOAuthHost vars
├── credentials_test.go     # NEW: fetchJSONAuth passthrough + non-2xx contract; githubAuthHeaders table;
│                           #   redditToken happy/401/malformed via httptest bridge; wire-level chain test (US-5)
├── github.go               # deliverable Z.2: token threaded into API GETs
├── github_test.go          # EXTEND (2 cases): token on request / no token, no header — rest untouched
├── social.go               # deliverable Z.3: OAuth-first branch + NEW single-listing decoder
├── social_oauth_test.go    # NEW: oauth-first permalink (existing fixture) + listing (NEW inline fixture),
│                           #   partial creds, token failure no-fallback, tokenless ladder-order regression (US-1/3/4)
├── export_test.go          # NEW bridge (package vertical): OverrideRedditEndpoints (decision 1)
├── vertical_test.go        # EXTEND additively: reqLog field + reqLog() accessor (decision 3); existing
│                           #   tests compile+pass unmodified
└── live_test.go            # EXTEND: TestLiveRedditOAuth (live tag + env-gated skip) — decision 7

magpie/cli/
├── profile.go              # deliverable Z.5: profile store + save/ls/rm command + scrape --as resolution
└── profile_test.go         # NEW: store table (0600/upsert/hostile/rm-missing), masked ls, --as precedence,
                            #   unknown-name error; dirs injected (DefaultConfigDir unreachable) — US-6
```

## 5. No conftest (Go)

Shared helpers stay scoped where the house puts them: the fake extension lives in `vertical/vertical_test.go` (its definition site), the bridge in `vertical/export_test.go`, consts in `vertical/credentials_test.go`. New helper worth pinning now:

```go
// setRedditCreds(t, id, secret, ua string) — t.Setenv wrapper; pass "" to
// omit a var (drives the partial-creds table without three copy-pastes).
func setRedditCreds(t *testing.T, id, secret, ua string) {
	t.Helper()
	if id != "" { t.Setenv("MAGPIE_REDDIT_CLIENT_ID", id) }
	if secret != "" { t.Setenv("MAGPIE_REDDIT_CLIENT_SECRET", secret) }
	if ua != "" { t.Setenv("MAGPIE_REDDIT_UA", ua) }
}
```

If a third file needs it, move to `credentials_test.go` only (rule-of-three, not before). No `t.Parallel()` in any `t.Setenv` test (decision 5).

## 6. Key Testing Decisions

| Decision | Approach | Rationale |
|----------|----------|-----------|
| Endpoint override via `export_test.go` bridge, not exported prod config | `OverrideRedditEndpoints` returns a restore func | External test package can't see unexported vars (decision 1); an exported prod knob would be attack/config surface for a URL nobody should configure — test-only file keeps both |
| Token POST bypasses `Fetcher` | `net/http` direct in `redditToken`; httptest fake at that layer only | `FetchRequest` is GET-shaped; adding Method/Body to fetch/ to test a POST would be the tail wagging the dog (decision 2) — and keeps `git diff --stat fetch/` empty as a CI-adjacent gate |
| Fake logs whole `FetchRequest`s, additive | New `reqLog`/`reqLog()`, `order`/`requests()` untouched | Headers are the subject under test (decision 3); touching `order` would ripple through ~40 existing call sites for zero assertive gain |
| Two header-assert altitudes, asymmetrically | Fake-level for every path; ONE wire-level httptest through real `StaticFetcher` | fetch/ already owns header application (existing crates test); we assert OUR passthrough everywhere and the full chain once — re-testing fetch/ internals per vertical is duplicate goldens |
| Empty-string env = unset for GitHub token | `githubAuthHeaders` returns nil unless non-empty | An `Authorization: Bearer ` empty header on the wire is a silent 401 factory; the loud path (no header → tokenless behavior) is the correct degenerate case |
| No-fallback asserts read `reqLog()` | Failure cases assert zero page GETs after token failure | US-4's real risk is silent degradation; "error non-nil" alone passes even if a fallback fired first — request-log makes the promise checkable |
| Partial-creds check precedes any request | `reqLog()` empty + error names vars | Fail at the trust boundary before spending a request; also proves the check lives in `extractReddit`, not buried in `redditToken` |
| Live smoke reuses `live` tag + env skip | `//go:build live` + `if creds missing { t.Skip }` | Only drift alarm for Reddit's grant/.json shapes (decision 7); default suite stays hermetic per AGENTS.md |
| `git diff --stat fetch/` empty as a run-command gate | §9 | Phase-Z.md's "fetch/ untouched" promise, made mechanical |
| Profile tests inject the config dir, never call `DefaultConfigDir()` | `dirs`-style struct handed to the store/command, phase-Y init pattern | Same seam that keeps `$HOME` out of init tests keeps real profiles out of profile tests; also lets `ls`-masking asserts run against known values |
| `ls` masking asserted negatively | Put the FULL secret in the profile, assert `!strings.Contains(stdout, secret)` AND presence of first4…last4 fragment | A masking test that only checks the fragment can pass while the secret leaks elsewhere in the line |
| Listing decoder test uses a NEW inline fixture, not a converted HTML fixture | Small single-listing `.json` const (children t3/t5) | No listing `.json` fixture exists (current path is HTML-only) — converting the HTML golden would test the wrong shape |

## 7. Example Test Case

```go
// vertical/social_oauth_test.go — the US-3 heart: token grant shape + oauth-first
// + header chaining, all hermetic.
package vertical_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

func TestReddit_OAuthFirst_Permalink(t *testing.T) {
	// Token endpoint: capture the grant, serve a bearer.
	var gotAuth, gotUA, gotGrant string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		_ = r.ParseForm() //nolint:errcheck // httptest local
		gotGrant = r.PostFormValue("grant_type")
		_, _ = w.Write([]byte(okTokenJSON)) //nolint:errcheck // httptest local
	}))
	defer tokenSrv.Close()

	// Oauth host: serve the committed fixture under the permalink path.
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".json") {
			t.Errorf("path %q, want .json suffix", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer t0k" {
			t.Errorf("Authorization = %q, want Bearer t0k (token from the POST)", got)
		}
		_, _ = w.Write(verticalFixture(t, "reddit-thread.json")) //nolint:errcheck // httptest local
	}))
	defer oauthSrv.Close()

	restore := verticalOverrideRedditEndpoints(t, tokenSrv.URL, oauthSrv.URL)
	defer restore()
	setRedditCreds(t, "cid", "sekret", "linux:magpie:v1 (by /u/tester)")

	ex, _ := vertical.Lookup("reddit")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"oauthSrv-placeholder": {body: []byte("x")}, // fake unused for GETs — real httptest serves them via bridge
	}}
	_ = fx // Extract's Fetcher is unused on the oauth-first GET path when the bridge points at httptest
	rec, err := ex.Extract(t.Context(), fetch.NewStaticFetcher(fetch.AllowPrivate), mustURL(t,
		"https://www.reddit.com/r/golang/comments/abc/post_title/"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["kind"] != "post" || rec["title"] == "" {
		t.Errorf("record = %#v, want kind=post + title (existing decoder shape)", rec)
	}

	// Grant shape (US-3): basic client_id:secret + grant_type + namespaced UA.
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:sekret"))
	if gotAuth != wantBasic {
		t.Errorf("token Authorization = %q, want %q", gotAuth, wantBasic)
	}
	if gotGrant != "client_credentials" {
		t.Errorf("grant_type = %q, want client_credentials", gotGrant)
	}
	if !strings.Contains(gotUA, "(by /u/tester)") {
		t.Errorf("token UA = %q, want namespaced reddit UA", gotUA)
	}
}

// verticalOverrideRedditEndpoints adapts the unexported bridge to the external
// test package via the reflection-free exported hook in export_test.go.
func verticalOverrideRedditEndpoints(t *testing.T, tokenURL, oauthHost string) func() {
	t.Helper()
	return verticalOverride(tokenURL, oauthHost) // = vertical.OverrideRedditEndpoints
}
```

> Implementation note for the writer: the exact call above is `vertical.OverrideRedditEndpoints(tokenSrv.URL, oauthSrv.URL)` — the wrapper exists only if the bridge takes the `*testing.T` for cleanup; pick one shape in `export_test.go` and keep test + bridge consistent. The `Fetcher` passed to `Extract` on the OAuth path still routes the `.json` GET through production code — if the implementation resolves `redditOAuthHost` before calling `f.Fetch`, the fake fetcher is bypassed naturally; if it routes through the Fetcher instead, point the fake at the httptest URL as key and drop `StaticFetcher`. **Decide when writing `social.go`, then pin the chosen wiring here — both are one-line changes.**

## 8. Execution Prompt

Copy everything between the `---` lines into a new pi session to write this test suite:

---

You are writing the test suite for Phase Z of magpie — credential seam for API-first verticals, including Z.5 saved credential profiles. Implementation (`vertical/credentials.go`, `github.go`, `social.go` changes, `cli/profile.go` + the scrape `--as` flag) is being built in the same pass; these tests are its acceptance gate.

### What This Project Is
magpie: Go 1.26 CLI web scraper (module `github.com/motherlodelab/magpie`), local-first, CGO-free. This phase adds env-var credentials: `MAGPIE_GITHUB_TOKEN` (Bearer on github API GETs) and `MAGPIE_REDDIT_CLIENT_ID`/`MAGPIE_REDDIT_CLIENT_SECRET`/`MAGPIE_REDDIT_UA` (Reddit app-only OAuth `client_credentials` → Bearer GETs against `oauth.reddit.com{path}.json`, replacing the unreliable anonymous ladder when creds exist). Read `AGENTS.md` and `.pi/rules/testing.md` first. All default tests hermetic; the only live-network test is `-tags live` + env-gated.

### Acceptance Criteria (from User Stories)

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | Tokenless = byte-identical behavior | `TestReddit_Tokenless_Unchanged`, `TestGithub_Tokenless_NoAuthHeader` | no auth headers in request log; old.reddit ladder order intact; existing tests pass unmodified |
| US-2 | GitHub PAT on API requests | `TestGithubAuthHeaders`, `TestGithub_TokenOnRequest` | Bearer header present iff token non-empty; empty string = unset |
| US-3 | Reddit OAuth-first | `TestReddit_OAuthFirst_Permalink`, `TestReddit_OAuthFirst_Subreddit` | token POST: basic `client_id:secret`, `grant_type=client_credentials`, namespaced UA; GET: `oauth.reddit.com…json` + `Bearer <token>`; parses via existing decoders |
| US-4 | Loud failure, no silent fallback | `TestReddit_PartialCreds`, `TestReddit_TokenFailure` | error names missing vars / grant step; zero page GETs in the request log on failure |
| US-5 | Headers reach the wire | `TestAuthHeadersReachTheWire` | httptest server observes `Authorization` through real `StaticFetcher` (explicit `AllowPrivate`) |
| US-6 | Saved credential profiles usable from the CLI without secrets in shell history | `TestProfile_SaveLsRm`, `TestProfile_LsMasksSecrets`, `TestScrape_AsResolvesProfile` | profiles.json 0600 under injected dir; `--as` injects like the flags, explicit flags win field-by-field; `ls` never prints a full secret; unknown name errors listing known names |

### Why Fakes Are Required
Reddit's token endpoint and oauth hosts are external services: default-suite tests must never touch them (hermetic + deterministic). The fake layer is two-piece: (1) `fakeVerticalFetcher` (extended to log full `fetch.FetchRequest`s — headers are the subject under test) for extractor logic; (2) `httptest` servers reached through a test-only `export_test.go` bridge (`OverrideRedditEndpoints`) because all `vertical/` tests are `package vertical_test` and cannot see unexported endpoint vars. The token POST uses `net/http` directly (FetchRequest is GET-shaped); everything else rides the `Fetcher`. The Z.5 profile layer needs NO fakes — it is pure logic over an injected config dir (phase-Y init `dirs` pattern); faking the filesystem would only re-test os.ReadFile.

### What NOT to Test
- `fetch/` header application internals — already covered by the existing crates-UA test; we assert passthrough once at the wire, per-vertical at the FetchRequest level
- Reddit's real API shapes beyond committed fixtures — the `-tags live` smoke owns drift
- OAuth crypto — `client_credentials` is one POST with basic auth; there is nothing to unit-test in a library we didn't write (and no library: stdlib only)
- CLI flag plumbing — env is read per-call via `os.Getenv`; there is no flag surface in this phase
- `goquery`/JSON decoding of page bodies — existing fixture tests own it; reuse their fixtures
- cobra's flag machinery itself — assert OUR flags produce OUR behavior (one unknown-name error case, one missing-flag case max)
- `DefaultConfigDir()` OS-specific path resolution — unit tests inject dirs; the resolution function is a one-liner exercised implicitly by command-level tests

### Critical: Fake Implementations

Extend `fakeVerticalFetcher` in `vertical/vertical_test.go` ADDITIVELY (existing `order`/`requests()` frozen for ~40 call sites): add `reqLog []fetch.FetchRequest` field, append `req` inside `Fetch()` under the existing mutex, add a `reqLog() []fetch.FetchRequest` copy-accessor. Create `vertical/export_test.go` (`package vertical`):

```go
var redditTokenURL, redditOAuthHost = "https://www.reddit.com/api/v1/access_token", "https://oauth.reddit.com"

func OverrideRedditEndpoints(tokenURL, oauthHost string) func() {
	oldT, oldH := redditTokenURL, redditOAuthHost
	redditTokenURL, redditOAuthHost = tokenURL, oauthHost
	return func() { redditTokenURL, redditOAuthHost = oldT, oldH }
}
```

Production `credentials.go` reads those vars (never hardcodes the URLs). Inline consts: `okTokenJSON = {"access_token":"t0k","expires_in":3600,"token_type":"bearer"}` and a no-access_token malformed variant. Env via `t.Setenv` ONLY (no `t.Parallel()` in those tests — process-global env). Never scrub env manually; never read real `$HOME`/network in default tests.

### Test Files to Create

```
magpie/vertical/
├── credentials_test.go   # NEW: fetchJSONAuth passthrough+non-2xx; githubAuthHeaders table;
│                         #   redditToken happy/401/malformed (httptest via bridge); wire-level chain test
├── social_oauth_test.go  # NEW: oauth-first permalink (existing fixture) + listing (NEW inline single-listing
│                         #   fixture — none exists today); partial creds; token failure no-fallback; tokenless regression
├── export_test.go        # NEW bridge (package vertical) — above
├── github_test.go        # EXTEND: +2 cases (token on request / tokenless no header)
├── vertical_test.go      # EXTEND: fake reqLog (additive only)
└── live_test.go          # EXTEND: TestLiveRedditOAuth — //go:build live, skip unless all 3 env vars set

magpie/cli/
└── profile_test.go       # NEW: store table (save/upsert/0600/hostile JSON/rm-missing), masked ls,
                          #   --as precedence + unknown-name error — dirs injected, phase-Y init pattern
```

### Per-File Coverage Guidance

#### vertical/credentials_test.go
- **`TestFetchJSONAuth_ForwardsHeaders`** — fake fetcher; headers appear verbatim + in order in `reqLog()[0].Headers`; nil-headers call produces a request byte-identical in shape to `fetchJSON`'s.
- **`TestFetchJSONAuth_Non2xxNamesURL`** — fake returns 403; error contains the URL and `403` (existing fetchBytes contract, now on the auth path).
- **`TestGithubAuthHeaders_Table`** — unset→nil; `""`→nil (empty is NOT a credential); `"ghp_x"`→`["Authorization: Bearer ghp_x"]`.
- **`TestRedditToken_Happy`** — httptest token server via `OverrideRedditEndpoints` (+defer restore); assert `Authorization: Basic b64(cid:sekret)`, `grant_type=client_credentials` POST form value, namespaced UA; returns `"t0k"`.
- **`TestRedditToken_FailureShapes`** — 401 → error mentions status + "token"; 200 + malformed body → error mentions decode. Endpoint vars restored via defer in both.
- **`TestAuthHeadersReachTheWire`** — real `fetch.NewStaticFetcher` with explicit private-allow option, httptest echo server returning `{"ok":true,"auth":<observed header>}` as JSON; `fetchJSONAuth` decodes; observed == injected. ONE such test (decision: wire proven once).

#### vertical/social_oauth_test.go
- **`TestReddit_OAuthFirst_Permalink` / `_Subreddit`** — skeleton in phase-Z-tests.md §7: token httptest + oauth httptest (fixture `reddit-thread.json` / a listing fixture from existing testdata) + `setRedditCreds`; assert GET path ends `.json` on the oauth host, Bearer == token from the POST, record `kind`+title/name from the EXISTING decoders.
- **`TestReddit_PartialCreds`** — table (id-only, id+secret, secret-only): error names EACH missing var (`MAGPIE_REDDIT_SECRET`, `MAGPIE_REDDIT_UA`); `reqLog()` empty — check fires before any request.
- **`TestReddit_TokenFailure`** — token server 401: Extract errors; `reqLog()` contains zero page GETs (no silent anonymous fallback).
- **`TestReddit_Tokenless_Unchanged`** — no env: first request is old.reddit (ladder order via `requests()`), zero auth headers; permalinks still `.json`-retry second.
- Every `t.Setenv` test: no `t.Parallel()`.

#### vertical/github_test.go (extend, don't rewrite)
- **`TestGithub_TokenOnRequest`** — creds via `t.Setenv`; existing fixture served; `reqLog()` last request carries the Bearer header.
- **`TestGithub_Tokenless_NoAuthHeader`** — US-1 regression: existing happy-path test's request log shows no Authorization key.

#### vertical/live_test.go (extend)
- **`TestLiveRedditOAuth`** — already `//go:build live`; add `t.Skip` unless all three `MAGPIE_REDDIT_*` set; extract a REAL permalink, assert `kind=post` + non-empty title. Loose asserts only (live captures drift).

#### cli/profile_test.go
- **`TestProfileStore_SaveUpsert`** — save twice with different values: file decodes to ONE entry with the LAST values; perms 0600 (GOOS-guarded); second save never widens perms of a pre-made 0644 file (pick the preserve-vs-rewrite behavior in code, then pin it — same ruling as phase-Y decision 7).
- **`TestProfileStore_HostileJSON`** — `{"profiles": "x"}` / invalid entry shapes: error, file bytes unchanged (sentinel compare, phase-Y pattern).
- **`TestProfile_RmMissing`** — `profile rm nope`: non-zero exit, error names the profile.
- **`TestProfile_LsMasksSecrets`** — save a profile whose cookie contains `supersecretvalue123`; `ls` stdout must NOT contain the full secret AND must contain the first4…last4 masked fragment (negative assert — fragment-only checks can pass while the secret leaks elsewhere in the line).
- **`TestScrape_AsResolvesProfile`** — options-level: profile carries cookies+headers, flags unset → both injected; `--cookies` flag set AND profile has cookies → FLAG value wins field-by-field (house precedence flags > file); profile headers pass the same ValidateOptions policing (a control-character header from a hand-edited file errors loudly).
- **`TestScrape_AsUnknownName`** — `--as nope`: error contains `nope` AND every saved profile name (mirrors phase-Y init's unknown-client listing).
- ALL profile tests: config dir injected via the `dirs`-style struct (phase-Y init pattern) — `DefaultConfigDir()` never called from test code; no `t.Parallel()` on any test that touches the store file if it uses a shared dir (each test gets its own `t.TempDir()` instead).

### Data Model Notes
- `fetch.FetchRequest.Headers` is `[]string` of raw `"Name: value"` lines — assert with exact string equality per line, not substring hunts.
- `vertical.Extractor.Extract` returns `map[string]any` — assert `rec["kind"]`, `rec["title"]` presence, not full-map DeepEqual (decoder adds derived fields).
- The bridge returns a `func()` restore — always `defer restore()` immediately; two tests sharing leaked endpoints is the #1 flake vector in this suite.
- `profiles.json` decodes to `map[string]struct{ Cookies string; Headers []string }` (plus created timestamp if the implementation stores one — assert presence, not value). Masking applies to BOTH cookies and every header line.

### Success Criteria
- `go test ./vertical/ -count=1` exits 0, <2s, zero external network (verify: no DNS in default run)
- `go test ./... -count=1 && go vet ./... && gofmt -l .` clean; `golangci-lint run ./...` clean
- `git diff go.mod` empty AND `git diff --stat fetch/` empty (both phase promises, mechanical)
- Every existing vertical test passes UNMODIFIED (additive-only guarantee) — `git diff vertical/vertical_test.go` shows only the reqLog addition
- Every US-1..US-6 row has its named test passing
- `go test ./cli/ -run TestProfile -v` exits 0 <1s; profile tests reference no real `$HOME` (grep `DefaultConfigDir()` in profile_test.go → 0 hits outside the injection seam)

---

---

## Run Commands

```bash
# This phase's gate (every push, rides go test ./...)
go test ./vertical/ -count=1 -v

# Full suite + phase promises (fetch/ untouched, no dep changes)
go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./... \
  && test -z "$(git diff go.mod)" && test -z "$(git diff --stat fetch/)"

# Just the new suites
go test ./vertical/ -run 'TestFetchJSONAuth|TestGithubAuth|TestReddit|TestAuthHeaders' -v
go test ./cli/ -run TestProfile -v

# Live Reddit smoke (manual; needs real MAGPIE_REDDIT_* creds; skips without)
go test -tags live ./vertical/ -run TestLiveRedditOAuth -v

# Hermetic-proof: default suite must not touch the network (fails if it does)
go test ./vertical/ -count=1 -v 2>&1 | grep -ci "dial\|lookup" # expect 0
```

## Coverage Check

- [x] Phase type identified and mock strategy stated — integration; two-altitude faking (extended fake fetcher + httptest via bridge); Z.5 is pure logic over an injected dir (no mocks)
- [x] User stories present (6) with binary pass conditions, derived from phase-Z deliverables
- [x] Every user story traces to ≥1 component row (US-1→tokenless rows; US-2→githubAuthHeaders/fetchJSONAuth rows; US-3→redditToken/oauth-first rows; US-4→partial-creds/no-fallback rows; US-5→wire-level row; US-6→profile store/command/`--as` rows)
- [x] Every phase-Z deliverable has a test file: credentials.go→credentials_test.go; github.go→github_test.go extension; social.go→social_oauth_test.go; profile.go+scrape --as→profile_test.go; docs (README, list descriptions)→grep gates in run commands
- [x] Every external dependency (Reddit token endpoint, oauth host) has a fake: httptest via `OverrideRedditEndpoints` bridge — no real network in default tier
- [x] Unit tests reference no real API, real creds, or real network — httptest localhost only, `AllowPrivate` pinned on the one StaticFetcher test
- [x] Integration gating: live smoke behind `-tags live` + env-presence skip (decision 7); nothing else leaves localhost
- [x] conftest section present as "No conftest (Go)" with the `setRedditCreds` helper + placement rule (skill's Python convention adapted, not skipped silently)
- [x] Execution prompt is self-contained: fake code inline (bridge + reqLog spec + consts), what-NOT-to-test, per-file guidance, data-model notes, success criteria — a fresh session needs zero follow-ups
- [x] Run commands section present with gates + hermetic-proof check
