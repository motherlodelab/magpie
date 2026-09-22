# Phase Y — Agent-skill + installer packaging (`magpie init`)

**Duration:** 1 day (~4 hours)
**Depends on:** Nothing (master @ 57eb3e9; `magpie serve` + 12 MCP tools already shipped — the gap-spec §6 precondition "until MCP tools land" is met)
**Blocks:** Nothing technical. Feeds agent adoption (README, agent-skill distribution) the same way Phase X feeds marketing.
**Risk Level:** LOW — one new CLI command writing JSON config files plus docs. No core packages touched. Only real hazard is clobbering a user's existing client config, mitigated by merge-never-replace semantics (Task Y.1) and temp-dir tests (Task Y.2).
**Stack:** go
**Runner:** manual (the `run-phase` skill hard-blocks on `stack: go` — execute via the execution prompt below)

Source item: `plan/competitive-analysis-2026-09-20.md` item 2 ("Agent-skill + installer packaging — all three competitors ship it"). Closes two matrix rows: "Agent-skill packaging ❌" and "Installs config for you ❌ (copy-paste JSON)". §5.1 from the same backlog (deterministic relocation) already shipped as Phase R (PR #33) — this is the last live backlog item.

---

## 1. Objective + What Success Looks Like

Give magpie the agent-adoption on-ramp webclaw and Firecrawl already have:

1. `magpie init --client <name>` writes the `serve` MCP stanza into the named
   client's config file — **merging** into existing config (never dropping other
   servers or keys), idempotent on re-run.
2. A committed `skill/SKILL.md` teaches coding agents (Claude Code, Cursor, any
   skill-compatible runner) how to drive magpie: CLI verbs, MCP tools, when to
   use which, zero-LLM paths first.
3. README "MCP server" section gains a short "Agent integrations" entry pointing
   at `magpie init` and the skill, replacing the hand-edit instruction.

What success looks like (all observable):

1. In an empty temp dir, `magpie init --client claude-code` creates `.mcp.json`
   containing `{"mcpServers":{"magpie":{"command":"<abs path to magpie>","args":["serve"]}}}`.
2. In a temp dir with an existing `.mcp.json` that has another server
   (`{"mcpServers":{"other":{"command":"x"}}}`), the same command leaves `other`
   byte-identical and adds `magpie`; re-running changes nothing further.
3. `magpie init --client generic` prints the same stanza JSON to stdout and
   writes no file. `--dry-run` forces stdout for every client.
4. `magpie init --client nope` exits non-zero listing the valid clients.
5. `go test ./cli/ -run TestInit -v` exits 0 (merge, idempotence, unknown
   client, per-client path resolution — all hermetic, no network).
6. `skill/SKILL.md` exists with valid frontmatter (name + description) and
   `gofmt -l .` / `go vet ./...` stay clean.

---

## 2. Key Design Decisions

```
magpie init --client X
        │
        ├─ resolveConfigPath(client, dirs) ──► ~/.mcp.json | Claude config | cursor config | stdout
        ├─ buildStanza()  { command: os.Executable(), args: ["serve"] }
        └─ mergeStanza(path, stanza)  read → unmarshal map[string]any → set mcpServers.magpie → write
```

**Data model strategy (Go):**

| Layer | Type | Why |
|-------|------|-----|
| Emitted stanza | `type mcpStanza struct { Command string \`json:"command"\`; Args []string \`json:"args"\` }` | Fixed shape we own; struct keeps field order stable |
| Read-side merge | `map[string]any` | Client configs carry arbitrary other keys (other servers, user settings) — a typed struct would drop them. Unmarshal-into-map, set one nested key, marshal back. |
| Path resolution input | `type dirs struct { cwd, home, configDir string }` | Pure-function seam so tests inject fake dirs instead of faking GOOS |

**Decisions to hold:**

- **Absolute `os.Executable()` path, not bare `magpie`.** GUI clients (Claude
  Desktop) don't inherit your shell PATH — same lesson as GOPATH/bin missing
  from this box's PATH. Bare name stays correct only for terminal-launched
  clients; absolute works for both.
- **Never emit an `env` block.** Keys flow from the user's own environment /
  `~/.magpie.env` / their client config. init must not touch, move, or copy
  secrets. (The repo's own `.mcp.json` carries env for local dev — that's a
  hand-maintained dev file, not init output.)
