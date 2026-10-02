package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/motherlodelab/magpie/mcp/clients"
	"github.com/spf13/cobra"
)

// mcpStanza is the fixed shape we emit; the struct keeps field order stable.
// No env block, ever — init never touches secrets.
type mcpStanza struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

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
	_ = cmd.MarkFlagRequired("client") //nolint:errcheck // flag defined two lines above; error impossible
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
	d, err := clients.UserDirs()
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	path, err := clients.ConfigPath(client, d)
	if err != nil { // the only failure is an unknown client; generic is handled above
		return fail(2, "init: unknown client %q (valid: claude-code, claude-desktop, cursor, generic)", client)
	}
	st, err := buildStanza()
	if err != nil {
		return err
	}
	if err := clients.Merge(path, "magpie", st); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	fmt.Println("wrote", path)
	return nil
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
