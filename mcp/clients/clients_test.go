package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// ---- helpers (scoped to this file) ----

// stanza is the CLI's stdio entry shape (cli.mcpStanza), restated so the
// moved merge cases keep exercising a struct entry.
type stanza struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func testDirs(t *testing.T) Dirs {
	t.Helper()
	return Dirs{CWD: t.TempDir(), Home: t.TempDir(), ConfigDir: t.TempDir()}
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

// ---- ConfigPath ----

func TestConfigPath_Table(t *testing.T) {
	d := testDirs(t)
	cases := []struct {
		client string
		want   string
	}{
		{"claude-code", filepath.Join(d.CWD, ".mcp.json")},
		{"claude-desktop", filepath.Join(d.ConfigDir, "Claude", "claude_desktop_config.json")},
		{"cursor", filepath.Join(d.Home, ".cursor", "mcp.json")},
	}
	for _, tc := range cases {
		got, err := ConfigPath(tc.client, d)
		if err != nil {
			t.Fatalf("%s: %v", tc.client, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.client, got, tc.want)
		}
	}
	for _, client := range []string{"nope", "generic"} { // generic is the CLI's print-only mode, not a file
		_, err := ConfigPath(client, d)
		want := `clients: unknown client "` + client + `" (valid: claude-code, claude-desktop, cursor)`
		if err == nil || err.Error() != want {
			t.Errorf("ConfigPath(%q) err = %v, want %q", client, err, want)
		}
	}
}

// ---- Merge: fresh file ----

func TestMerge_FreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := Merge(path, "magpie", stanza{Command: "/opt/magpie", Args: []string{"serve"}}); err != nil {
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

func TestMerge_NullServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, `{"mcpServers":null,"keep":1}`)
	if err := Merge(path, "magpie", stanza{Command: "x", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	cfg := readJSON(t, path)
	if _, ok := cfg["mcpServers"].(map[string]any)["magpie"]; !ok {
		t.Errorf("null mcpServers not replaced by an object holding magpie: %#v", cfg)
	}
	if cfg["keep"] != float64(1) {
		t.Errorf("top-level key dropped: %#v", cfg)
	}
}

func TestMerge_CreatedFilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix perms only")
	}
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := Merge(path, "magpie", stanza{Command: "x", Args: []string{"serve"}}); err != nil {
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

// TestMerge_AtomicKeepsModeAndLink — QA ST4: another app's config is
// replaced via temp + rename (a crash can't leave it half-written), keeps
// its mode, and a symlinked config stays a symlink.
func TestMerge_AtomicKeepsModeAndLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix perms + symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(target, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(os.Chmod(target, 0o640), os.Symlink(target, link)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := Merge(link, "magpie", stanza{Command: "x", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("config was rewritten in place, want temp + rename")
	}
	if after.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the existing 0640 kept", after.Mode().Perm())
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced by a regular file (%v)", err)
	}
	if b, err := os.ReadFile(target); err != nil || !bytes.Contains(b, []byte(`"magpie"`)) || !bytes.Contains(b, []byte(`"theme"`)) {
		t.Errorf("target = %s, want the merged entry and the user's key", b)
	}
}

// ---- Merge: preserve + overwrite ----

func TestMerge_PreservesSibling(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, siblingConfigJSON)
	before := readJSON(t, path)

	if err := Merge(path, "magpie", stanza{Command: "/opt/magpie", Args: []string{"serve"}}); err != nil {
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

func TestMerge_ExistingEntryOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, `{"mcpServers":{"magpie":{"command":"stale","args":["old"]},"other":{"command":"x"}}}`)
	if err := Merge(path, "magpie", stanza{Command: "/new/magpie", Args: []string{"serve"}}); err != nil {
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

// TestMerge_Name: the entry lands under the caller's name — the desktop
// writes "magpie-desktop" so a CLI "magpie" entry survives (D6).
func TestMerge_Name(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := Merge(path, "x", map[string]string{"url": "u"}); err != nil {
		t.Fatal(err)
	}
	servers := readJSON(t, path)["mcpServers"].(map[string]any)
	if !reflect.DeepEqual(servers["x"], map[string]any{"url": "u"}) {
		t.Errorf("mcpServers.x = %#v, want {url: u}", servers["x"])
	}
	if _, ok := servers["magpie"]; ok {
		t.Error("Merge wrote a magpie entry it wasn't asked for")
	}
}

// ---- Merge: hostile shapes ----

func TestMerge_ServersNotObject(t *testing.T) {
	for _, body := range []string{`{"mcpServers":"nope"}`, `{"mcpServers":3}`} {
		path := filepath.Join(t.TempDir(), "claude_desktop_config.json")
		writeConfig(t, path, body)
		sentinel, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := Merge(path, "magpie", stanza{Command: "x", Args: []string{"serve"}}); err == nil || !strings.Contains(err.Error(), "not overwriting") {
			t.Fatalf("body %s: want a not-overwriting error for non-object mcpServers, got %v", body, err)
		}
		now, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(sentinel, now) {
			t.Fatalf("body %s: file mutated on error: before %q, after %q (err %v)", body, sentinel, now, err)
		}
	}
}

// TestMerge_MissingDirNotCreated: Merge never creates a client's config
// folder — the desktop turns this into "doesn't look installed".
func TestMerge_MissingDirNotCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".cursor")
	if err := Merge(filepath.Join(dir, "mcp.json"), "x", map[string]string{"url": "u"}); err == nil {
		t.Fatal("want an error writing into a missing folder")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Merge created %s (stat err %v)", dir, err)
	}
}

// ---- Merge: idempotence ----

func TestMerge_IdempotentRerun(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	st := stanza{Command: "/opt/magpie", Args: []string{"serve"}}
	if err := Merge(path, "magpie", st); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Merge(path, "magpie", st); err != nil {
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

// ---- Merge: user numbers survive the round-trip (trust boundary) ----

func TestMerge_UserNumbersVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, `{"big":12345678901234567890,"mcpServers":{"other":{"command":"x","maxTokens":123456789012345678}}}`)
	if err := Merge(path, "magpie", stanza{Command: "x", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, lit := range []string{"123456789012345678", "12345678901234567890"} {
		if !bytes.Contains(raw, []byte(lit)) {
			t.Errorf("user integer %s rewritten (float64 round-trip?): %s", lit, raw)
		}
	}
}

// ---- Merge: perms preserved on existing file ----

func TestMerge_ExistingFilePermsPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix perms only")
	}
	path := filepath.Join(t.TempDir(), ".mcp.json")
	writeConfig(t, path, siblingConfigJSON) // 0644
	if err := Merge(path, "magpie", stanza{Command: "x", Args: []string{"serve"}}); err != nil {
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