- **Merge, never replace.** `mcpServers` present but not an object → hard error,
  don't overwrite. Unknown top-level keys survive round-trip via `map[string]any`.
  Write perms 0600 (sibling configs commonly hold API keys).
- **`os.UserConfigDir()` covers claude-desktop on all three OSes**
  (Linux `~/.config`, macOS `~/Library/Application Support`, Windows `%AppData%`)
  → path is always `filepath.Join(configDir, "Claude", "claude_desktop_config.json")`.
- **No `config/` package reuse.** `config.Save` is YAML (`yaml.Node` round-trip);
  client configs are JSON. Different format, different file, owned entirely in
  `cli/init.go`. Don't generalize config/ for a second caller that isn't one.
- **Client set is exactly: `claude-code`, `claude-desktop`, `cursor`, `generic`.**
  `claude-code` = `.mcp.json` in cwd (project scope; the file format this repo
  itself uses). `generic` = stdout only. No user-scope flag, no `--addr`/HTTP
  stanza — stdio is the zero-config integration; HTTP mode stays documented in
  README/`serve --help`. Rule of three: add clients when the third user shows up.

---

## 3. Tasks

### Task Y.1 — `cli/init.go`: stanza writer + merge (1.5h)

Add `newInitCmd()` — cobra command `init`, flags `--client` (required) and
`--dry-run` (bool). Register in `root.go`'s `AddCommand(...)` list.

```go
// Confirmed patterns (in-repo, no research needed):
// stanza shape — .mcp.json at repo root:
//   {"mcpServers":{"magpie":{"command":"/abs/magpie","args":["serve"]}}}
// cobra command — cli/serve.go:
//   cmd := &cobra.Command{Use: "serve", Short: "...", RunE: func(cmd *cobra.Command, args []string) error {...}}
//   cmd.Flags().StringVar(&client, "client", "", "...")
```

- `resolveConfigPath(client string, d dirs) (string, error)`: claude-code →
  `filepath.Join(d.cwd, ".mcp.json")`; claude-desktop →
  `filepath.Join(d.configDir, "Claude", "claude_desktop_config.json")`; cursor →
  `filepath.Join(d.home, ".cursor", "mcp.json")`; generic → error "writes
  nothing; stanza printed to stdout" (caller handles). Unknown → error listing
  valid clients.
- `buildStanza() mcpStanza`: `os.Executable()` (propagate error), `Args:
  ["serve"]`.
- `mergeStanza(path string, s mcpStanza) error`: read file (missing → start
  from `{}`), `json.Unmarshal` into `map[string]any`; if `mcpServers` exists
  and is not `map[string]any` → error; set `mcpServers["magpie"] = s`;
  `json.MarshalIndent` two-space + trailing `\n`; `os.WriteFile(path, ..., 0600)`.
- `--dry-run` or `--client generic`: marshal stanza only, print to stdout.

**Sanity check:** `go run ./cmd/magpie init --client generic` prints the stanza; `echo $?` is 0.

### Task Y.2 — `cli/init_test.go`: hermetic merge tests (1h)

Table-driven, `t.TempDir()`, inject `dirs{cwd: tmp, home: tmp, configDir: tmp}`.
Cases: fresh-file write; merge preserves a sibling server byte-for-byte (decode
both, compare `other` subtree); idempotent re-run (deep-equal before/after);
`mcpServers` non-object → error, file untouched; unknown client → error names
the four valid ones; claude-desktop/cursor paths land under the injected dirs.
Run through the real cobra command where practical (follow `cli/cmd_test.go`
patterns) so flag parsing is covered too.

**Sanity check:** `go test ./cli/ -run TestInit -v` exits 0.

### Task Y.3 — `skill/SKILL.md` + README agent section (1h)

- `skill/SKILL.md`: YAML frontmatter (`name: magpie`, one-line description),
  then: what magpie is (fetch→clean→extract, local-first, zero-LLM by default);
  CLI quick reference (`scrape`, `crawl`, `extract`, `summarize`, `search`,
  `map`, `diff`, `brand`, `vertical` — one line each, point at `--help` for
  flags); MCP tools table (reuse the README table's tool list verbatim — 12
  tools incl. `search`); "prefer zero-LLM verbs and verticals before spending
  tokens"; JSON schema example for `extract_structured`; politeness note
  (per-host 1 rps, keep `MaxPages` bounded); setup line:
  `magpie init --client <claude-code|claude-desktop|cursor>` or copy the stanza.
