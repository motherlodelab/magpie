package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---- helpers (scoped to this file; the merge/path cases live in mcp/clients) ----

func exe(t *testing.T) string { // os.Executable() = test binary, not "magpie"
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
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
