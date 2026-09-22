# Phase Y — Testing: Agent-skill + installer packaging (`magpie init`)

**Scope:** the new `cli/init.go` command surface — `resolveConfigPath` (per-client path placement), `buildStanza` (absolute-path + `["serve"]` shape), `mergeStanza` (merge-never-replace JSON semantics, 0600 perms, loud failure on hostile shapes) — plus the cobra wiring (`--client`/`--dry-run`, root registration) and the docs gates for `skill/SKILL.md` + the README "Agent integrations" passage.
**Key Pattern:** **No fakes, no mocks** (pure logic phase — the subject is stdlib `encoding/json` manipulation + path joins + one cobra command, all testable hermetically). Every test injects a `dirs` struct pointing into `t.TempDir()`; `os.UserHomeDir`/`os.UserConfigDir`/`os.Getwd` are never consulted by unit tests; the only command-level tests run the real `rootCmd()` tree and capture stdout, matching the `cli/cmd_test.go` house pattern.
**Dependencies:** stdlib only: `testing`, `os`, `path/filepath`, `encoding/json`, `reflect`, `strings`, `runtime` (GOOS guard for the perms assert) + in-repo `cli` (`rootCmd`, `resetGlobals`, `captureOutput`) + `cobra`. **No new deps; `git diff go.mod` empty.**

**Decisions pinned while verifying seams** (test-plan additions to phase-Y.md, discovered by reading `cli/cmd_test.go` / `cli/serve.go` / repo `.mcp.json` on master @ 57eb3e9, 2026-09-24):

1. **`os.Executable()` inside `go test` returns the test binary, not `magpie`.** So stanza assertions must compare against `os.Executable()` evaluated at test time — never hardcode a binary name, never assert a `.test`-less path. `args == ["serve"]` is the stable half of the assertion; `command` is whatever `os.Executable()` says right now (assert `filepath.IsAbs`).
2. **`encoding/json` sorts map keys deterministically on marshal.** That makes the idempotence assert byte-level: merge → snapshot bytes → merge again → bytes identical. No deep-equal gymnastics needed for the re-run case.
3. **`mergeStanza(path, stanza)` takes a path, not a `dirs` — that's the whole unit-test seam.** The cobra `RunE` is the only place that resolves real environment dirs (`os.Getwd`/`os.UserHomeDir`/`os.UserConfigDir`) into `dirs{}`; unit tests inject fake dirs into `resolveConfigPath` and hand `mergeStanza` explicit temp paths. `os.UserConfigDir` is therefore unreachable from unit tests by construction.
4. **`t.Chdir` (stdlib since Go 1.24) for the one cwd-dependent test** — claude-code writes `.mcp.json` in cwd; `t.Chdir(tmp)` covers restore automatically. First use in `cli/` (grep confirmed zero today); it replaces the old `os.Chdir` + `t.Cleanup` dance. If the reviewer prefers zero novelty, the alternative is calling the RunE's internals with `dirs{cwd: tmp}` — but then the flag→cwd wiring goes untested.
5. **House command-test pattern is mandatory:** fresh `rootCmd()` per invocation, `SetArgs`, `Execute` inside `captureOutput`, `resetGlobals()` between runs (see `TestCacheCmd_InspectClear`). init must add **zero package-level state** — flags are closure vars like `serve.go`'s `transport, addr`, so parallel/sequential invocations can't leak.
6. **Docs (`skill/SKILL.md`, README passage) are verified by grep commands, not tests** — same ruling as phase-X decision 7. A test asserting README content is a second golden; the opposite of cheap docs.
7. **Perms assert is `0600` on created files, GOOS-guarded.** `os.WriteFile` perms are best-effort on Windows; the assert runs `if runtime.GOOS != "windows"`. The real property being pinned is "not world-readable" (sibling configs hold API keys).

---

