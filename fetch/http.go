package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

// StaticFetcher is the spec §1.3 net/http fetcher.
type StaticFetcher struct {
	client   *http.Client
	browsers browserClients // per-profile impersonating clients (fetch/utls.go)
	jar      http.CookieJar // shared stock + browser so warmup cookies carry over
	ua       string
	ssrf     SSRFOptions
}

// defaultHeaders is the single static-fetch header bundle (spec §1.3).
var defaultHeaders = map[string]string{
	"User-Agent":      "github.com/motherlodelab/magpie/1.0 (+https://github.com/you/magpie)",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Accept-Language": "fr-FR,fr;q=0.9,en;q=0.8",
}

// GuardedTransport builds the shared transport with the SSRF dial guard,
// proxy resolution, and the file:// handler. Used by the static fetcher
// AND the crawl robots Checker so every outbound connection honors the
// same policy. Options come from the environment/test-binary hatch; see
// NewStaticFetcherWithOptions for explicit options.
// Phase E note: a uTLS transport swap must preserve the DialContext peer
// check and the Proxy func — they are the SSRF/egress policy, not
// incidentals of the stock transport.
func GuardedTransport() *http.Transport {
	return guardedTransport(resolvedSSRFOptions())
}

// GuardedTransportWithOptions builds the same transport with explicit
// SSRF options — for clients whose PEER is entirely operator-configured
// (e.g. the search provider registry + MAGPIE_SEARXNG_URL), where a
// localhost/LAN endpoint is the legitimate deployment shape. Targets
// from untrusted input never go through such a client.
func GuardedTransportWithOptions(o SSRFOptions) *http.Transport {
	return guardedTransport(o)
}

func guardedTransport(o SSRFOptions) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           guardedDialFunc(o, dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
	}
	transport.RegisterProtocol("file", http.NewFileTransport(http.Dir("/")))
	return transport
}

// guardedDialFunc is the raw-TCP dial behind both the stock transport
// and Phase E's browser dial: plain dial + post-connect peer check. The
// check is the TOCTOU backstop shared by every transport we run.
func guardedDialFunc(o SSRFOptions, dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		// Operator-configured egress (MAGPIE_PROXY / proxy pool) →
		// this peer is the chosen proxy (trusted, often localhost),
		// not the SSRF target: the policy applies to target URLs via
		// ValidateURL + CheckRedirect. The decision is per dial-addr:
		// proxied requests dial the proxy host (trusted), direct
		// requests dial the target (peer-checked, and NO_PROXY-exempt
		// targets resolve to nil here — safe default).
		host := addr
		if h, _, serr := net.SplitHostPort(addr); serr == nil {
			host = h
		}
		proxied := proxiedForHost(host)
		if !dialPeerAllowed(conn.RemoteAddr(), o, proxied) {
			_ = conn.Close() //nolint:errcheck // rejection path; close error unactionable
			return nil, ssrfErr("fetch: dial peer %s is not a public address (DNS rebind?)", conn.RemoteAddr())
		}
		return conn, nil
	}
}

// dialPeerAllowed re-checks the CONNECTED peer address, closing the TOCTOU
// window between ValidateURL's DNS answer and the actual socket: no HTTP
// byte is written before this passes. Proxied connections skip the check —
// their peer is the trusted proxy, and the proxy (not us) resolves the
// target, so a post-connect check tells us nothing about the target anyway.
func dialPeerAllowed(addr net.Addr, o SSRFOptions, proxied bool) bool {
	if proxied || o.AllowPrivate {
		return true
	}
	ip, ok := addrIP(addr)
	return ok && isPublicIP(ip)
}

// addrIP extracts the IP from a dial peer. http.Transport only dials tcp,
// so anything else fails closed.
func addrIP(addr net.Addr) (netip.Addr, bool) {
	ta, ok := addr.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(ta.IP)
	return a, ok
}

