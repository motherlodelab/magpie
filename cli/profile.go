package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/motherlodelab/magpie/config"
	"github.com/spf13/cobra"
)

// Saved credential profiles (Phase Z.5): a name → static
// cookies/headers map. Profiles are NOT sessions — nothing captures,
// refreshes, or logs in; they cover any site whose login reduces to a
// static session header.

var profileNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// Profile is one saved credential set.
type Profile struct {
	Cookies string   `json:"cookies,omitempty"`
	Headers []string `json:"headers,omitempty"`
}

// profilePath resolves the store path; overridable for tests.
var profilePath = func() string {
	return filepath.Join(config.DefaultConfigDir(), "profiles.json")
}

func loadProfiles() (map[string]Profile, error) {
	data, err := os.ReadFile(profilePath())
	if os.IsNotExist(err) {
		return map[string]Profile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read profiles: %w", err)
	}
	m := map[string]Profile{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("profiles: parse %s: %w", profilePath(), err)
	}
	return m, nil
}

func saveProfiles(m map[string]Profile) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := profilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("profiles: mkdir: %w", err)
	}
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		// 0600 on create — the file holds credentials; never widen later.
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("profiles: write: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("profiles: write: %w", err)
	}
	return nil
}

// maskSecret renders "first4…last4"; ≤8 chars collapse to "…".
func maskSecret(s string) string {
	if len(s) <= 8 {
		return "…"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func maskHeader(h string) string {
	name, val, ok := strings.Cut(h, ":")
	if !ok {
		return maskSecret(h)
	}
	return name + ": " + maskSecret(strings.TrimSpace(val))
}

// resolveProfile merges a named profile into explicit flag values —
// flags win field-by-field (explicit wins over file, per house
// precedence). Resolution validates the merged set via the same header
// policing scrape.ValidateOptions applies, so a hostile profile fails
// loudly at the options boundary.
func resolveProfile(name string, cookies string, headers []string) (string, []string, error) {
	if name == "" {
		return cookies, headers, nil
	}
	if !profileNameRe.MatchString(name) {
		return "", nil, fmt.Errorf("profile name %q must match %s", name, profileNameRe)
	}
	profiles, err := loadProfiles()
	if err != nil {
		return "", nil, err
	}
	p, ok := profiles[name]
	if !ok {
		var names []string
		for n := range profiles {
			names = append(names, n)
		}
		return "", nil, fmt.Errorf("unknown profile %q (known: %s)", name, strings.Join(names, ", "))
	}
	if cookies == "" {
		cookies = p.Cookies
	}
	if len(headers) == 0 {
		headers = p.Headers
	}
	return cookies, headers, nil
}

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Save and manage scrape credential profiles",
	}
	cmd.AddCommand(newProfileSaveCmd(), newProfileLsCmd(), newProfileRmCmd())
	return cmd
}

func newProfileSaveCmd() *cobra.Command {
	var cookies string
	var headers []string
	cmd := &cobra.Command{
		Use:   "save <name>",
		Short: "Upsert a profile (--cookies and/or --header, repeatable)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !profileNameRe.MatchString(name) {
				return fail(2, "profile name %q must match %s", name, profileNameRe)
			}
			if cookies == "" && len(headers) == 0 {
				return fail(2, "nothing to save: pass --cookies and/or --header")
			}
			m, err := loadProfiles()
			if err != nil {
				return err
			}
			m[name] = Profile{Cookies: cookies, Headers: headers}
			return saveProfiles(m)
		},
	}
	cmd.Flags().StringVar(&cookies, "cookies", "", "raw Cookie header value, e.g. \"a=b; c=d\"")
	cmd.Flags().StringSliceVar(&headers, "header", nil, "raw request header, repeatable: \"Name: value\"")
	return cmd
}

func newProfileLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List profiles (secrets masked)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadProfiles()
			if err != nil {
				return err
			}
			for name, p := range m {
				var parts []string
				if p.Cookies != "" {
					parts = append(parts, "cookies="+maskSecret(p.Cookies))
				}
				for _, h := range p.Headers {
					parts = append(parts, maskHeader(h))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", name, strings.Join(parts, " ")) //nolint:errcheck // stdout write to a cobra buffer; a short ls line is unactionable
			}
			return nil
		},
	}
}

func newProfileRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			m, err := loadProfiles()
			if err != nil {
				return err
			}
			if _, ok := m[name]; !ok {
				return fail(2, "unknown profile %q", name)
			}
			delete(m, name)
			return saveProfiles(m)
		},
	}
}