## User Stories

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | As an agent user adopting magpie, I want `magpie init --client claude-code` to produce a working MCP stanza with zero hand-editing, so setup is one command | `TestInit_ClaudeCode_CleanDir` (command-level, `t.Chdir`) | `.mcp.json` created in cwd; decoded `mcpServers.magpie.args == ["serve"]`; `command` == `os.Executable()` at test time and is absolute; exit code 0 |
| US-2 | As a user with existing MCP clients configured, I want init to merge only the `magpie` key, so my other servers and settings survive untouched | `TestInit_MergePreservesSibling` + `TestInit_IdempotentRerun` | Sibling server decodes byte-for-byte equal to its input JSON; second run leaves file **byte-identical** (decision 2); unknown top-level keys survive round-trip |
| US-3 | As a cautious user, I want `--dry-run`/`generic` to show the stanza without touching disk, so I can inspect before committing | `TestInit_GenericStdout` + `TestInit_DryRunWritesNothing` | Stanza JSON on stdout parses to the same shape; no file created anywhere in the temp cwd; exit code 0 |
| US-4 | As a user with a malformed or unexpected client config, I want init to fail loudly and leave my file alone, so a tool bug can't corrupt my setup | `TestInit_McpServersNotObject` + `TestInit_UnknownClient` | `mcpServers: "string"` → non-zero exit, error names the problem, file bytes unchanged (sentinel compare); unknown client → non-zero exit listing all four valid names |
| US-5 | As a multi-client user, I want each client's config written where that client actually reads it, so init works for claude-desktop/cursor too | `TestResolveConfigPath_Table` | claude-code→`cwd/.mcp.json`; claude-desktop→`configDir/Claude/claude_desktop_config.json`; cursor→`home/.cursor/mcp.json`; each under the **injected** dir, never the real `$HOME` |

---

## 1. Component Mock Strategy

Phase type: **pure logic** (no network, no browser, no heavy deps in any tier). Mock strategy in one sentence: **nothing is mocked — path logic is a pure function over injected `dirs`, merge logic is a pure function over explicit temp paths, and command wiring is tested through the real cobra tree with captured stdout, exactly like `TestCacheCmd_InspectClear`.**

| Component | Mock Strategy | What to Assert | User Story |
|-----------|--------------|----------------|------------|
| `resolveConfigPath(client, dirs)` | Table test, pure function, injected `dirs{cwd, home, configDir}` all = distinct `t.TempDir()` subdirs | Exact joined path per client (US-5 table); unknown client error message contains `claude-code`, `claude-desktop`, `cursor`, `generic`; no fs access at all | US-5, US-4 |
| `buildStanza()` | Real call in unit test | `Args` deep-equals `["serve"]`; `Command` == `os.Executable()` evaluated in the test (decision 1); `filepath.IsAbs(Command)` | US-1 |
| `mergeStanza(path, stanza)` — fresh file | Real call, explicit temp path | Created file 0600 (GOOS-guarded, decision 7); decodes to `{"mcpServers":{"magpie":{command,args}}}`; two-space indent + trailing `\n` | US-1 |
| `mergeStanza` — existing config | Real call over hand-written config JSON with sibling server + unknown top-level key | Sibling server subtree deep-equals original; unknown top-level key present after; `magpie` entry == stanza; existing `magpie` entry **overwritten** (ours) | US-2 |
| `mergeStanza` — hostile shapes | Real call over `mcpServers: "string"`, `mcpServers: 3`, `{"mcpServers":{"magpie": 5}}` (non-object entry is fine to overwrite; non-object `mcpServers` is not) | First two → error, file bytes unchanged (sentinel compare); third → succeeds (the `magpie` key is ours to replace) | US-4 |
| Idempotence | Two consecutive `mergeStanza` calls, byte snapshot between | Bytes identical across the re-run (decision 2) | US-2 |
| Cobra wiring: `init --client claude-code` | Real `rootCmd()` + `SetArgs` + `Execute` in `captureOutput`, `t.Chdir(tempdir)` (decision 4) | `.mcp.json` exists in temp cwd; decoded stanza matches US-1 conditions; `resetGlobals()`-clean between runs | US-1 |
| Cobra wiring: `init --client generic` / `--dry-run` | Real `rootCmd()`, `captureOutput`, `t.Chdir(tempdir)` | Stdout parses to the stanza shape; **zero** `*.json` files created in temp cwd; missing `--client` → non-zero + usage error | US-3, US-4 |
| Perms on merge-preserve path | `os.Stat` after merge over pre-made 0644 file | Existing file's mode **preserved** (only `WriteFile` on create sets 0600; never widen a config the user made permissive — or, if implementation rewrites with 0600 unconditionally, pin that instead; pick one behavior in code and assert it, don't leave it accidental) | US-2 |
| Docs gates (SKILL.md frontmatter, README passage) | grep commands, NOT tests | `head -4 skill/SKILL.md` shows `name:`/`description:` keys; `grep -c "init --client" README.md` ≥ 1; SKILL.md names all 12 MCP tools (`grep -c scrape_url skill/SKILL.md` ≥ 1) | — |

