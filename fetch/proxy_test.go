package fetch_test

// Proxy tests: MAGPIE_PROXY honored (hit), NO_PROXY bypass, invalid
// value loud, robots Checker rides the same guarded transport. All env
// via t.Setenv (parallel-safe, auto-restore).

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/fetch"
)

// newClosedPort returns a 127.0.0.1 host:port that reliably refuses
// connections (bound then released) — the deterministic dead entry.
func newClosedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// proxyURLhost strips the scheme: pool lines want "http://host:port"
// built from an httptest URL, redaction comparisons want host:port.
func proxyURLhost(proxyURL string) string {
	return strings.TrimPrefix(proxyURL, "http://")
}

// newProxyOrigin is a stdlib reverse proxy with a hit counter — the
// test-only egress stand-in (no proxy stub to maintain).
func newProxyOrigin(t *testing.T, hits *atomic.Int64, target string) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestProxy_Hit(t *testing.T) {
	var originHits, proxyHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("via proxy")) //nolint:errcheck // test server
	})
	proxyURL := newProxyOrigin(t, &proxyHits, origin.URL)
	t.Setenv("MAGPIE_PROXY", proxyURL)

	f := relaxedFetcher(t)
	resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.HTML) != "via proxy" {
		t.Errorf("body = %q, want via proxy", resp.HTML)
	}
	if n := proxyHits.Load(); n != 1 {
		t.Errorf("proxy hits = %d, want 1", n)
	}
	if n := originHits.Load(); n != 1 {
		t.Errorf("origin hits = %d, want 1", n)
	}
}

func TestProxy_NoProxyBypass(t *testing.T) {
	var originHits, proxyHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("direct")) //nolint:errcheck // test server
	})
	proxyURL := newProxyOrigin(t, &proxyHits, origin.URL)
	t.Setenv("MAGPIE_PROXY", proxyURL)
	// Hostname WITHOUT port: noProxyMatch keys on u.Hostname().
	t.Setenv("NO_PROXY", "127.0.0.1")

	f := relaxedFetcher(t)
	resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.HTML) != "direct" {
		t.Errorf("body = %q, want direct", resp.HTML)
	}
	if n := proxyHits.Load(); n != 0 {
		t.Errorf("proxy hits = %d, want 0 (NO_PROXY bypass)", n)
	}
}

func TestProxy_InvalidValue(t *testing.T) {
	var originHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should never be served")) //nolint:errcheck // test server
	})
	t.Setenv("MAGPIE_PROXY", "://bogus")

	f := relaxedFetcher(t)
	_, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x"})
	if err == nil || !strings.Contains(err.Error(), "MAGPIE_PROXY") {
		t.Fatalf("err = %v, want loud failure naming MAGPIE_PROXY", err)
	}
	if n := originHits.Load(); n != 0 {
		t.Errorf("origin hits = %d, want 0 (garbage proxy config must not reach the origin)", n)
	}
	// Sentinel-shaped configs fail too: wrong scheme, no host.
	for _, bad := range []string{"ftp://p.example:3128", "http://"} {
		t.Setenv("MAGPIE_PROXY", bad)
		if _, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x"}); err == nil || !strings.Contains(err.Error(), "MAGPIE_PROXY") {
			t.Errorf("MAGPIE_PROXY=%q: err = %v, want loud failure", bad, err)
		}
	}
}

func TestProxy_RobotsViaProxy(t *testing.T) {
	var proxyHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow:\n")) //nolint:errcheck // test server
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	proxyURL := newProxyOrigin(t, &proxyHits, origin.URL)
	t.Setenv("MAGPIE_PROXY", proxyURL)

	// The Checker shares GuardedTransport → robots fetches honor the proxy.
	c := crawl.NewChecker()
	ok, err := c.Allowed(t.Context(), origin.URL+"/page")
	if err != nil || !ok {
		t.Fatalf("Allowed via proxy = %v,%v, want true", ok, err)
	}
	if n := proxyHits.Load(); n < 1 {
		t.Errorf("proxy hits = %d, want ≥1 (robots fetch must ride the proxy)", n)
	}
}