- README `## MCP server` section: replace the hand-edit Claude Desktop config
  paragraph with a 4–6 line "Agent integrations" passage: run
  `magpie init --client ...`, supported clients, skill pointer
  (`skill/SKILL.md`), HTTP-mode one-liner stays. Don't add a new H2 (keeps TOC
  and anchor links stable).

**Sanity check:** `head -8 skill/SKILL.md` shows valid frontmatter; `grep -c "init --client" README.md` ≥ 1.

### Task Y.4 — wire-up + full gates (0.5h)

`go build ./... && go vet ./... && golangci-lint run ./... && go test ./...`,
`gofmt -l .` prints nothing. Skim `magpie init --help` output for flag doc
quality.

**Sanity check:** all gates green, then commit on branch `phase-y-agent-packaging`.

---

## 4. Deliverables

```
magpie/
├── cli/
│   ├── init.go           # newInitCmd, resolveConfigPath, buildStanza, mergeStanza (~120 lines)
│   ├── init_test.go      # hermetic merge/path/idempotence tests
│   └── root.go           # +newInitCmd() in AddCommand list (1-line diff)
├── skill/
│   └── SKILL.md          # agent-skill doc: frontmatter + CLI/MCP reference + zero-LLM guidance
└── README.md             # "MCP server" section: Agent integrations passage replaces hand-edit para
```

---

## 5. Exit Criteria

- [ ] `magpie init --client claude-code` in a clean dir creates `.mcp.json` with the exact stanza (criterion 1)
- [ ] Existing sibling server survives merge; re-run is a no-op (criterion 2)
- [ ] `--client generic` / `--dry-run` print to stdout, write nothing (criterion 3)
- [ ] Unknown client exits non-zero with valid-client list (criterion 4)
- [ ] `go test ./cli/ -run TestInit -v` exits 0, all cases hermetic (criterion 5)
- [ ] `skill/SKILL.md` valid frontmatter + covers all 12 MCP tools; README updated; full gates green (criterion 6)

---

## Execution Prompt

Copy everything between the `---` lines into a new pi session to implement this phase:

---

You are building Phase Y of magpie — agent-skill + installer packaging.

### What This Project Is
magpie is a Go 1.26 CLI web scraper (fetch → clean → extract), single static
binary, CGO-free, module `github.com/motherlodelab/magpie`, binary `magpie`.
Local-first, zero-LLM by default, LLM verbs are BYOK. Repo root: the working
directory. Coding rules are in AGENTS.md + `.pi/rules/go.md` — read them first.
Ethos: minimum diff, no new deps, no abstractions with one caller.

### Established in Prior Phases (verified on master @ 57eb3e9)
- `magpie serve` already serves 12 MCP tools (scrape_url, crawl_site,
  extract_structured, get_cached_selectors, batch, map, summarize, diff, brand,
  list_extractors, vertical_scrape, search) over stdio or stateless Streamable
  HTTP — `cli/serve.go`, tools in `mcp/tools.go`. Nothing in this phase touches
  mcp/ or serve.
- The MCP stanza shape is proven by the repo's own `.mcp.json`:
  `{"mcpServers":{"magpie":{"command":"<abs path>","args":["serve"]}}}`.
  init output = same shape, WITHOUT the env block (init never handles secrets).
- CLI pattern: cobra commands in `cli/*.go`, registered in `root.go`'s
  `AddCommand(...)` list; `RunE` returns error; see `cli/serve.go` for flag style.
- `config/` package is YAML-only (`config.Save` round-trips via yaml.Node) —
  do NOT reuse it; client configs are JSON, owned in `cli/init.go`.
- Test conventions: hermetic only (no network), `t.TempDir()`, see
  `cli/cmd_test.go` for command-level test patterns.

### Your Goal for This Phase
`magpie init --client <name>` writes/merges the magpie MCP stanza into the
named client's config; `skill/SKILL.md` + a README "Agent integrations" passage
teach agents the toolset.

### Data Model Rules (follow exactly)
- Emitted stanza: `type mcpStanza struct { Command string; Args []string }` with
  json tags `command`/`args` — fixed shape we own.
- Read-side merge: `map[string]any` — client configs hold arbitrary user keys;
  a typed struct would drop them. Set one nested key (`mcpServers` → `magpie`),
  marshal back with `json.MarshalIndent(v, "", "  ")` + trailing newline.
