package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// ---- helpers (scoped to this file; second consumer = rule-of-three signal) ----

func testDirs(t *testing.T) dirs {
	t.Helper()
	return dirs{cwd: t.TempDir(), home: t.TempDir(), configDir: t.TempDir()}
}

func exe(t *testing.T) string { // os.Executable() = test binary, not "magpie"
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return cfg
}

const siblingConfigJSON = `{"other":{"enabled":true},"mcpServers":{"other":{"command":"x","args":["-y"]}}}`

// ---- resolveConfigPath (US-5, US-4) ----

func TestResolveConfigPath_Table(t *testing.T) {
	d := testDirs(t)
	cases := []struct {
		client string
		want   string
	}{
		{"claude-code", filepath.Join(d.cwd, ".mcp.json")},
		{"claude-desktop", filepath.Join(d.configDir, "Claude", "claude_desktop_config.json")},
		{"cursor", filepath.Join(d.home, ".cursor", "mcp.json")},
	}
	for _, tc := range cases {
		got, err := resolveConfigPath(tc.client, d)
		if err != nil {
			t.Fatalf("%s: %v", tc.client, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.client, got, tc.want)
		}
	}
	for _, name := range []string{"claude-code", "claude-desktop", "cursor", "generic"} {
		if _, err := resolveConfigPath("nope", d); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("unknown-client error must list %q, got: %v", name, err)
		}
	}
}

// ---- buildStanza (US-1) ----

func TestBuildStanza(t *testing.T) {
	st, err := buildStanza()
	if err != nil {
		t.Fatal(err)
	}
	if want := exe(t); st.Command != want { // test binary, not "magpie"
		t.Errorf("command = %q, want %q", st.Command, want)
	}
	if !filepath.IsAbs(st.Command) {
		t.Errorf("command %q not absolute", st.Command)
	}
	if !reflect.DeepEqual(st.Args, []string{"serve"}) {
		t.Errorf("args = %#v, want [serve]", st.Args)
	}
}

// ---- mergeStanza: fresh file (US-1) ----

func TestInit_Merge_FreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	st := mcpStanza{Command: "/opt/magpie", Args: []string{"serve"}}
	if err := mergeStanza(path, st); err != nil {
		t.Fatal(err)
	}
	cfg := readJSON(t, path)
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers type %T, want object", cfg["mcpServers"])
	}
	got, ok := servers["magpie"].(map[string]any)
	if !ok {
		t.Fatalf("magpie entry type %T, want object", servers["magpie"])
	}
	if got["command"] != "/opt/magpie" || !reflect.DeepEqual(got["args"], []any{"serve"}) {
		t.Errorf("magpie entry = %#v, want command+/serve", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) || !bytes.Contains(raw, []byte("\n  \"mcpServers\"")) {
		t.Errorf("want 2-space indent + trailing newline, got %q", raw)
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
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("perms = %v, want 0600 (sibling configs hold API keys)", fi.Mode().Perm())
	}
}

// ---- mergeStanza: preserve + overwrite (US-2) ----

func TestInit_MergePreservesSibling(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, siblingConfigJSON)
	before := readJSON(t, path)

	if err := mergeStanza(path, mcpStanza{Command: "/opt/magpie", Args: []string{"serve"}}); err != nil {
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

func TestInit_ExistingMagpieOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, `{"mcpServers":{"magpie":{"command":"stale","args":["old"]},"other":{"command":"x"}}}`)
	if err := mergeStanza(path, mcpStanza{Command: "/new/magpie", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	cfg := readJSON(t, path)
	servers := cfg["mcpServers"].(map[string]any)
	got := servers["magpie"].(map[string]any)
	if got["command"] != "/new/magpie" || !reflect.DeepEqual(got["args"], []any{"serve"}) {
		t.Errorf("stale magpie entry not replaced: %#v", got)
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server dropped during overwrite")
	}
}

// ---- mergeStanza: hostile shapes (US-4) ----

func TestInit_McpServersNotObject(t *testing.T) {
	for _, body := range []string{`{"mcpServers":"nope"}`, `{"mcpServers":3}`} {
		path := filepath.Join(t.TempDir(), "claude_desktop_config.json")
		writeConfig(t, path, body)
		sentinel, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := mergeStanza(path, mcpStanza{Command: "x", Args: []string{"serve"}}); err == nil {
			t.Fatalf("body %s: want error for non-object mcpServers, got nil", body)
		}
		now, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(sentinel, now) {
			t.Fatalf("body %s: file mutated on error: before %q, after %q (err %v)", body, sentinel, now, err)
		}
	}
}

// ---- mergeStanza: idempotence (US-2) ----

func TestInit_IdempotentRerun(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	st := mcpStanza{Command: "/opt/magpie", Args: []string{"serve"}}
	if err := mergeStanza(path, st); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeStanza(path, st); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) { // json sorts map keys → deterministic bytes
		t.Errorf("re-run changed bytes:\nfirst  %q\nsecond %q", first, second)
	}
}

// ---- mergeStanza: user numbers survive the round-trip (US-2, trust boundary) ----

func TestInit_UserNumbersVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, `{"mcpServers":{"other":{"command":"x","maxTokens":123456789012345678}}}`)
	if err := mergeStanza(path, mcpStanza{Command: "x", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("123456789012345678")) {
		t.Errorf("user integer rewritten (float64 round-trip?): %s", raw)
	}
}

// ---- mergeStanza: perms preserved on existing file (US-2) ----

func TestInit_ExistingFilePermsPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix perms only")
	}
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, siblingConfigJSON) // 0644
	if err := mergeStanza(path, mcpStanza{Command: "x", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0644 {
		t.Errorf("perms = %v, want preserved 0644", fi.Mode().Perm())
	}
}

// ---- command-level wiring (US-1, US-3, US-4) ----

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
	if want := exe(t); m.Command != want { // test binary, not "magpie"
		t.Errorf("command = %q, want %q", m.Command, want)
	}
	if !reflect.DeepEqual(m.Args, []string{"serve"}) {
		t.Errorf("args = %#v, want [serve]", m.Args)
	}
}

func TestInit_GenericStdout(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"init", "--client", "generic"})
	out, _ := captureOutput(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("init: %v", err)
		}
	})
	var st struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("stdout does not parse as stanza: %v\nout: %q", err, out)
	}
	if want := exe(t); st.Command != want || !reflect.DeepEqual(st.Args, []string{"serve"}) {
		t.Errorf("stanza = %+v, want command+serve", st)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("generic wrote files: %v", entries)
	}
}

func TestInit_DryRunWritesNothing(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"init", "--client", "claude-code", "--dry-run"})
	out, _ := captureOutput(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("init: %v", err)
		}
	})
	var st struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("stdout does not parse as stanza: %v\nout: %q", err, out)
	}
	if want := exe(t); st.Command != want || !reflect.DeepEqual(st.Args, []string{"serve"}) {
		t.Errorf("stanza = %+v, want command+serve", st)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".mcp.json")); !os.IsNotExist(err) {
		t.Errorf(".mcp.json exists after --dry-run (stat err %v)", err)
	}
}

func TestInit_UnknownClient(t *testing.T) {
	t.Chdir(t.TempDir())
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"init", "--client", "nope"})
	err := root.Execute()
	if err == nil {
		t.Fatal("want error for unknown client, got nil")
	}
	for _, name := range []string{"claude-code", "claude-desktop", "cursor", "generic"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error must list %q, got: %v", name, err)
		}
	}
}

func TestInit_MissingClientFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"init"})
	if err := root.Execute(); err == nil {
		t.Fatal("want error for missing --client, got nil")
	}
}