// TestProxy_DialGuardSkipsProxyPeer is the regression test for the
// proxyconnect bug: with MAGPIE_PROXY set, the dial peer IS the
// operator-configured proxy (trusted egress, often localhost) — the
// guard must reject TARGETS (ValidateURL pre-dial), never the proxy
// itself. Strict options throughout: no test-binary relaxation involved.
// The flip side (public target + local proxy succeeds) is only provable
// against a routable origin — covered by manual smoke, noted here.
func TestProxy_DialGuardSkipsProxyPeer(t *testing.T) {
	var proxyHits, originHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never")) //nolint:errcheck // test server
	})
	proxyURL := newProxyOrigin(t, &proxyHits, origin.URL)
	t.Setenv("MAGPIE_PROXY", proxyURL)

	f, err := fetch.NewStaticFetcherWithOptions(fetch.SSRFOptions{}) // STRICT
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL})
	if !errors.Is(err, fetch.ErrPrivateAddress) {
		t.Fatalf("err = %v, want the target's SSRF rejection (policy holds behind a proxy)", err)
	}
	if strings.Contains(err.Error(), "proxyconnect") || strings.Contains(err.Error(), "dial peer") {
		t.Errorf("err = %v: the dial guard fired on the proxy peer — operator proxies are trusted egress", err)
	}
	if n := originHits.Load(); n != 0 {
		t.Errorf("origin hits = %d, want 0 (target still rejected pre-dial)", n)
	}
}

// --- Phase G pool extensions: failover, 4xx-not-egress, NO_PROXY×pool,
// run_history surfacing. The five tests above run unmodified. ---

// TestProxy_PoolFailover: entry1 is a closed port (connection refused ⇒
// egress error), entry2 is the reverse-proxy stand-in. The fetch must
// transparently serve via entry2 and surface the REDACTED endpoint
// (credentials from a sibling entry must never appear).
func TestProxy_PoolFailover(t *testing.T) {
	var originHits, proxyHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("via pool")) //nolint:errcheck // test server
	})
	proxyURL := newProxyOrigin(t, &proxyHits, origin.URL)
	closed := newClosedPort(t)
	poolFile := filepath.Join(t.TempDir(), "pool.txt")
	content := "http://cust:hunter2password@" + closed + "\nhttp://" + proxyURLhost(proxyURL) + "\n"
	if err := os.WriteFile(poolFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", poolFile)
	t.Setenv("MAGPIE_PROXY_STRATEGY", "")
	t.Setenv("NO_PROXY", "")

	f := relaxedFetcher(t)
	resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x"})
	if err != nil {
		t.Fatalf("failover fetch: %v", err)
	}
	if string(resp.HTML) != "via pool" {
		t.Errorf("body = %q, want via pool", resp.HTML)
	}
	if n := proxyHits.Load(); n != 1 {
		t.Errorf("entry2 proxy hits = %d, want 1", n)
	}
	if strings.Contains(resp.Proxy, "hunter2password") || strings.Contains(resp.Proxy, "cust@") {
		t.Errorf("resp.Proxy = %q, credentials leaked", resp.Proxy)
	}
	if resp.Proxy != proxyURLhost(proxyURL) {
		t.Errorf("resp.Proxy = %q, want the serving entry redacted to %s", resp.Proxy, proxyURLhost(proxyURL))
	}
}

// TestProxy_Pool4xxNotEgress: an HTTP 500 through entry1 is a PAGE
// outcome — the pool must NOT rotate (entry2 stays untouched, the 500
// body is returned). Failover on statuses would mask site errors as
// proxy churn.
func TestProxy_Pool4xxNotEgress(t *testing.T) {
	var badOriginHits, badProxyHits, goodProxyHits atomic.Int64
	badOrigin := hitOrigin(t, &badOriginHits, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "site exploded", http.StatusInternalServerError)
	})
	badProxy := newProxyOrigin(t, &badProxyHits, badOrigin.URL)
	var goodOriginHits atomic.Int64
	goodOrigin := hitOrigin(t, &goodOriginHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never")) //nolint:errcheck // test server
	})
	goodProxy := newProxyOrigin(t, &goodProxyHits, goodOrigin.URL)
	poolFile := filepath.Join(t.TempDir(), "pool.txt")
	content := "http://" + proxyURLhost(badProxy) + "\nhttp://" + proxyURLhost(goodProxy) + "\n"
	if err := os.WriteFile(poolFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", poolFile)
	t.Setenv("MAGPIE_PROXY_STRATEGY", "")
	t.Setenv("NO_PROXY", "")

	f := relaxedFetcher(t)
	resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: badOrigin.URL + "/x"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 passthrough", resp.StatusCode)
	}
	if n := badProxyHits.Load(); n != 1 {
		t.Errorf("bad proxy hits = %d, want exactly 1", n)
	}
	if n := goodProxyHits.Load(); n != 0 {
		t.Errorf("good proxy hits = %d, want 0 (5xx is a page outcome, never egress)", n)
	}
}