- Path resolution: `type dirs struct { cwd, home, configDir string }` passed
  into `resolveConfigPath` so tests inject fakes instead of faking GOOS.

### Architecture
Single file `cli/init.go`:
- `resolveConfigPath(client string, d dirs) (string, error)`:
  claude-code → `filepath.Join(d.cwd, ".mcp.json")`; claude-desktop →
  `filepath.Join(d.configDir, "Claude", "claude_desktop_config.json")` (works
  on all OSes via `os.UserConfigDir()`); cursor →
  `filepath.Join(d.home, ".cursor", "mcp.json")`; generic → error (stdout only);
  unknown → error listing the four valid clients.
- `buildStanza() (mcpStanza, error)`: `os.Executable()` for command (absolute —
  GUI clients don't inherit shell PATH), `Args: []string{"serve"}`.
- `mergeStanza(path string, s mcpStanza) error`: missing file → `{}`;
  unmarshal into `map[string]any`; `mcpServers` present but not an object →
  error, leave file untouched; set the `magpie` entry; write 0600. Merge only
  the magpie key — every other byte of user config survives (via round-trip).
- Command wiring: `--client` (required), `--dry-run` (print stanza to stdout
  instead of writing; `generic` always prints). Register `newInitCmd()` in
  `root.go`.

### Files to Create
- `cli/init.go` — everything above, ~120 lines, stdlib only (cobra is the one
  permitted existing dep). No backup files, no HTTP-mode stanza, no user-scope
  flag, no config-package reuse.
- `cli/init_test.go` — table-driven, `t.TempDir()` with injected `dirs`:
  fresh write; merge preserves a sibling server; idempotent re-run;
  `mcpServers: "string"` → error + file untouched; unknown client error;
  per-client path placement. Drive the real cobra command where practical.
- `skill/SKILL.md` — frontmatter (name: magpie + one-line description), then:
  what magpie is; CLI verb one-liners (`scrape`, `crawl`, `extract`,
  `summarize`, `search`, `map`, `diff`, `brand`, `vertical`); the 12 MCP tools
  (copy the README table's rows); "prefer zero-LLM verbs and verticals before
  spending tokens"; one `extract_structured` JSON-schema example; politeness
  (per-host 1 rps, bounded MaxPages); setup via `magpie init --client ...`.
- `README.md` — in `## MCP server`, replace the hand-edit-Claude-Desktop-config
  paragraph with a 4–6 line Agent integrations passage (init command, client
  list, skill pointer, HTTP-mode one-liner). No new H2 heading (TOC stability).
- `cli/root.go` — add `newInitCmd()` to the AddCommand list (one line).

### Success Criteria
- Clean-dir init creates the exact stanza; sibling-server merge preserves it;
  re-run idempotent; generic/dry-run stdout-only; unknown client errors with
  the valid list
- `go build ./... && go vet ./... && go test ./...` green; `gofmt -l .` empty;
  `golangci-lint run ./...` clean
- All work on branch `phase-y-agent-packaging`

---

## Readiness Check

- [PASS] All inputs from prior phases are listed and available — `cli/serve.go`, `mcp/tools.go` (12 tools), stanza shape (repo `.mcp.json`), cobra pattern, and the README `## MCP server` section were all verified on master (57eb3e9) this session; `magpie init` does not yet exist (grep confirmed)
- [PASS] Every sub-task has a clear, testable completion condition — each task ends in a one-line sanity check (Y.4 = the full gate suite)
- [PASS] Execution prompt is self-contained: (a) prior-phase facts inline (serve/12 tools/stanza shape/config-is-YAML warning/test conventions), (b) confirmed snippets inline (stanza JSON, cobra RunE pattern, exact file paths per client), (c) Data Model Rules section present, (d) per-file guidance for all five files, (e) observable success criteria with exact commands
- [PASS] Exit criteria map 1:1 to deliverables — init.go covered by criteria 1–4, init_test.go by 5, SKILL.md/README by 6, root.go's one-line registration exercised by every command-level test + the build gate
- [PASS] Heavy external dependency strategy — none exist; stdlib `encoding/json` + already-vendored cobra only; tests fully hermetic via injected `dirs` + `t.TempDir()`
- [PASS] New libraries — none; no research step needed (cobra usage confirmed from in-repo `cli/serve.go`, not docs)
