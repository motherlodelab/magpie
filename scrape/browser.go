// Browser-path helpers: the rod escalation and screenshot capture used by
// Run's fetch dispatch. Split from scrape.go — same package, no import
// changes.

package scrape

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/motherlodelab/magpie/fetch"
)

// parseViewport parses the WxH screenshot viewport shape (0,0 = default).
func parseViewport(v string) (int, int, error) {
	if v == "" {
		return 0, 0, nil
	}
	w, h, ok := strings.Cut(v, "x")
	if !ok {
		return 0, 0, fmt.Errorf("want WxH")
	}
	pw, err1 := strconv.Atoi(w)
	ph, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("want WxH")
	}
	return pw, ph, nil
}

// screenshotPage captures a full-page PNG through a fresh browser;
// with actions the capture joins the action session as its final step.
func screenshotPage(ctx context.Context, rawURL string, o Options) ([]byte, error) {
	w, h, err := parseViewport(o.Viewport)
	if err != nil {
		return nil, &OptionsError{fmt.Sprintf("scrape: viewport %q must be WxH (e.g. 1280x800)", o.Viewport)}
	}
	acts, err := fetch.ParseActions(o.Actions)
	if err != nil {
		return nil, err
	}
	rod := fetch.NewRodFetcher()
	rod.CDP, rod.Proxy = o.CDP, o.Proxy
	defer func() { _ = rod.Close() }() //nolint:errcheck // browser teardown; failure unactionable
	if len(acts) == 0 {
		return rod.Screenshot(ctx, rawURL, w, h)
	}
	return rod.ScreenshotActions(ctx, rawURL, w, h, acts)
}

func fetchBrowserChecked(ctx context.Context, rawURL string, o Options) (*fetch.FetchResponse, error) {
	resp, err := fetchBrowser(ctx, rawURL, o)
	if err != nil {
		return nil, err
	}
	// A challenge that survives the browser is still a challenge: type it
	// instead of shipping the interstitial DOM to cleaning (which reads it
	// as an empty page and reports the misleading "quality blocked (empty)").
	vendor := fetch.DetectChallenge(resp.HTML, resp.Headers, resp.StatusCode)
	if vendor == "" {
		vendor = fetch.DetectChallengeRendered(resp.HTML)
	}
	if vendor != "" {
		return nil, &fetch.ChallengeError{Vendor: vendor, StatusCode: resp.StatusCode, URL: rawURL}
	}
	return resp, nil
}

func fetchBrowser(ctx context.Context, rawURL string, o Options) (*fetch.FetchResponse, error) {
	// ValidateOptions pre-flighted the lines for Run; direct callers get
	// the typed line error here.
	acts, err := fetch.ParseActions(o.Actions)
	if err != nil {
		return nil, err
	}
	rod := fetch.NewRodFetcher()
	rod.CDP, rod.Proxy = o.CDP, o.Proxy
	defer func() { _ = rod.Close() }() //nolint:errcheck // browser teardown; failure unactionable
	// The browser path rides the SAME request fields as the static path
	// (scrape.go's one full FetchRequest): Headers (UA override via
	// openPage) and Cookies ride every fetcher path — escalation, actions,
	// render=browser. Dropping them here logged the browser out and
	// re-emulated the device UA on every escalated/actions fetch.
	return rod.FetchWithActions(ctx, fetch.FetchRequest{
		URL: rawURL, Lang: o.Lang, CaptureXHR: o.CaptureXHR,
		Headers: o.Headers, Cookies: o.Cookies,
	}, acts)
}