// TestProxy_PoolNoProxyBypass: NO_PROXY naming the target host must
// bypass the pool entirely (direct dial, peer check applies).
func TestProxy_PoolNoProxyBypass(t *testing.T) {
	var originHits, proxyHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("direct")) //nolint:errcheck // test server
	})
	proxyURL := newProxyOrigin(t, &proxyHits, origin.URL)
	poolFile := filepath.Join(t.TempDir(), "pool.txt")
	if err := os.WriteFile(poolFile, []byte("http://"+proxyURLhost(proxyURL)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", poolFile)
	t.Setenv("MAGPIE_PROXY_STRATEGY", "")
	t.Setenv("NO_PROXY", "127.0.0.1")

	f := relaxedFetcher(t)
	resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.HTML) != "direct" {
		t.Errorf("body = %q, want direct", resp.HTML)
	}
	if n := proxyHits.Load(); n != 0 {
		t.Errorf("proxy hits = %d, want 0 (NO_PROXY bypasses the pool)", n)
	}
}

// TestProxy_RequestBeatsEnv — Batch B: the per-run Proxy beats the env
// pool for THIS request, wins pre-I/O on garbage, and is opt-in (an
// identical request without Proxy rides the pool; the override is not
// sticky). The pool memoization is keyed per env value, so the two
// pools never cross-contaminate.
func TestProxy_RequestBeatsEnv(t *testing.T) {
	var originHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok")) //nolint:errcheck // test server
	})
	poolA, connsA, _ := fakeSOCKS5(t, mustURL(t, origin.URL))
	poolB, connsB, _ := fakeSOCKS5(t, mustURL(t, origin.URL))
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", poolFile(t, poolA))
	t.Setenv("NO_PROXY", "")

	f := relaxedFetcher(t)
	resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x", Proxy: poolB})
	if err != nil {
		t.Fatal(err)
	}
	if n := connsB.Load(); n != 1 {
		t.Errorf("request proxy conns = %d, want 1", n)
	}
	if n := connsA.Load(); n != 0 {
		t.Errorf("env pool conns = %d, want 0 (override must bypass the pool)", n)
	}
	if got, want := resp.Proxy, strings.TrimPrefix(poolB, "socks5://"); got != want {
		t.Errorf("surfaced proxy = %q, want %q (response audit trail)", got, want)
	}

	// Opt-in, not sticky: the same request WITHOUT Proxy rides pool A.
	if _, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/y"}); err != nil {
		t.Fatal(err)
	}
	if n := connsA.Load(); n != 1 {
		t.Errorf("env pool conns after pool-routed request = %d, want 1", n)
	}
	if n := connsB.Load(); n != 1 {
		t.Errorf("override must not be sticky: B conns = %d, want 1", n)
	}
}

// TestProxy_BadScheme — Batch B: a bad per-run proxy is a typed
// ErrProxyConfig failure BEFORE any I/O, message naming the field.
func TestProxy_BadScheme(t *testing.T) {
	var originHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {})
	for _, bad := range []string{"ftp://p.example:3128", "not-a-url", "127.0.0.1:9999", "http://"} {
		_, err := relaxedFetcher(t).Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL + "/x", Proxy: bad})
		if err == nil || !errors.Is(err, fetch.ErrProxyConfig) {
			t.Errorf("Proxy %q: err = %v, want ErrProxyConfig family", bad, err)
			continue
		}
		if !strings.Contains(err.Error(), "per-run proxy") {
			t.Errorf("Proxy %q: err %q must name the per-run field", bad, err)
		}
	}
	if n := originHits.Load(); n != 0 {
		t.Errorf("origin hits = %d, want 0 (bad proxy config must fail pre-I/O)", n)
	}
}