// proxyFunc resolves the proxy per request: the pool (MAGPIE_PROXY_FILE,
// then MAGPIE_PROXY) wins over the standard HTTP(S)_PROXY environment,
// with a minimal NO_PROXY exact/dot-suffix bypass. Read per request so
// env changes take effect without rebuilding the transport. The chosen
// pick is recorded for the failover loop and response surfacing.
func proxyFunc(req *http.Request) (*url.URL, error) {
	u, p, idx, err := proxyForHostPick(req.URL.Hostname())
	if err == nil && p != nil {
		if ref := pickRefFrom(req.Context()); ref != nil {
			ref.set(p, idx, u)
		}
	}
	return u, err
}

// noProxyMatch: comma-separated NO_PROXY entries, exact or dot-suffix
// host match, "*" bypasses everything. Deliberately dumb (no CIDR, no
// port matching) — same shape as the plan spec.
func noProxyMatch(host string) bool {
	np := os.Getenv("NO_PROXY")
	if np == "" {
		np = os.Getenv("no_proxy")
	}
	for _, e := range strings.Split(np, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if e == "*" || host == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

// homepageOf returns scheme://host/ for warmup, or "" for non-http URLs.
func homepageOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/"
}

// CanHandle is always true for the static fetcher.
func (s *StaticFetcher) CanHandle(req FetchRequest) bool { return true }

// Close drains the stock transport's idle connections. The browser
// clients' h2 connections have no CloseIdleConnections passthrough in
// the wrapper — ponytail: they die with the process; ceiling is CLI-
// lifetime connections only, hence --browser documented scrape-first.
func (s *StaticFetcher) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

// Fetch performs one GET with a per-attempt timeout budget, wrapped in
// the proxy-pool failover loop (dial/CONNECT/timeout errors rotate to the
// next entry; HTTP statuses are page outcomes, never proxy failures). On
// a challenge response it warms the cookie jar with one homepage GET then
// retries the original URL exactly once. A typed challenge that survives
// the retry becomes *ChallengeError; untyped challenge bodies pass
// through to the quality gate as before (no loops).
func (s *StaticFetcher) Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	if req.URL == "" {
		return nil, fmt.Errorf("fetch: empty URL")
	}
	resp, err := s.doWithFailover(ctx, req)
	if err != nil {
		return nil, err
	}
	vendor := DetectChallenge(resp.HTML, resp.Headers, resp.StatusCode)
	classified := vendor != "" || IsChallengePage(resp.HTML, resp.StatusCode)
	// Warmup fires for profiles, browser fingerprints, AND typed
	// challenges: --browser requests and vendor-verified challenge pages
	// are exactly the blocked-page case. Untyped challenge bodies on a
	// bare request keep the old passthrough contract (quality gate owns
	// them); clean pages never warm up.
	impersonated := req.Profile != "" || req.Browser != "" || vendor != ""
	if !classified || !impersonated {
		return resp, nil
	}
	if home := homepageOf(req.URL); home != "" {
		_, _ = s.do(ctx, FetchRequest{URL: home, Timeout: req.Timeout, Profile: req.Profile, Cookies: req.Cookies, Browser: req.Browser}) //nolint:errcheck // warmup best-effort; retry proceeds regardless
	}
	// The retry is a single attempt by design: it is already the second
	// chance after a response arrived, so egress failover does not apply.
	retry, rerr := s.do(ctx, req)
	if rerr != nil {
		return nil, rerr
	}
	if v := DetectChallenge(retry.HTML, retry.Headers, retry.StatusCode); v != "" {
		return nil, &ChallengeError{Vendor: v, StatusCode: retry.StatusCode, URL: req.URL}
	}
	return retry, nil
}

// doWithFailover wraps one attempt in the pool retry loop: at most
// min(3, pool size) attempts, and only on egress-shaped errors. ponytail:
// a caller-canceled ctx fails all attempts fast, so no separate cancel
// check — the bounded loop is the ceiling.
func (s *StaticFetcher) doWithFailover(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	attempts := 1
	if p, err := currentPool(); err == nil && p != nil {
		attempts = min(3, len(p.entries))
	}
	var resp *FetchResponse
	var err error
	for i := 0; ; i++ {
		ref := &pickRef{}
		resp, err = s.do(context.WithValue(ctx, pickKey{}, ref), req)
		if err != nil && isEgressError(err) {
			if p, idx, _ := ref.chosen(); p != nil {
				p.reportFailure(idx)
			}
		}
		if err == nil || i+1 >= attempts || !isEgressError(err) {
			return resp, err
		}
	}
}

