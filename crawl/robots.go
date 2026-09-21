package crawl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jimsmart/grobotstxt"

	"github.com/motherlodelab/magpie/fetch"
)

// ErrRobotsUnreachable marks a host whose robots.txt is 5xx or unfetchable.
// Per RFC 9309 §2.3.1 the crawler MUST assume complete disallow.
var ErrRobotsUnreachable = errors.New("robots.txt unreachable")

// Checker enforces robots.txt per host, fetching each body once per run.
type Checker struct {
	client *http.Client
	mu     sync.Mutex
	bodies map[string]robotsEntry
	token  string // bare product token, e.g. "magpie"
	// proxy is the per-run egress override (crawl.Options.Proxy,
	// validated): robots fetches ride it so a proxy-only site's politeness
	// check uses the same egress as its page fetches. nil = env pool.
	proxy *url.URL
}

type robotsEntry struct {
	body    string
	delay   time.Duration
	sitemap []string
	denyAll bool
}

const robotsUA = "github.com/motherlodelab/magpie/1.0 (+https://github.com/you/magpie)"

// NewChecker builds a Checker with the bare product token derived from the UA.
// The client shares fetch.GuardedTransport: robots fetches honor
// MAGPIE_PROXY and the SSRF dial guard like every other request.
func NewChecker() *Checker {
	return &Checker{
		client: &http.Client{Timeout: 10 * time.Second, Transport: fetch.GuardedTransport()},
		bodies: map[string]robotsEntry{},
		token:  "magpie",
	}
}

// UseProxy routes robots.txt fetches through u (a validated per-run
// proxy from fetch.ValidateRequestProxy). Call at construction, before
// the first Allowed/CrawlDelay/Sitemaps — bodies are cached per run,
// so a later swap would only affect hosts not yet loaded.
func (c *Checker) UseProxy(u *url.URL) { c.proxy = u }

// delayHandler collects Crawl-Delay for groups matching * or our token.
type delayHandler struct {
	token   string
	inGroup bool
	delay   time.Duration
}

func (h *delayHandler) HandleUserAgent(_ int, value string) {
	if value == "*" || strings.EqualFold(value, h.token) {
		h.inGroup = true
	}
}
func (h *delayHandler) HandleRobotsStart()             {}
func (h *delayHandler) HandleRobotsEnd()               {}
func (h *delayHandler) HandleAllow(_ int, _ string)    {}
func (h *delayHandler) HandleDisallow(_ int, _ string) {}
func (h *delayHandler) HandleSitemap(_ int, _ string)  {}
func (h *delayHandler) HandleUnknownAction(_ int, action, value string) {
	if !h.inGroup || !strings.EqualFold(action, "crawl-delay") {
		return
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || f <= 0 {
		return
	}
	if d := time.Duration(f * float64(time.Second)); d > h.delay {
		h.delay = d
	}
}

// load fetches and parses one host's robots.txt (cached per run).
func (c *Checker) load(ctx context.Context, host, scheme string) robotsEntry {
	c.mu.Lock()
	if e, ok := c.bodies[host]; ok {
		c.mu.Unlock()
		return e
	}
	c.mu.Unlock()

	e := c.fetchRobots(ctx, scheme+"://"+host+"/robots.txt")
	c.mu.Lock()
	c.bodies[host] = e
	c.mu.Unlock()
	return e
}

func (c *Checker) fetchRobots(ctx context.Context, robotsURL string) robotsEntry {
	ctx = fetch.WithRequestProxy(ctx, c.proxy) // no-op without a per-run proxy
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return robotsEntry{denyAll: true}
	}
	req.Header.Set("User-Agent", robotsUA)
	resp, err := c.client.Do(req)
	if err != nil {
		return robotsEntry{denyAll: true}
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body fully read; close unactionable
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return robotsEntry{denyAll: true}
	}
	switch {
	case resp.StatusCode/100 == 2:
		h := &delayHandler{token: c.token}
		grobotstxt.Parse(string(body), h)
		return robotsEntry{body: string(body), delay: h.delay, sitemap: grobotstxt.Sitemaps(string(body))}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return robotsEntry{} // unavailable → allow all
	default:
		return robotsEntry{denyAll: true} // 5xx → disallow all
	}
}

// Allowed reports whether rawURL may be fetched. Returns
// ErrRobotsUnreachable (wrapped) when the host disallows via 5xx/network.
func (c *Checker) Allowed(ctx context.Context, rawURL string) (bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false, fmt.Errorf("crawl: robots: bad url %q", rawURL)
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	e := c.load(ctx, u.Host, scheme)
	if e.denyAll {
		return false, fmt.Errorf("crawl: robots: %w for host %s", ErrRobotsUnreachable, u.Host)
	}
	if e.body == "" {
		return true, nil
	}
	if !grobotstxt.AgentAllowed(e.body, c.token, rawURL) {
		return false, nil
	}
	return true, nil
}

// CrawlDelay returns the parsed crawl-delay floor for the URL's host.
func (c *Checker) CrawlDelay(ctx context.Context, rawURL string) time.Duration {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return 0
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return c.load(ctx, u.Host, scheme).delay
}

// Sitemaps returns robots-declared sitemap URLs for the URL's host.
func (c *Checker) Sitemaps(ctx context.Context, rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return c.load(ctx, u.Host, scheme).sitemap
}