## 2. Test Tier Table

| Tier | Dependencies | Speed | When to Run |
|------|-------------|-------|-------------|
| Default (`go test ./cli/ -run TestInit`) | Temp dirs only; **no network, no HOME access, no fakes** | <1s | Every push; rides `go test ./...` |
| Full cli package (`go test ./cli/`) | Same + existing suite | ~10s | Every push (unchanged gate) |
| Docs/release gate (manual greps) | Committed `skill/SKILL.md` + `README.md` | seconds | When touching docs in this phase |

No integration or E2E tier exists: there is no external system to integrate with — clients are just JSON files at paths.

## 3. No Fake Implementations (Pure Logic)

No component of this phase talks to a network, model, database, or even the real filesystem outside `t.TempDir()` — the heaviest thing `mergeStanza` touches is `encoding/json`. A fake would only re-implement `json.Unmarshal`, which is the thing under test's own dependency. The one "external" behavior — where configs live per OS — is neutralized by the `dirs` injection seam (decision 3), not by faking.

## 4. Test File List

```
magpie/
├── cli/
│   ├── init.go            # deliverable: newInitCmd, resolveConfigPath, buildStanza, mergeStanza, dirs
│   ├── init_test.go       # ALL tests: resolve table, buildStanza, mergeStanza (fresh/merge/hostile/
│   │                      # idempotent/perms), command-level happy path + generic/dry-run + error exits
│   └── root.go            # +newInitCmd() registration — covered by every command-level test (a missing
│                          # registration fails TestInit_ClaudeCode_CleanDir with "unknown command")
├── skill/
│   └── SKILL.md           # docs gate: frontmatter + 12 MCP tool names — verified by grep commands (§1)
└── README.md              # docs gate: "init --client" passage in ## MCP server — verified by grep
```

## 5. No conftest (Go)

Go has no conftest; shared test helpers live in `cli/init_test.go` itself, scoped to this file so they can't accrete into a second `cmd_test.go`:

```go
// dirs(t) — three distinct temp subdirs so a wrong-dir bug can't self-mask.
func dirs(t *testing.T) cliDirs // {cwd, home, configDir: t.TempDir() each} (type name = whatever init.go ships)

// writeConfig(t, path, contents string) — writes with 0644, fails the test on error.
func writeConfig(t *testing.T, path, contents string)

// readJSON(t, path) map[string]any — decode helper; fails on malformed JSON.
func readJSON(t *testing.T, path string) map[string]any

// siblingConfigJSON — const with one foreign server + one unknown top-level key:
const siblingConfigJSON = `{"other":{"enabled":true},"mcpServers":{"other":{"command":"x","args":["-y"]}}}`
```

If implementation ends up wanting these in a second file, that's the rule-of-three signal — not before.

## 6. Key Testing Decisions

| Decision | Approach | Rationale |
|----------|----------|-----------|
| Unit surface = pure functions with injected paths/dirs | `resolveConfigPath(client, dirs)` + `mergeStanza(path, stanza)` tested directly | Makes `os.UserConfigDir`/`$HOME` unreachable from tests by construction (decision 3); no env scrubbing, no parallel-test flake |
| Idempotence asserted at byte level | Snapshot `os.ReadFile` bytes between two merges | `encoding/json` sorts map keys (decision 2) — byte equality is deterministic and stricter than DeepEqual |
| Hostile-shape table splits "ours" from "theirs" | `mcpServers.magpie: 5` overwrites; `mcpServers: "string"` errors, file untouched | The magpie key is init's to replace; the container is the user's. Conflating them either corrupts user config or refuses legit re-init |
| One command-level test per output channel | claude-code (writes), generic (stdout), dry-run (nothing), unknown (error exit) | Exercises the real flag parsing + RunE branching the unit tests can't reach; mirrors `TestCacheCmd_InspectClear` structure |
| Perms: pick create-0600 vs preserve-existing deliberately, then pin | `os.Stat` assert, GOOS-guarded | Decision 7 — an accidental perm story is a security-adjacent hole (sibling configs hold API keys) |
| Stanza `command` never hardcoded | Compare to `os.Executable()` at test time | Decision 1 — test binary path ≠ `magpie`; hardcoding breaks the suite, not the code |
| Docs by grep, not by test | Commands in §9 | Decision 6 — content assertions on prose are goldens with worse ergonomics |

