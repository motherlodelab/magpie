package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

const keyringService = "magpie"

// Config holds resolved CLI configuration.
// Precedence: flags > env (MAGPIE_) > file > defaults.
type Config struct {
	ExtractProvider string  `yaml:"extract_provider" json:"extract_provider"`
	Model           string  `yaml:"model" json:"model"`
	Render          string  `yaml:"render" json:"render"`
	Format          string  `yaml:"format" json:"format"`
	Out             string  `yaml:"out" json:"out"`
	Schema          string  `yaml:"schema" json:"schema"`
	CacheDB         string  `yaml:"cache_db" json:"cache_db"`
	MaxCost         float64 `yaml:"max_cost" json:"max_cost"`
	NoCache         bool    `yaml:"no_cache" json:"no_cache"`
	ServeTransport  string  `yaml:"serve_transport" json:"serve_transport"`
	ServeAddr       string  `yaml:"serve_addr" json:"serve_addr"`
	ExporterCmd     string  `yaml:"exporter_cmd" json:"exporter_cmd"`

	// flag-provided API key (never persisted, never logged)
	APIKeyFlag string `yaml:"-" json:"-"`
	// file path actually loaded (for diagnostics)
	filePath string
}

// Defaults per provider.
func DefaultConfig() Config {
	return Config{
		ExtractProvider: "anthropic",
		Render:          "auto",
		Format:          "json",
		CacheDB:         DefaultDBPath(),
		ServeTransport:  "stdio",
		ServeAddr:       ":8080",
	}
}

func DefaultConfigDir() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "magpie")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "magpie")
	}
	return "."
}

func DefaultConfigPath() string {
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "magpie", "config.yaml")
	}
	return filepath.Join(DefaultConfigDir(), "config.yaml")
}

func DefaultDBPath() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "magpie", "cache.db")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache", "magpie", "cache.db")
	}
	return filepath.Join(".", "cache.db")
}

func DefaultModel(provider string) string {
	switch strings.ToLower(provider) {
	case "openai":
		return "gpt-4o-mini"
	case "ollama":
		return "llama3.1"
	case "openrouter":
		return "openai/gpt-4o-mini" // cheap sane default; any OpenRouter model id works
	case "codex":
		return "gpt-5.2"
	case "opencode-go":
		return "glm-5.3"
	case "opencode-zen":
		return "claude-sonnet-4-6"
	default:
		return "claude-sonnet-5"
	}
}

// Save applies mutate to the config file at path and writes it back.
// The file round-trips through yaml.Node so the operator's comments and
// unknown keys survive — a GUI writing this shared file must never
// clobber hand-edits. Missing or empty file → node built from
// DefaultConfig(), then mutated; a comments-only file is refused (the
// notes cannot round-trip into a mapping). Only fields with explicit
// node support are persisted (extract_provider and model today); a
// mutate that sets other fields silently drops them — extend per caller, no generic
// field-mapping layer until a second caller needs one. A mutate error
// (or a parse failure) aborts before any write: the file stays
// byte-identical.
func Save(path string, mutate func(*Config) error) error {
	if mutate == nil {
		mutate = func(*Config) error { return nil }
	}
	doc := &yaml.Node{}
	var existing []byte
	if data, err := os.ReadFile(path); err == nil {
		existing = data
		if err := yaml.Unmarshal(data, doc); err != nil {
			return fmt.Errorf("config: parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if doc.Kind == 0 {
		if len(strings.TrimSpace(string(existing))) > 0 {
			// Comments/whitespace only: the operator's notes are content
			// we cannot round-trip into a mapping — refuse rather than
			// silently replace the file with defaults.
			return fmt.Errorf("config: %s holds no configuration keys (comments only); add the keys by hand", path)
		}
		// Missing or truly empty file → defaults as the base.
		def := DefaultConfig()
		if err := doc.Encode(&def); err != nil {
			return fmt.Errorf("config: encode defaults: %w", err)
		}
	}
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return fmt.Errorf("config: %s: empty document", path)
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config: %s: top level must be a mapping (got kind %v)", path, root.Kind)
	}
	cfg := Config{}
	if err := root.Decode(&cfg); err != nil {
		return fmt.Errorf("config: decode %s: %w", path, err)
	}
	if err := mutate(&cfg); err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}
	setMappingKey(root, "extract_provider", cfg.ExtractProvider)
	setMappingKey(root, "model", cfg.Model)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("config: encode %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", filepath.Dir(path), err)
	}
	// 0600 only applies to fresh files (WriteFile keeps existing modes) —
	// matches Load's group/world-readable warning threshold.
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// setMappingKey sets key to the scalar string val in a mapping node,
// appending the pair when absent. Existing comments on the key and its
// value nodes are kept (only Value/Tag/Kind are touched).
func setMappingKey(m *yaml.Node, key, val string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			v.Kind, v.Tag, v.Value, v.Style = yaml.ScalarNode, "!!str", val, 0
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
}

