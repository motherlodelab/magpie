# Go rules — magpie (`magpie`)

- Go 1.26+ required (go-trafilatura v2). Toolchain: `go build`, `go test`, `go vet`,
  `gofmt`, `golangci-lint` (see `.golangci.yml`).
- Stdlib first: `net/http`, `net/http/cookiejar`, `context`, `encoding/json`.
  Concurrency: `golang.org/x/sync/errgroup` + per-host token buckets (`golang.org/x/time/rate`).
- **Dependencies: `go.mod` is the single source of truth for versions** — never copy a
  version from docs or this file; check `go.mod`. Policy only: no new direct require
  without asking (permanent supply-chain cost); pure-Go, no CGO ever; must build with
  `CGO_ENABLED=0` for windows/amd64, linux/amd64, darwin/arm64. Sharp edge: when bumping
  `modernc.org/sqlite`, keep `modernc.org/libc` on the exact version its go.mod wants.
  Sanctioned choices (names only — spec §13 + promoted helpers): cobra, rod, go-trafilatura,
  html-to-markdown, jsonschema/v6 + jsonschema-go, go-keyring, goquery/cascadia/go-shiori-dom,
  backoff, grobotstxt, bloom, mcp go-sdk, wazero, modernc sqlite, impersonate-http/utls,
  klauspost/brotli, ledongthuc/pdf.
- **Layering — dependency arrows point one way, surface → orchestration → stages → leaves.**
  Leaves `clean extract store config plugin` import no magpie packages (keep portable:
  the GUI port depends on it). Orchestration (`crawl`, `scrape` via its `Deps` seam, and
  `research` above both — it may import `scrape`/`crawl`, never the reverse) may
  import stages; `mcp` and `cli` are surfaces that import inward; only `build` (codegen)
  and `cmd` import `cli`; nothing imports `mcp`. rod is imported only inside `fetch/`,
  behind the `Fetcher` interface — static fetch first, browser escalation on detect
  score ≥ 2 (spec §1.2).
- **Errors:** one exported sentinel per domain package (`clean.ErrQuality` style), message
  prefixed `pkg: reason`; always wrap with `%w`; typed errors only when callers need
  `errors.As`. A new user-visible error must be wired end to end: sentinel in the domain
  package → `errors.Is/As` arm in `cli.exitCode` (the ONLY place exit codes are decided —
  handlers never re-map) → README exit-code table → an asserting test.
- **Concurrency:** every goroutine has an owner that waits (errgroup/WaitGroup); `ctx` is
  the first parameter; no bare `go` statements in library packages; `time.Sleep` only
  inside documented retry/backoff/settle paths. `go test -race` must stay clean on any
  package you touch (see testing.md).
- **Hygiene:** new files stay under ~500 lines; the known exceptions (`crawl/crawl.go`,
  `store/sqlite.go`) are cohesive state machines — don't grow them, factor out instead.
  Panics only for startup config or broken invariants, never across package boundaries.
  `any`, never `interface{}`. No TODO/FIXME comments — either do it, or it doesn't exist.
- External input validated at the boundary; fail loudly, never swallow errors.
  Mark deliberate shortcuts with a `ponytail:` comment naming the ceiling.
