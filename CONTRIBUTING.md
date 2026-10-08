# Contributing

Thanks for considering a contribution to magpie.

## Contribution terms

By submitting a pull request or patch to this repository you agree that:

1. Your contribution is licensed under the project's license —
   [AGPL-3.0-or-later, or the project's commercial license](LICENSE) — at
   the maintainer's choice.
2. You grant the project maintainer a perpetual, worldwide, royalty-free
   right to include, modify, and relicense your contribution as part of
   future project releases — including under different or additional
   licenses. This keeps the project free to change its license as it
   evolves; code already distributed stays available to its recipients
   under the terms in effect when it was distributed.

## Ground rules

- No new dependencies without asking first (a dependency is a permanent
  maintenance and supply-chain cost) — see `AGENTS.md`.
- No CGO dependencies, ever.
- Tests stay hermetic: no live network in the default suite; browser tests
  behind the `browser` build tag.
- Gates before pushing: `go build ./... && go vet ./... && gofmt -l . &&
  golangci-lint run ./... && go test ./...`.
