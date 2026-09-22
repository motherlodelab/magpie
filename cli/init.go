package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// mcpStanza is the fixed shape we emit; the struct keeps field order stable.
// No env block, ever — init never touches secrets.
type mcpStanza struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// dirs is the path-resolution input. Pure seam: tests inject temp dirs
// instead of faking GOOS; only the RunE consults the real environment.
type dirs struct{ cwd, home, configDir string }

func newInitCmd() *cobra.Command {
	var client string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write the magpie MCP stanza into an agent client's config",
		Long: `Write the magpie MCP stanza into an agent client's config.

Merges into the existing config (never drops other servers or keys) and
is idempotent on re-run. --client generic and --dry-run print the stanza
to stdout and write nothing. Never emits an env block; keys flow from
your own environment.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(client, dryRun)
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "claude-code|claude-desktop|cursor|generic")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the stanza to stdout instead of writing")
	if err := cmd.MarkFlagRequired("client"); err != nil { //nolint:errcheck // flag exists; error impossible
		panic(err)
	}
	return cmd
}

func runInit(client string, dryRun bool) error {
	if dryRun || client == "generic" {
		st, err := buildStanza()
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return fmt.Errorf("init: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	d, err := realDirs()
	if err != nil {
		return err
	}
	path, err := resolveConfigPath(client, d)
	if err != nil {
		return err
	}
	st, err := buildStanza()
	if err != nil {
		return err
	}
	if err := mergeStanza(path, st); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func resolveConfigPath(client string, d dirs) (string, error) {
	switch client {
	case "claude-code":
		return filepath.Join(d.cwd, ".mcp.json"), nil
	case "claude-desktop":
		return filepath.Join(d.configDir, "Claude", "claude_desktop_config.json"), nil
	case "cursor":
		return filepath.Join(d.home, ".cursor", "mcp.json"), nil
	case "generic":
		return "", fail(2, "init: client %q writes nothing; stanza printed to stdout", client)
	default:
		return "", fail(2, "init: unknown client %q (valid: claude-code, claude-desktop, cursor, generic)", client)
	}
}

func realDirs() (dirs, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return dirs{}, fmt.Errorf("init: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dirs{}, fmt.Errorf("init: %w", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return dirs{}, fmt.Errorf("init: %w", err)
	}
	return dirs{cwd: cwd, home: home, configDir: configDir}, nil
}

// buildStanza uses the absolute executable path — GUI clients don't
// inherit your shell PATH, so a bare "magpie" only works when the client
// was launched from a terminal that has it.
func buildStanza() (mcpStanza, error) {
	exe, err := os.Executable()
	if err != nil {
		return mcpStanza{}, fmt.Errorf("init: resolve magpie path: %w", err)
	}
	return mcpStanza{Command: exe, Args: []string{"serve"}}, nil
}

// mergeStanza sets mcpServers.magpie and nothing else. Read-side is
// map[string]any — client configs carry arbitrary user keys a typed
// struct would drop. mcpServers present but not an object is a hard
// error, never an overwrite. os.WriteFile sets 0600 only on create;
// an existing file keeps its mode.
func mergeStanza(path string, s mcpStanza) error {
	var cfg map[string]any
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("init: parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("init: read %s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if cfg["mcpServers"] != nil && !ok {
		return fmt.Errorf("init: %s: mcpServers is %T, want an object — not overwriting", path, cfg["mcpServers"])
	}
	if servers == nil {
		servers = map[string]any{}
	}
	servers["magpie"] = s
	cfg["mcpServers"] = servers
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("init: encode %s: %w", path, err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("init: write %s: %w", path, err)
	}
	return nil
}