## 7. Example Test Case

```go
// cli/init_test.go — the merge trio: preserve, hostile, idempotent. The heart of US-2/US-4.
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestInit_MergePreservesSibling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	writeConfig(t, path, siblingConfigJSON)
	before := readJSON(t, path)

	stanza := mcpStanza{Command: "/opt/magpie", Args: []string{"serve"}} // command value irrelevant to merge
	if err := mergeStanza(path, stanza); err != nil {
		t.Fatalf("merge: %v", err)
	}

	after := readJSON(t, path)
	servers, ok := after["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers type %T, want object", after["mcpServers"])
	}
	if !reflect.DeepEqual(servers["other"], before["mcpServers"].(map[string]any)["other"]) {
		t.Errorf("sibling server mutated: got %#v", servers["other"])
	}
	if !reflect.DeepEqual(after["other"], before["other"]) {
		t.Errorf("unknown top-level key dropped: got %#v", after["other"])
	}
	got := servers["magpie"].(map[string]any)
	if got["command"] != "/opt/magpie" || !reflect.DeepEqual(got["args"], []any{"serve"}) {
		t.Errorf("magpie entry = %#v, want command+/serve", got)
	}
}

func TestInit_McpServersNotObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	writeConfig(t, path, `{"mcpServers":"nope"}`)
	sentinel, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeStanza(path, mcpStanza{Command: "x", Args: []string{"serve"}}); err == nil {
		t.Fatal("want error for non-object mcpServers, got nil")
	}
	now, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(sentinel, now) {
		t.Fatalf("file mutated on error: before %q, after %q (err %v)", sentinel, now, err)
	}
}

func TestInit_IdempotentRerun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	st := mcpStanza{Command: "/opt/magpie", Args: []string{"serve"}}
	if err := mergeStanza(path, st); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := mergeStanza(path, st); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) { // json map keys are sorted → deterministic bytes
		t.Errorf("re-run changed bytes:\nfirst  %q\nsecond %q", first, second)
	}
}

func TestInit_CreatedFilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix perms only")
	}
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := mergeStanza(path, mcpStanza{Command: "x", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0600 {
		t.Errorf("perms = %v, want 0600 (sibling configs hold API keys)", fi.Mode().Perm())
	}
}
```

Command-level pattern (wiring half of the suite):

```go
func TestInit_ClaudeCode_CleanDir(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"init", "--client", "claude-code"})
	_, _ = captureOutput(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("init: %v", err)
		}
	})
	raw, err := os.ReadFile(filepath.Join(tmp, ".mcp.json"))
	if err != nil {
		t.Fatalf(".mcp.json not created in cwd: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m, ok := cfg.MCPServers["magpie"]
	if !ok {
		t.Fatal("no magpie entry")
	}
	if want, _ := os.Executable(); m.Command != want { // test binary, not "magpie" (decision 1)
		t.Errorf("command = %q, want %q", m.Command, want)
	}
	if !reflect.DeepEqual(m.Args, []string{"serve"}) {
		t.Errorf("args = %#v", m.Args)
	}
}
```

## 8. Execution Prompt

Copy everything between the `---` lines into a new pi session to write this test suite:

---

You are writing the test suite for Phase Y of magpie — agent-skill + installer packaging (`magpie init`). The implementation (`cli/init.go`) is being built in the same pass; these tests are its acceptance gate.

### What This Project Is
magpie: Go 1.26 CLI web scraper (module `github.com/motherlodelab/magpie`), local-first, CGO-free. This phase adds `magpie init --client <name>` writing a `{"mcpServers":{"magpie":{"command":"<abs>","args":["serve"]}}}` stanza into client configs (claude-code → `.mcp.json` in cwd; claude-desktop → `<configDir>/Claude/claude_desktop_config.json`; cursor → `<home>/.cursor/mcp.json`; generic → stdout). Read `AGENTS.md` and `.pi/rules/testing.md` first. All tests hermetic; no network.

