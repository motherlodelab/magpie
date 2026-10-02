// Package clients writes one MCP server entry into an agent client's JSON
// config (Claude Code's project .mcp.json, Claude Desktop, Cursor). It
// merges, never replaces: every other key and server survives, number
// literals round-trip verbatim. The entry's shape is the caller's — stdio
// command+args for the CLI, {"url": …} for an HTTP server.
package clients

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Dirs is the path-resolution input. Pure seam: tests inject temp dirs
// instead of faking GOOS; UserDirs reads the real environment.
type Dirs struct{ CWD, Home, ConfigDir string }

// UserDirs is the real environment: working dir, home, OS config dir.
func UserDirs() (Dirs, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Dirs{}, fmt.Errorf("clients: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, fmt.Errorf("clients: %w", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return Dirs{}, fmt.Errorf("clients: %w", err)
	}
	return Dirs{CWD: cwd, Home: home, ConfigDir: configDir}, nil
}

// ConfigPath is where client keeps its MCP servers.
func ConfigPath(client string, d Dirs) (string, error) {
	switch client {
	case "claude-code":
		return filepath.Join(d.CWD, ".mcp.json"), nil
	case "claude-desktop":
		return filepath.Join(d.ConfigDir, "Claude", "claude_desktop_config.json"), nil
	case "cursor":
		return filepath.Join(d.Home, ".cursor", "mcp.json"), nil
	default:
		return "", fmt.Errorf("clients: unknown client %q (valid: claude-code, claude-desktop, cursor)", client)
	}
}

// Merge sets mcpServers[name] = entry and nothing else. Read-side is
// map[string]any — client configs carry arbitrary user keys a typed
// struct would drop. mcpServers present but not an object is a hard
// error, never an overwrite. os.WriteFile sets 0600 only on create;
// an existing file keeps its mode. A missing parent folder is an error
// too (os.WriteFile doesn't create it) — callers decide what that means.
// ponytail: os.WriteFile truncates in place, so a kill mid-write can in
// theory lose the config; temp+rename was rejected because preserving an
// existing file's mode across rename needs a stat+chmod dance for a
// ~200-byte file written by an interactive command.
func Merge(path, name string, entry any) error {
	var cfg map[string]any
	if raw, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		// UseNumber keeps the user config's number literals verbatim on the
		// round-trip (same rationale as scrape.go) — float64 would silently
		// rewrite integers past 2^53 in keys we promised not to touch.
		dec.UseNumber()
		if err := dec.Decode(&cfg); err != nil {
			return fmt.Errorf("clients: parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("clients: read %s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	var servers map[string]any
	switch v := cfg["mcpServers"].(type) {
	case map[string]any:
		servers = v
	case nil: // key absent, or explicit JSON null — nothing of theirs to lose
		servers = map[string]any{}
	default:
		return fmt.Errorf("clients: %s: mcpServers is %T, want an object — not overwriting", path, cfg["mcpServers"])
	}
	servers[name] = entry
	cfg["mcpServers"] = servers
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("clients: encode %s: %w", path, err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("clients: write %s: %w", path, err)
	}
	return nil
}
