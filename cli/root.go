package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/vertical"

	"github.com/spf13/cobra"
)

var (
	cfgFile   string
	cacheDB   string
	maxCost   float64
	apiKey    string
	proxyFile string
)

// Execute runs the magpie command tree and returns the process exit code.
// The cmd/magpie shim is `os.Exit(cli.Execute())` so generated custom
// binaries share the exact tree.
func Execute() int {
	if err := rootCmd().Execute(); err != nil {
		return exitFor(err)
	}
	return 0
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "magpie",
		Short: "Fetch → clean → extract structured data from the web",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// ponytail: the env is the real config surface; the flag is
			// sugar so GUI/docs don't need env plumbing.
			if proxyFile != "" {
				if err := os.Setenv("MAGPIE_PROXY_FILE", proxyFile); err != nil {
					return fmt.Errorf("set MAGPIE_PROXY_FILE: %w", err)
				}
			}
			return nil
		},
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file path")
	root.PersistentFlags().StringVar(&cacheDB, "cache-db", "", "SQLite cache DB path")
	root.PersistentFlags().Float64Var(&maxCost, "max-cost", 0, "USD cost ceiling (abort before exceeding; flat-rate providers codex, opencode-go exempt)")
	root.PersistentFlags().StringVar(&apiKey, "api-key", "", "provider API key (overrides env/keyring)")
	root.PersistentFlags().StringVar(&proxyFile, "proxy-file", "", "proxy pool file: one URL (http|https|socks5) or host:port:user:pass per line; # comments (overrides MAGPIE_PROXY)")
	root.AddCommand(newScrapeCmd(), newExtractCmd(), newConfigCmd(), newCrawlCmd(), newCacheCmd(), newServeCmd(), newBuildCmd(),
		newBatchCmd(), newMapCmd(), newSummarizeCmd(), newDiffCmd(), newBrandCmd(), newVerticalCmd(), newSearchCmd(), newWatchCmd(), newInitCmd())
	return root
}

// resolveConfig loads file+env then overlays global flags.
func resolveConfig() (config.Config, error) {
	path := cfgFile
	if path == "" {
		path = config.DefaultConfigPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	f := config.Flags{}
	if cacheDB != "" {
		f.CacheDB, f.CacheDBChanged = cacheDB, true
	}
	if maxCost != 0 {
		f.MaxCost, f.MaxCostChanged = maxCost, true
	}
	if apiKey != "" {
		f.APIKey, f.APIKeyChanged = apiKey, true
	}
	cfg.ApplyFlags(f)
	return cfg, nil
}

// exitCode maps an error to the process exit code — pure, no printing
// (tests assert the map directly). It is the ONLY place exit codes for
// typed pipeline errors are decided (validation → 2, missing key → 7,
// quality → 8, cost ceiling → 6, robots → 5, scope/SSRF/vertical-
// mismatch → 2); command handlers return the error (with context
// attached) and never re-map. Command-specific usage errors keep their
// local fail(code, …) — each is single-site, not a shared mapping.
func exitCode(err error) int {
	if ce, ok := err.(*cmdError); ok {
		return ce.code
	}
	code := 1
	var oe *scrape.OptionsError
	var che *fetch.ChallengeError
	switch {
	case errors.As(err, &oe):
		code = 2
	case errors.Is(err, scrape.ErrMissingKey):
		code = 7
	case errors.As(err, &che):
		// A surviving bot challenge is a page-usability failure, grouped
		// with quality blocks (fetch couldn't produce scrapable content).
		code = 8
	case errors.Is(err, crawl.ErrCostCeiling):
		code = 6
	case errors.Is(err, clean.ErrQuality):
		code = 8
	case errors.Is(err, crawl.ErrRobotsBlocked):
		code = 5
	case errors.Is(err, crawl.ErrSitemapOnlyEmpty):
		// All "pages" failed to even enqueue — the no-pages code, not the
		// default 1 (exit codes are documented contract).
		code = 3
	case errors.Is(err, vertical.ErrURLMismatch),
		errors.Is(err, fetch.ErrPrivateAddress),
		errors.Is(err, fetch.ErrProxyConfig),
		errors.Is(err, crawl.ErrBadScope):
		code = 2
	}
	return code
}

// exitFor prints err the way the process edge does and returns exitCode's
// mapping. Only Execute calls it; tests use exitCode to stay silent.
func exitFor(err error) int {
	code := exitCode(err)
	if ce, ok := err.(*cmdError); ok {
		fmt.Fprintln(os.Stderr, ce.msg)
		return code
	}
	if code == 1 {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, err)
	return code
}

type cmdError struct {
	code int
	msg  string
}

func (e *cmdError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &cmdError{code: code, msg: fmt.Sprintf(format, args...)}
}