### Acceptance Criteria (from User Stories)

| # | User Story | Validation Check | Pass Condition |
|---|-----------|-----------------|----------------|
| US-1 | Agent user: one-command MCP setup | `TestInit_ClaudeCode_CleanDir` | `.mcp.json` in cwd, `args==["serve"]`, `command==os.Executable()`, exit 0 |
| US-2 | Existing configs survive merge | `TestInit_MergePreservesSibling` + `TestInit_IdempotentRerun` | sibling + unknown keys deep-equal original; re-run byte-identical |
| US-3 | Inspect-before-commit | `TestInit_GenericStdout` + `TestInit_DryRunWritesNothing` | stanza on stdout parses; zero files created |
| US-4 | Loud failure, no corruption | `TestInit_McpServersNotObject` + `TestInit_UnknownClient` | non-zero exit, file bytes unchanged; error lists all 4 clients |
| US-5 | Right path per client | `TestResolveConfigPath_Table` | exact joined path under injected dirs for all 3 file clients |

### Why No Fakes Are Needed
The subject is stdlib JSON manipulation, path joins, and cobra wiring. Every "external" surface (HOME, UserConfigDir, cwd) is behind the `dirs` injection seam or `t.TempDir()`. Faking `encoding/json` would test the fake.

### What NOT to Test
- cobra's flag parsing itself (test that our flags produce our behavior, one missing-required-flag case max)
- `os.UserConfigDir`/`os.UserHomeDir` OS behavior — unit tests never call them (the RunE does; the seam is `dirs`)
- SKILL.md/README prose content — grep gates in run commands, not assertions
- `mcp/` or `serve` internals — untouched by this phase
- JSON indentation byte-format beyond "parses + 2-space + trailing newline"

### Test Files to Create

```
magpie/cli/init_test.go   # the only new test file; all cases below
```

