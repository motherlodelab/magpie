package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/config"
)

func isolatedXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("APPDATA", "")
	return dir
}

func TestPrecedence(t *testing.T) {
	dir := isolatedXDG(t)
	cfgPath := filepath.Join(dir, "magpie", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("extract_provider: openai\nmodel: file-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_EXTRACT_PROVIDER", "ollama")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExtractProvider != "ollama" {
		t.Errorf("env should beat file: %q", cfg.ExtractProvider)
	}
	if cfg.Model != "file-model" {
		t.Errorf("file value lost: %q", cfg.Model)
	}
	cfg.ApplyFlags(config.Flags{Provider: "anthropic", ProviderChanged: true})
	if cfg.ExtractProvider != "anthropic" {
		t.Errorf("flag should beat env: %q", cfg.ExtractProvider)
	}
	def, err := config.Load(filepath.Join(dir, "nonexistent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if def.ExtractProvider != "ollama" {
		t.Errorf("defaults+env: %q", def.ExtractProvider)
	}
}

func TestKeyLookupOrder(t *testing.T) {
	isolatedXDG(t)
	cfg := config.DefaultConfig()
	t.Setenv("MAGPIE_ANTHROPIC_API_KEY", "env-key")
	if k := cfg.APIKey("anthropic"); k != "env-key" {
		t.Errorf("env key: %q", k)
	}
	cfg.APIKeyFlag = "flag-key"
	if k := cfg.APIKey("anthropic"); k != "flag-key" {
		t.Errorf("flag key: %q", k)
	}
}

func TestKeyEnvDashToUnderscore(t *testing.T) {
	isolatedXDG(t)
	cfg := config.DefaultConfig()
	t.Setenv("MAGPIE_OPENCODE_GO_API_KEY", "env-key")
	if k := cfg.APIKey("opencode-go"); k != "env-key" {
		t.Errorf("APIKey(opencode-go) = %q, want dash-to-underscore env hit", k)
	}
}

func TestDefaultModels(t *testing.T) {
	for provider, want := range map[string]string{
		"anthropic":    "claude-sonnet-5",
		"openai":       "gpt-4o-mini",
		"ollama":       "llama3.1",
		"openrouter":   "openai/gpt-4o-mini",
		"codex":        "gpt-5.2",
		"opencode-go":  "glm-5.3",
		"opencode-zen": "claude-sonnet-4-6",
	} {
		if got := config.DefaultModel(provider); got != want {
			t.Errorf("DefaultModel(%q) = %q, want %q", provider, got, want)
		}
	}
}

func TestShowRedacts(t *testing.T) {
	isolatedXDG(t)
	t.Setenv("MAGPIE_ANTHROPIC_API_KEY", "super-secret")
	cfg, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Redacted()
	if m["api_key"] != "***redacted***" {
		t.Errorf("api_key = %v", m["api_key"])
	}
	for _, v := range m {
		if s, ok := v.(string); ok && strings.Contains(s, "super-secret") {
			t.Error("raw key leaked in redacted output")
		}
	}
}

func TestServeKeys_OverlayChain(t *testing.T) {
	dir := isolatedXDG(t)
	cfgPath := filepath.Join(dir, "magpie", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Defaults.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServeTransport != "stdio" {
		t.Errorf("default serve_transport = %q, want stdio", cfg.ServeTransport)
	}
	if cfg.ExporterCmd != "" {
		t.Errorf("default exporter_cmd = %q, want empty", cfg.ExporterCmd)
	}
	// File sets serve_addr.
	if err := os.WriteFile(cfgPath, []byte("serve_addr: 127.0.0.1:9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServeAddr != "127.0.0.1:9999" {
		t.Errorf("file serve_addr = %q, want 127.0.0.1:9999", cfg.ServeAddr)
	}
	// Env beats file.
	t.Setenv("MAGPIE_SERVE_TRANSPORT", "http")
	t.Setenv("MAGPIE_EXPORTER_CMD", "myprog --fast")
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServeTransport != "http" {
		t.Errorf("env serve_transport = %q, want http", cfg.ServeTransport)
	}
	if cfg.ExporterCmd != "myprog --fast" {
		t.Errorf("env exporter_cmd = %q", cfg.ExporterCmd)
	}
	// Flags beat env.
	cfg.ApplyFlags(config.Flags{
		ServeTransport: "stdio", ServeTransportChanged: true,
		ServeAddr: "127.0.0.1:1111", ServeAddrChanged: true,
		ExporterCmd: "other", ExporterCmdChanged: true,
	})
	if cfg.ServeTransport != "stdio" || cfg.ServeAddr != "127.0.0.1:1111" || cfg.ExporterCmd != "other" {
		t.Errorf("flag overlay = %+v, want stdio/127.0.0.1:1111/other", cfg)
	}
}

// writeInitial seeds a config file and returns its path.
func writeInitial(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "magpie", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestSavePreservesUnknownsAndComments: Save mutates ONLY the known
// fields it is asked to — the operator's hand-edits (comments, unknown
// keys) are contract, not collateral. The shared config.yaml is
// hand-editable by design; a GUI save that reorders or drops it is the
// exact clobber this function exists to prevent.
func TestSavePreservesUnknownsAndComments(t *testing.T) {
	dir := isolatedXDG(t)
	path := writeInitial(t, dir, `# magpie config — hand-tuned
extract_provider: openai
model: old-model
# my scoring tweak, leave alone
custom_tuning: {a: 1, b: [2, 3]}
`)

	err := config.Save(path, func(c *config.Config) error {
		c.ExtractProvider = "anthropic"
		c.Model = "claude-sonnet-5"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	s := mustRead(t, path)
	for _, want := range []string{
		"# magpie config — hand-tuned",    // header comment survives
		"# my scoring tweak, leave alone", // inline comment survives
		"custom_tuning",                   // unknown key survives
		"a: 1",                            // unknown key's content survives
		"extract_provider: anthropic",     // known field mutated
		"model: claude-sonnet-5",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("after Save, file lost %q\nfile:\n%s", want, s)
		}
	}
	if strings.Contains(s, "old-model") {
		t.Errorf("old value not replaced:\n%s", s)
	}
}

// TestSaveCreatesMissing: no file yet → Save creates it from defaults,
// then applies the mutation.
func TestSaveCreatesMissing(t *testing.T) {
	dir := isolatedXDG(t)
	path := filepath.Join(dir, "magpie", "config.yaml")

	if err := config.Save(path, func(c *config.Config) error {
		c.ExtractProvider = "openai"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	s := mustRead(t, path)
	for _, want := range []string{"extract_provider: openai", "render: auto", "format: json"} {
		if !strings.Contains(s, want) {
			t.Errorf("fresh Save missing %q\nfile:\n%s", want, s)
		}
	}
	// The created file must Load back cleanly.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load created file: %v", err)
	}
	if cfg.ExtractProvider != "openai" {
		t.Errorf("Load round-trip provider = %q, want openai", cfg.ExtractProvider)
	}
}

// TestSaveMutateErrorLeavesFile: a mutate error aborts before any
// write — the file stays byte-identical.
func TestSaveMutateErrorLeavesFile(t *testing.T) {
	dir := isolatedXDG(t)
	path := writeInitial(t, dir, "extract_provider: openai\n")
	before := mustRead(t, path)

	err := config.Save(path, func(c *config.Config) error {
		return errors.New("forced")
	})
	if err == nil {
		t.Fatal("mutate error must propagate")
	}
	if got := mustRead(t, path); got != before {
		t.Errorf("failed mutate must not touch the file:\nbefore=%q after=%q", before, got)
	}
}

// TestSaveUnparseableExisting: garbage in the file → loud error, file
// untouched (never truncated to "fix" itself).
func TestSaveUnparseableExisting(t *testing.T) {
	dir := isolatedXDG(t)
	path := writeInitial(t, dir, "broken: [yaml\n  ::nope\n")
	before := mustRead(t, path)

	err := config.Save(path, func(c *config.Config) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("err = %v, want parse failure", err)
	}
	if got := mustRead(t, path); got != before {
		t.Errorf("failed parse must not touch the file:\nbefore=%q after=%q", before, got)
	}
}

func TestConfigShow_HasPhase3Keys(t *testing.T) {
	isolatedXDG(t)
	cfg, err := config.Load(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "magpie", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	red := cfg.Redacted()
	for _, k := range []string{"serve_transport", "serve_addr", "exporter_cmd"} {
		if _, ok := red[k]; !ok {
			t.Errorf("Redacted() missing %q", k)
		}
	}
}
