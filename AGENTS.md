# AGENTS.md — magpie (`magpie`)

Go CLI web scraper: fetch → clean → extract. Source of truth: `spec.md`.
Binary name: `magpie`. Module: `magpie`. Requires Go 1.26+ (go-trafilatura v2).

## Coding standard — read these

Detailed rules live alongside this file and are imported into context:

@.pi/rules/go.md
@.pi/rules/testing.md

Overarching ethos (lazy senior dev): **the best code is the code never written.** Before
adding anything, climb the ladder — does it need to exist (YAGNI) → does it already exist
here (reuse the helper) → does the stdlib/platform do it → does an installed dep do it →
can it be one line → only then write the minimum. Deletion over addition. Boring over
clever. Shortest working diff that you actually understand. Mark deliberate shortcuts with
a `ponytail:` comment naming the ceiling.

Not lazy about: understanding the problem first, input validation at trust boundaries,
error handling, security.

## Commands

| Command | Action |
| :-- | :-- |
| `go build ./...` | Build all packages |
| `go test ./...` | All tests (fast, hermetic, no network) |
| `go test -tags browser ./...` | Browser suite (needs Chrome, slow) |
| `go vet ./...` | Vet |
| `golangci-lint run ./...` | Lint (errcheck, staticcheck, ineffassign, unused; config `.golangci.yml`) |
| `gofmt -l .` | Must print nothing |

## Structure (per spec §13)

```
cmd/magpie/      # 10-line shim (os.Exit(cli.Execute()))
cli/             # Cobra tree (importable; custom binaries link here)
core/         # module registry, pipeline wiring
fetch/ clean/ extract/ selector/ crawl/ store/ mcp/ plugin/ config/
testdata/     # golden fixtures
plan/         # big-plan.md + phase-N.md planning artifacts
```

## Cross-repo (magpie-desktop)

Sibling: `../magpie-desktop` (PRIVATE Wails3 product shell over this core).
System map (local checkouts): `../SHARED-CONTEXT.md` at the projects root
(sibling of this repo; symlinked inside magpie-desktop, which is private).
Direction is one-way: desktop imports core, core never imports desktop.
Core changes the GUI needs arrive as public PRs here + a tag bump there.
Planning docs for the product live in the desktop repo (`docs/`), not here.

## Don't / gotchas

- **No new dep without asking:** a dependency is a permanent maintenance and supply-chain cost.
- **No live-network tests in the default suite** — gate browser tests with `//go:build browser`.
- **No CGO deps, ever.**
- **Never import go-rod outside `fetch/`** — the browser sits behind the `Fetcher` interface.
- **phase-plan/run-phase skills expect `stack: python|nextjs|react|typescript`** — this repo is Go.
  Write `stack: go` in phase files and expect `run-phase` to hard-block; execute phases manually
  via the execution prompt instead.