### Per-File Coverage Guidance (cli/init_test.go)
- **`TestResolveConfigPath_Table`** — table: client × injected dirs (three DISTINCT temp dirs — a wrong-dir bug must not self-mask) → exact `filepath.Join` result; unknown client → error mentions all four names; zero filesystem assertions needed (pure function).
- **`TestBuildStanza`** — `Args` deep-equals `["serve"]`; `Command` == `os.Executable()` evaluated in the test; `filepath.IsAbs(Command)` true.
- **`TestInit_Merge_FreshFile`** — creates file; decodes to exact stanza shape; 0600 perms (GOOS-guarded, `runtime` import); trailing newline.
- **`TestInit_MergePreservesSibling`** — const `siblingConfigJSON` (foreign server + unknown top-level key); after merge: sibling deep-equal, unknown key survives, magpie entry correct (pattern below).
- **`TestInit_McpServersNotObject`** — `mcpServers:"nope"` → error + sentinel byte-compare proves file untouched; also `mcpServers:3`. **Counter-case**: `{"mcpServers":{"magpie":5}}` → succeeds (our key, ours to replace).
- **`TestInit_IdempotentRerun`** — two merges, `bytes.Equal` between snapshots (json sorts map keys — byte equality is deterministic).
- **`TestInit_ExistingMagpieOverwritten`** — stale `magpie` stanza replaced, siblings intact (re-init is a legit flow).
- **`TestInit_ClaudeCode_CleanDir`** — house pattern: `t.Chdir(tmp)`, `resetGlobals()`, fresh `rootCmd()`, `SetArgs`, `Execute` in `captureOutput`; assert file + decoded stanza (skeleton below).
- **`TestInit_GenericStdout`** — `--client generic`: stdout parses to stanza shape; temp cwd has zero new files.
- **`TestInit_DryRunWritesNothing`** — `--client claude-code --dry-run`: stdout has stanza; no `.mcp.json` anywhere in temp cwd.
- **`TestInit_UnknownClient`** — `--client nope`: `Execute` errors; message contains `claude-code`, `claude-desktop`, `cursor`, `generic`.
- **`TestInit_MissingClientFlag`** — no `--client`: errors (required-flag enforcement).
- Between every command-level test: `resetGlobals()` (house rule — `init` must own zero package state; if the implementation adds any, that's an implementation bug to flag, not a test to accommodate).

### Key Patterns (paste-verbatim scaffolding)

```go
// House command-level pattern (TestCacheCmd_InspectClear in cli/cmd_test.go is the reference):
func TestInit_ClaudeCode_CleanDir(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"init", "--client", "claude-code"})
	_, _ = captureOutput(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("init: %v", err)
		}
	})
	raw, err := os.ReadFile(filepath.Join(tmp, ".mcp.json"))
	// ... decode into struct{ MCPServers map[string]struct {
	//        Command string   `json:"command"`
	//        Args    []string `json:"args"`
	//    } } `json:"mcpServers"`
	// ... assert entry exists, Command == os.Executable() at test time, Args deep-equal ["serve"]
}

const siblingConfigJSON = `{"other":{"enabled":true},"mcpServers":{"other":{"command":"x","args":["-y"]}}}`
// After mergeStanza(path, stanza):
//   reflect.DeepEqual(servers["other"], before-subtree) — sibling untouched
//   reflect.DeepEqual(after["other"], before["other"]) — unknown key survives
//   servers["magpie"] == stanza — ours, written

// Hostile-shape sentinel (error must not touch the file):
//   writeConfig(t, path, `{"mcpServers":"nope"}`)
//   sentinel, _ := os.ReadFile(path)
//   if err := mergeStanza(path, st); err == nil { t.Fatal("want error") }
//   now, _ := os.ReadFile(path)
//   bytes.Equal(sentinel, now) must hold
```

### Data Model Notes
- Assert against decoded `map[string]any` / typed anonymous structs, never raw string-contains on JSON (key order churn).
- `mcpStanza` is a struct with json tags — construct it directly in unit tests; only command-level tests go through flags.
- Sibling-preservation checks use `reflect.DeepEqual` on decoded subtrees; idempotence uses `bytes.Equal` on raw bytes (both correct at their own layer — don't mix).

### Success Criteria
- `go test ./cli/ -run TestInit -v` exits 0, all cases pass, <1s
- `go test ./... -count=1` green; `go vet ./...` clean; `gofmt -l .` empty; `golangci-lint run ./...` clean; `git diff go.mod` empty
- Every US-1..US-5 row has its named test passing; zero references to real `$HOME`, real network, or the real `magpie` binary in init_test.go

---

---

## Run Commands

```bash
# This phase's gate (every push, rides go test ./...)
go test ./cli/ -run TestInit -v

# Full cli package (house suite)
go test ./cli/

# Full suite + gates (mirrors phase-Y exit criteria)
go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./... \
  && test -z "$(git diff go.mod)"

# Docs gates (grep, not tests)
head -4 skill/SKILL.md                                   # frontmatter keys present
grep -c "init --client" README.md                        # ≥ 1
grep -c "scrape_url" skill/SKILL.md                      # ≥ 1 (tools documented)
grep -rn "os.UserConfigDir\|os.UserHomeDir" cli/init_test.go   # 0 hits (seam held)
```

---

## Coverage Check

- [x] Phase type identified and mock strategy stated — pure logic; no mocks, injected `dirs` seam
- [x] User stories present (5) with binary pass conditions, derived from phase-Y deliverables
- [x] Every user story traces to ≥1 component row in the mock strategy table (US-1→stanza/claude-code rows; US-2→merge/idempotence rows; US-3→generic/dry-run rows; US-4→hostile/unknown rows; US-5→resolve table row)
- [x] Every phase-Y deliverable has a test file: init.go→init_test.go; root.go registration→command-level tests (unknown command = loud failure); SKILL.md/README→grep gates (decision 6, deliberate)
- [x] Heavy dependencies: none exist; "No Fake Implementations (Pure Logic)" section states why
- [x] Unit tests reference no real HOME, network, API, or the real binary — `os.Executable()` = test binary, pinned in decision 1
- [x] Integration gating: N/A — no integration tier exists (stated in §2); everything rides the default suite
- [x] conftest section present as "No conftest (Go)" with the helper inventory (skill's Python convention adapted, not skipped silently)
- [x] Execution prompt is self-contained: fakes-why, what-NOT-to-test, per-file guidance, data-model notes, success criteria — no "see above" references to this plan's earlier sections that a fresh session couldn't follow (§7 code is pasted into the prompt's source doc, §7 itself lives in this file which the prompt summarizes fully)
- [x] Run commands section present with gates + docs checks