// Load reads file (if present) then overlays MAGPIE_ env vars.
func Load(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		path = DefaultConfigPath()
	}
	cfg.filePath = path
	if data, err := os.ReadFile(path); err == nil {
		// Warn when the file holds a key but is group/world-readable.
		if fi, serr := os.Stat(path); serr == nil && fi.Mode().Perm()&0o077 != 0 && strings.Contains(string(data), "api_key") {
			fmt.Fprintln(os.Stderr, "warning: config file holds a key and is readable by others; chmod 0600")
		}
		var fileCfg Config
		if err := yaml.Unmarshal(data, &fileCfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
		cfg.overlay(fileCfg)
	} else if !os.IsNotExist(err) {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg.overlayEnv()
	if cfg.Model == "" {
		cfg.Model = DefaultModel(cfg.ExtractProvider)
	}
	return cfg, nil
}

// overlay copies non-zero file values over c.
func (c *Config) overlay(o Config) {
	if o.ExtractProvider != "" {
		c.ExtractProvider = o.ExtractProvider
	}
	if o.Model != "" {
		c.Model = o.Model
	}
	if o.Render != "" {
		c.Render = o.Render
	}
	if o.Format != "" {
		c.Format = o.Format
	}
	if o.Out != "" {
		c.Out = o.Out
	}
	if o.Schema != "" {
		c.Schema = o.Schema
	}
	if o.CacheDB != "" {
		c.CacheDB = o.CacheDB
	}
	if o.MaxCost != 0 {
		c.MaxCost = o.MaxCost
	}
	if o.NoCache {
		c.NoCache = o.NoCache
	}
	if o.ServeTransport != "" {
		c.ServeTransport = o.ServeTransport
	}
	if o.ServeAddr != "" {
		c.ServeAddr = o.ServeAddr
	}
	if o.ExporterCmd != "" {
		c.ExporterCmd = o.ExporterCmd
	}
}

func (c *Config) overlayEnv() {
	if v := os.Getenv("MAGPIE_EXTRACT_PROVIDER"); v != "" {
		c.ExtractProvider = v
	}
	if v := os.Getenv("MAGPIE_PROVIDER"); v != "" {
		c.ExtractProvider = v
	}
	if v := os.Getenv("MAGPIE_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("MAGPIE_RENDER"); v != "" {
		c.Render = v
	}
	if v := os.Getenv("MAGPIE_FORMAT"); v != "" {
		c.Format = v
	}
	if v := os.Getenv("MAGPIE_OUT"); v != "" {
		c.Out = v
	}
	if v := os.Getenv("MAGPIE_SCHEMA"); v != "" {
		c.Schema = v
	}
	if v := os.Getenv("MAGPIE_CACHE_DB"); v != "" {
		c.CacheDB = v
	}
	if v := os.Getenv("MAGPIE_SERVE_TRANSPORT"); v != "" {
		c.ServeTransport = v
	}
	if v := os.Getenv("MAGPIE_SERVE_ADDR"); v != "" {
		c.ServeAddr = v
	}
	if v := os.Getenv("MAGPIE_EXPORTER_CMD"); v != "" {
		c.ExporterCmd = v
	}
	if v := os.Getenv("MAGPIE_MAX_COST"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
			fmt.Fprintf(os.Stderr, "warning: ignoring invalid MAGPIE_MAX_COST %q\n", v)
		} else {
			c.MaxCost = f
		}
	}
}