// isEgressError reports whether err looks like a dial/CONNECT/timeout
// failure (proxy rotation is worth a retry) rather than a page outcome.
func isEgressError(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "proxyconnect") || strings.Contains(msg, "socks connect")
}

func (s *StaticFetcher) do(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	// Pre-dial SSRF gate: a rejected URL never opens a socket (the strict
	// fetcher test proves the origin's hit counter stays 0). go-rod
	// escalation only runs on content already fetched through this client,
	// so a blocked URL never reaches the browser.
	if err := ValidateURL(ctx, req.URL, nil, s.ssrf); err != nil {
		return nil, err
	}
	timeout := budget(req)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Reuse a caller-injected pickRef (doWithFailover) so failed picks are
	// reported and successes surface the serving entry; create one only
	// for bare do() calls (warmup, smoke tests).
	ref := pickRefFrom(ctx)
	if ref == nil {
		ref = &pickRef{}
		cctx = context.WithValue(cctx, pickKey{}, ref)
	}
	client, err := s.clientFor(req)
	if err != nil {
		return nil, err
	}
	browserPath := client != s.client
	hreq, err := http.NewRequestWithContext(cctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	// Effective headers: an impersonated https request without an explicit
	// profile gets NO headers from us — the wrapper injects its matching
	// browser set (a stale Phase-A UA next to a fresh hello is a fingerprint
	// tell). An explicit Profile is the caller's override and wins.
	// Cleartext never reaches the wrapper, so cleartext+browser falls back
	// to our matching header profile (a real UA beats none).
	headers := profileHeaders(req.Profile)
	if browserPath && req.Profile == "" {
		headers = nil
	} else if !browserPath && req.Profile == "" && req.Browser != "" {
		if p, ok := resolveBrowser(req.Browser); ok {
			headers = profileHeaders(p.Name)
		}
	}
	for k, v := range headers {
		hreq.Header.Set(k, v)
	}
	// Lang overrides the profile bundle's Accept-Language — one merge
	// site, after profiles (profile maps are never mutated).
	if req.Lang != "" {
		hreq.Header.Set("Accept-Language", req.Lang)
	}
	cookies := req.Cookies
	if cookies != "" {
		hreq.Header.Set("Cookie", cookies)
	}
	// Run headers merge LAST — the caller wins over the profile bundle,
	// Lang, and Cookies (one directional merge; the conflict flip is
	// pinned by a single test, a matrix would test the stdlib).
	for _, h := range req.Headers {
		if name, val, ok := strings.Cut(h, ":"); ok {
			hreq.Header.Set(strings.TrimSpace(name), strings.TrimSpace(val))
		}
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body fully read above; close error unactionable
	// 50 MB cap on the DECODED stream: a gzip bomb (50 KB on the wire →
	// 60 MB+ decoded) is truncated at exactly the cap. Pinned by
	// TestStaticGzipBomb; if the cap changes intentionally, that test
	// should be updated with it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, fmt.Errorf("fetch: read body: %w", err)
	}
	// Browser-path responses arrive encoded: the wrapper's h2 never
	// decompresses and its h1 sees the profile's Accept-Encoding. Decode
	// here (re-capped) so callers always see plain bytes.
	if browserPath {
		if body, err = decodeBody(body, resp.Header); err != nil {
			return nil, fmt.Errorf("fetch: decode body: %w", err)
		}
	}
	finalURL := req.URL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	out := &FetchResponse{
		URL:        req.URL,
		FinalURL:   finalURL,
		StatusCode: resp.StatusCode,
		HTML:       body,
		Headers:    resp.Header,
	}
	if _, _, u := ref.chosen(); u != nil {
		out.Proxy = RedactProxy(u)
	}
	return out, nil
}
