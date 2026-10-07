# Phase Z — Credential seam for API-first verticals (OAuth where it pays)

**Duration:** ~6 hours
**Depends on:** Nothing (master @ 01652f1 — includes #43/#44, the header-seam hardening this plan leans on; registry at 27 verticals)
**Blocks:** Nothing. Optional precondition for future user-context verticals (Notion-style export in magpie-desktop).
**Risk Level:** LOW — additive env-var handling in `vertical/`; no fetch/ changes (the seam exists). cli/ changes are additive only: Z.4 edits two `Desc` strings, Z.5 adds `cli/profile.go` + one `--as` flag on scrape. No new deps (stdlib HTTP + net/url only).
**Stack:** go
**Runner:** manual (the `run-phase` skill hard-blocks on `stack: go` — execute via the execution prompt below)

Source: 2026-09-23 LinkedIn authwall/OAuth investigation (session notes: anonymous company + sales pages 302 → login; developer products self-serve tier = OIDC identity only; API terms §4.1 "No Storing Any Content"; Sales Display = SNAP partner-only). Conclusion carried into this phase: **OAuth is worth a seam only where a provider self-serves credentials that unlock real data.** This phase builds exactly that seam and wires the two providers where it pays today.

---

## 1. Realistic use of OAuth — provider-by-provider verdict

The gate for inclusion is not "supports OAuth" but "credentials are self-serve AND unlock data magpie cannot get tokenless." Verified 2026-09-23:

| Provider | Auth model | Self-serve? | Unlocks for magpie | Verdict |
| :-- | :-- | :-- | :-- | :-- |
| **Reddit** | OAuth2 `client_credentials` (app-only, no user context) | Yes — instant app creation at reddit.com/prefs/apps | Reliable `.json` from datacenter IPs (anonymous reddit .json is IP-blocked in practice); ~100 QPM vs anonymous blocks. UA must be namespaced `<platform>:<app-id>:<version> (by /u/<name>)`. | **IN — Z.3** |
| **GitHub** | PAT (not even OAuth — a header) | Yes | 60 → 5,000 req/h on the existing `github` vertical. | **IN — Z.2** |
| LinkedIn | OAuth2, but every data product is review-gated; self-serve tier is OIDC identity only (own name/email/photo); §4.1 no-storage; Sales/Nav = SNAP partner-only | Apps yes, *products* no | Nothing a scraper needs. Contractually incompatible with persisted output. | **OUT — permanent** (see §5) |
| Notion / Airtable / Linear | OAuth code+PKCE (user context) or PAT | Yes | Export the *user's own* workspace — a real desktop-product fit, but zero verticals exist yet. | Deferred — rule of three |
| Bluesky / Mastodon | App passwords / OAuth-for-write | Yes | Public reads are already tokenless — no win. | OUT |
| YouTube | API key (not OAuth) | Yes | Data-API quota economics lose to the existing player-response scrape. | OUT |

Net: **v1 ships env-var credentials only.** No `magpie auth` subcommand, no browser loopback dance — every IN provider is credential-in-env shaped. Authorization-code+PKCE waits until a user-context vertical actually exists (it would also be the natural magpie-desktop settings surface; core stays terminal-free).

## 2. Key design decisions

```
user env (MAGPIE_GITHUB_TOKEN / MAGPIE_REDDIT_{CLIENT_ID,SECRET,UA})
        │
        ├─ vertical/credentials.go ──► header []string (nil when unset)
        │
        ├─ github.go    GET api.github.com  + "Authorization: Bearer …"   (Z.2)
        └─ reddit.go    POST /api/v1/access_token (basic client_id:secret,
                        grant_type=client_credentials) → Bearer token,
                        GET oauth.reddit.com{path}.json FIRST;
                        existing permalink decoder + NEW listing decoder;
                        old.reddit HTML ladder stays as fallback          (Z.3)
```

1. **The injection seam already exists**: `fetch.FetchRequest.Headers []string` (raw `Name: value`, caller wins). `fetch/` is untouched. `vertical/` gains a `fetchJSONAuth(ctx, f, url, headers ...string)` variant; `fetchBytes`/`fetchJSON` delegate to it (no duplication).
2. **Credentials are read per-call via `os.Getenv`** — no config threading through `Extractor` signatures, `t.Setenv`-testable, matches MAGPIE_ env convention. Absent vars = nil headers = exact current behavior. Zero regression surface.
3. **Reddit token lifetime vs CLI shape**: access tokens expire in ~1 h; magpie invocations are short. **Fetch a token per `extractReddit` call that needs one** — no refresh logic, no storage, no cache.
   `ponytail:` ceiling = one extra ~200 ms RTT per reddit page in `crawl` mode; upgrade path = package-level token cache keyed on expiry when a real crawl workload complains.
4. **Reddit endpoints swap by credential presence**: with creds, `.json`-first against `oauth.reddit.com` for BOTH permalinks and listings (anonymous listings are the unreliable path); tokenless fallback remains today's ladder. Token failures degrade loudly (error names the grant step) — never silent fallback to anonymous, which would hide a misconfigured env.
5. **Saved credential profiles (Z.5)** generalize "bring your own credential" beyond the two named providers: `<DefaultConfigDir>/profiles.json` (0600) maps a name → `{cookies, headers[]}`; `magpie scrape --as <name>` injects them exactly as the `--cookies`/`--header` flags would, explicit flags winning field-by-field (house precedence: flags > file). This covers any site whose login reduces to a static session header — without becoming session automation: nothing captures, refreshes, or logs in.

## 3. Tasks

### Z.1 — `vertical/credentials.go` + header-carrying fetch helper (~1 h)
- `fetchJSONAuth` (+ `fetchBytesAuth`); existing `fetchBytes`/`fetchJSON` become `nil`-header wrappers.
- `githubAuthHeaders()` → `[]string` bearer header when `MAGPIE_GITHUB_TOKEN` is set.
- `redditToken(ctx)` → bearer string; hard error naming the step on non-200/undecodable. Bare `http.Post`, not the Fetcher — `fetch.FetchRequest` is GET-only by design and adding a Method field for one OAuth call is fetch/ creep.
  `ponytail:` the token hop deliberately bypasses the proxy pool (only page fetches ride the Fetcher); upgrade path = Method field in fetch/ if a second token-style POST ever appears.
- Unexported package vars `redditTokenURL` / `redditOAuthHost` for hermetic tests (the token POST is the one request the fake fetcher can't intercept — it never goes through the Fetcher).
- Extend `fakeVerticalFetcher` to record `fetch.FetchRequest.Headers` (today it captures URLs only) — tests §4.1–4.3 assert on forwarded headers.

### Z.2 — GitHub vertical token passthrough (~30 min)
- Thread `githubAuthHeaders()` into the existing API GETs in `github.go`.
- Tokenless behavior byte-identical; token adds one header, nothing else.

### Z.3 — Reddit OAuth-first path (~2.5 h)
- In `extractReddit`: when `MAGPIE_REDDIT_CLIENT_ID` + `MAGPIE_REDDIT_CLIENT_SECRET` (+ `MAGPIE_REDDIT_UA`) are all set → token → `https://oauth.reddit.com{RequestURI}.json` with Bearer + the required UA.
- Permalinks reuse `redditThreadFromJSON`. Listings need a NEW single-listing decoder (~30–45 min): a subreddit `.json` is ONE listing (children = t3/t5), not the permalink two-listing shape, and today's listing path is HTML-only (`redditSubredditFromHTML`) — nothing to reuse.
- Partial creds (1–2 of 3 set) → error naming the missing vars (fail loudly at a trust boundary).
- Tokenless → today's ladder unchanged.

### Z.4 — Docs (~30 min)
- README "Vertical credentials" note (named to stay grep-distinct from the existing LLM-provider credentials content): the three env vars, what they unlock, where to create apps; explicit "LinkedIn is not supported and why" one-liner pointing here.
- `magpie list` descriptions for reddit/github mention optional creds.

### Z.5 — Saved credential profiles: `magpie profile` + `scrape --as` (~1.5 h)
- `cli/profile.go`: store at `<DefaultConfigDir>/profiles.json`, 0600 on create (never widen an existing file's perms). Subcommands: `save <name> --cookies … --header …` (upsert), `ls` (names + MASKED previews — secrets render as first4…last4), `rm <name>` (errors when missing). Name pattern `[a-zA-Z0-9_-]{1,32}`.
- `cli/scrape.go`: `--as <name>` — unknown name errors listing the known ones; resolution sits at the options boundary and reuses `scrape.ValidateOptions` header policing (no control characters). Explicit `--cookies`/`--header` flags win over profile values field-by-field.
- `crawl` gets `--as` only when crawl grows cookie/header flags of its own (today it has none — verified 2026-09-24).

## 4. Tests (all hermetic — `httptest` + `t.Setenv`; zero live network)

1. `fetchJSONAuth` forwards headers; absent headers → request identical to today (golden-ish assertion on captured request).
2. GitHub: `MAGPIE_GITHUB_TOKEN` set → Authorization header present; unset → absent.
3. Reddit token (httptest server via the unexported vars — not the fake fetcher, which never sees this request): 200 → Bearer used on the `.json` GET with namespaced UA; 401 → error names the grant; missing env var → error names the var.
4. Reddit oauth-first: permalinks parse through the EXISTING `redditThreadFromJSON` (fixture reuse from `social_test.go`); subreddit listings through the NEW single-listing decoder (small inline fixture — single listing, children t3/t5).
5. Tokenless reddit regression: current ladder untouched (existing tests stay green unmodified).
6. Profiles (phase-Y init pattern, injected temp dirs): `save`/`ls`/`rm` incl. masked `ls` output + 0600 perms; `--as` merge precedence (explicit flag wins, profile fills gaps); hostile JSON + unknown name fail loudly with names in the error.

## 5. Non-goals (explicit, so nobody re-litigates)

- **LinkedIn — permanent OUT, verified 2026-09-23.** Anonymous pages 302 → login wall. OAuth self-serve tier grants only the member's own OIDC identity; profile/company/leads products are application-review or SNAP-partner gated; API terms §4.1 forbid storing Content, which is a scraper's entire output. Cookie-automation (PhantomBuster-style) is a different product category: credential custody + ToS violation borne by the user + arms-race maintenance — rejected for magpie and magpie-desktop alike.
- **No `magpie auth` subcommand / loopback redirect / PKCE** — returns when a user-context vertical exists (Notion-class) or desktop settings need it.
- **No keyring, no token persistence, no refresh** — env-file (.magpie.env) is the sanctioned secret path on this platform (no org.freedesktop.secrets here anyway).
- **No new dependency** (no golang.org/x/oauth2 — one POST with stdlib is smaller than the dep).
- **Profiles are not sessions** — no browser-capture flow, no refresh, no keyring; `crawl --as` waits for crawl to grow cookie flags; no v1 command prints profile secrets in full.

## 6. Open questions

- Confirm Reddit's current free-tier QPM (~100) at implementation time; the architecture doesn't hinge on the number.
- Desktop follow-up (magpie-desktop repo): settings fields that write these vars into `.magpie.env` — separate PR there, core untouched.