// ApplyFlags overlays explicit cobra flag values (only when changed).
func (c *Config) ApplyFlags(f Flags) {
	if f.ProviderChanged {
		c.ExtractProvider = f.Provider
	}
	if f.ModelChanged {
		c.Model = f.Model
	}
	if f.RenderChanged {
		c.Render = f.Render
	}
	if f.FormatChanged {
		c.Format = f.Format
	}
	if f.OutChanged {
		c.Out = f.Out
	}
	if f.SchemaChanged {
		c.Schema = f.Schema
	}
	if f.CacheDBChanged {
		c.CacheDB = f.CacheDB
	}
	if f.MaxCostChanged {
		c.MaxCost = f.MaxCost
	}
	if f.NoCacheChanged {
		c.NoCache = f.NoCache
	}
	if f.APIKeyChanged {
		c.APIKeyFlag = f.APIKey
	}
	if f.ServeTransportChanged {
		c.ServeTransport = f.ServeTransport
	}
	if f.ServeAddrChanged {
		c.ServeAddr = f.ServeAddr
	}
	if f.ExporterCmdChanged {
		c.ExporterCmd = f.ExporterCmd
	}
	if c.Model == "" {
		c.Model = DefaultModel(c.ExtractProvider)
	}
}

// Flags mirrors the CLI surface so config stays cobra-free.
type Flags struct {
	Provider              string
	Model                 string
	Render                string
	Format                string
	Out                   string
	Schema                string
	CacheDB               string
	MaxCost               float64
	NoCache               bool
	APIKey                string
	ServeTransport        string
	ServeAddr             string
	ExporterCmd           string
	ProviderChanged       bool
	ModelChanged          bool
	RenderChanged         bool
	FormatChanged         bool
	OutChanged            bool
	SchemaChanged         bool
	CacheDBChanged        bool
	MaxCostChanged        bool
	NoCacheChanged        bool
	APIKeyChanged         bool
	ServeTransportChanged bool
	ServeAddrChanged      bool
	ExporterCmdChanged    bool
}

// keyEnvName maps a provider id to its env-var middle: opencode-go → OPENCODE_GO.
func keyEnvName(provider string) string {
	return strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))
}

// APIKey resolves flag > env > keyring > (no file keys; returns "" when absent).
func (c Config) APIKey(provider string) string {
	if c.APIKeyFlag != "" {
		return c.APIKeyFlag
	}
	p := keyEnvName(provider)
	for _, name := range []string{"MAGPIE_" + p + "_API_KEY", "MAGPIE_API_KEY"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	if k, err := keyring.Get(keyringService, strings.ToLower(provider)); err == nil && k != "" {
		return k
	}
	return ""
}

// SetKey stores provider key in the OS keyring.
func SetKey(provider, key string) error {
	if err := keyring.Set(keyringService, strings.ToLower(provider), key); err != nil {
		return fmt.Errorf("no secret service; export MAGPIE_%s_API_KEY instead: %w",
			keyEnvName(provider), err)
	}
	return nil
}

// Redacted returns a copy safe for `config show`.
func (c Config) Redacted() map[string]any {
	key := "***redacted***"
	if c.APIKey(c.ExtractProvider) == "" {
		key = "(not set)"
	}
	return map[string]any{
		"extract_provider": c.ExtractProvider,
		"model":            c.Model,
		"render":           c.Render,
		"format":           c.Format,
		"cache_db":         c.CacheDB,
		"max_cost":         c.MaxCost,
		"serve_transport":  c.ServeTransport,
		"serve_addr":       c.ServeAddr,
		"exporter_cmd":     c.ExporterCmd,
		"api_key":          key,
		"config_file":      c.filePath,
	}
}
