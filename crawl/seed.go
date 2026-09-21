package crawl

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// seed enqueues the seed (robots-checked first) plus sitemap expansion on
// fresh runs; resume runs start from the persisted frontier instead.
// Former Run seed block, verbatim.
func (c *crawlContext) seed(ctx context.Context) error {
	if c.opts.Resume {
		return nil
	}
	seedHost, herr := hostOf(c.opts.SeedURL)
	if herr != nil {
		seedHost = c.opts.SeedURL
	}
	if isHTTP(c.opts.SeedURL) && !c.opts.IgnoreRobots {
		allowed, err := c.checker.Allowed(ctx, c.opts.SeedURL)
		if err != nil || !allowed {
			if ferr := c.db.FinishRun(c.runID, 0, 0, "robots_blocked"); ferr != nil {
				fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
			}
			if err != nil && errors.Is(err, ErrRobotsUnreachable) {
				return fmt.Errorf("crawl: seed %s: %w", seedHost, ErrRobotsBlocked)
			}
			return fmt.Errorf("crawl: seed %s: %w", seedHost, ErrRobotsBlocked)
		}
	}
	seeds := []string{c.opts.SeedURL}
	if isHTTP(c.opts.SeedURL) && !c.opts.NoSitemap {
		// Expand the seed through the sitemap (page URLs, capped at
		// maxPages) instead of enqueuing raw sitemap-XML URLs. In normal
		// mode expansion NEVER fails the crawl: warn and proceed seed-only.
		// In sitemap-only mode the expansion IS the frontier, so an
		// expansion error is fatal (there is no seed-only fallback — the
		// seed URL is explicitly excluded from the contract).
		expanded, _, serr := ListSitemapURLs(ctx, c.static, c.opts.SeedURL)
		if serr != nil {
			if c.opts.SitemapOnly {
				return fmt.Errorf("crawl: sitemap-only: %w", serr)
			}
			fmt.Fprintf(os.Stderr, "crawl: sitemap expansion: %v; continuing seed-only\n", serr)
		} else {
			if len(expanded) > c.maxPages {
				expanded = expanded[:c.maxPages]
			}
			filtered := scopeFilterExpansion(c.scope, c.opts.SeedURL, expanded)
			if c.opts.SitemapOnly {
				seeds = filtered // seed itself excluded unless the sitemap lists it
				if len(seeds) == 0 {
					return fmt.Errorf("crawl: %w", ErrSitemapOnlyEmpty)
				}
			} else {
				seeds = append(seeds, filtered...)
			}
		}
	}
	n, err := c.frontier.Add(seeds, 0)
	if err != nil {
		return err
	}
	c.outstanding.Add(int64(n))
	return nil
}

// scopeFilterExpansion keeps only in-scope sitemap-expanded URLs (the
// seed itself is always kept by the caller — the operator chose it).
// Sitemaps list the whole site, so without this filter --path-prefix
// would burn the --max-pages budget on out-of-scope URLs.
func scopeFilterExpansion(scope Scope, seedURL string, expanded []string) []string {
	seedU, err := url.Parse(seedURL)
	if err != nil {
		return nil
	}
	var out []string
	for _, raw := range expanded {
		c, cerr := Canonicalize(raw)
		if cerr != nil {
			continue
		}
		cu, perr := url.Parse(c)
		if perr != nil {
			continue
		}
		if scope.Allows(seedU, cu) {
			out = append(out, c)
		}
	}
	return out
}
