// Package clients writes one MCP server entry into an agent client's JSON
// config (Claude Code's project .mcp.json, Claude Desktop, Cursor). It
// merges, never replaces: every other key and server survives, number
// literals round-trip verbatim. The entry's shape is the caller's — stdio
// command+args for the CLI, {"url": …} for an HTTP server.
package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
// error, never an overwrite. The write is atomic (QA ST4: these are
// other apps' files, so a kill mid-write must not lose them): 0600 on
// create, an existing file keeps its mode. A missing parent folder is an
// error too (nothing creates it) — callers decide what that means.
func Merge(path, name string, entry any) error {
	var cfg map[string]any
	raw, err := os.ReadFile(path)
	if err == nil {
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
	if raw != nil {
		if err := backupOnce(path, raw); err != nil {
			return fmt.Errorf("clients: back up %s: %w", path, err)
		}
	}
	if err := writeFileAtomic(path, b); err != nil {
		return fmt.Errorf("clients: write %s: %w", path, err)
	}
	return nil
}

// backupOnce keeps the user's own config as <path>.magpie.bak (same mode)
// before magpie's first write to it (QA §3). O_EXCL: a later merge never
// overwrites it, so the backup is the user's file, not a magpie write.
func backupOnce(path string, raw []byte) error {
	perm := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	f, err := os.OpenFile(path+".magpie.bak", os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err = errors.Join(err, f.Chmod(perm), f.Close()); err != nil {
		_ = os.Remove(f.Name()) //nolint:errcheck // a partial backup must not block the next try
		return err
	}
	return nil
}

// writeFileAtomic replaces path via a same-dir temp + rename, so a crash
// leaves the old file or the new one, never half. An existing file keeps
// its mode (new files get 0600), and a symlinked path (a dotfiles setup)
// is written through, not replaced.
func writeFileAtomic(path string, b []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	perm := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() //nolint:errcheck // no-op after the rename
	_, err = f.Write(b)
	if err = errors.Join(err, f.Chmod(perm), f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
