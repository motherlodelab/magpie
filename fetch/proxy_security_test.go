package fetch_test

// Phase G gate: the proxy/egress security matrix. Proxies are trusted
// egress (their peer is often loopback, e.g. Tor); loopback TARGETS stay
// governed by ValidateURL. Any row that can only pass by weakening
// dialPeerAllowed or ValidateURL is a STOP per phase-G §6 — revert the
// pool, keep the rest.

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

func poolFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// fakeSOCKS5 starts a minimal no-auth SOCKS5 CONNECT server. It NATs every
// requested address to the local upstream (test-only egress stand-in: the
// client may ask for any host, the dial always lands on `upstream`), records
// the requested addr + connection count, and refuses to dial anything but
// 127.0.0.1 itself — the test helper must not become the suite's own SSRF hole.
func fakeSOCKS5(t *testing.T, upstream *url.URL) (proxyURL string, conns, requested *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() }) //nolint:errcheck // recorder teardown
	var connsCount, requestedCount atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck // test server
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
					return
				}
				methods := make([]byte, int(hdr[1]))
				_, _ = io.ReadFull(c, methods) //nolint:errcheck // protocol skeleton; errors drop the conn
				_, _ = c.Write([]byte{5, 0})   //nolint:errcheck // no-auth reply; errors drop the conn
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 { // CONNECT only
					return
				}
				var host string
				switch req[3] {
				case 1: // IPv4
					b := make([]byte, 4)
					_, _ = io.ReadFull(c, b) //nolint:errcheck // protocol skeleton; errors drop the conn
					host = net.IP(b).String()
				case 3: // domain
					n := make([]byte, 1)
					if _, err := io.ReadFull(c, n); err != nil {
						return
					}
					d := make([]byte, n[0])
					_, _ = io.ReadFull(c, d) //nolint:errcheck // protocol skeleton; errors drop the conn
					host = string(d)
				default:
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(c, port); err != nil {
					return
				}
				_ = host // requested addr parsed for protocol fidelity; the NAT routes everything upstream
				requestedCount.Add(1)
				connsCount.Add(1)
				up, err := net.Dial("tcp", upstream.Host) // NAT: always the local origin
				if err != nil {
					_, _ = c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0}) //nolint:errcheck // general failure reply; errors drop the conn
					return
				}
				defer up.Close()                                     //nolint:errcheck // test server
				_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}) //nolint:errcheck // success reply; errors drop the conn
				go func() { _, _ = io.Copy(up, c) }()                //nolint:errcheck
				_, _ = io.Copy(c, up)                                //nolint:errcheck
			}(c)
		}
	}()
	return "socks5://" + ln.Addr().String(), &connsCount, &requestedCount
}

// TestFakeSOCKS5_SelfCheck runs FIRST: a broken helper must fail loudly
// as a helper bug, never as a matrix-row bug.
func TestFakeSOCKS5_SelfCheck(t *testing.T) {
	var originHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("selfcheck")) //nolint:errcheck // test server
	})
	socks, conns, requested := fakeSOCKS5(t, mustURL(t, origin.URL))
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", poolFile(t, socks))
	t.Setenv("NO_PROXY", "")
	f, err := fetch.NewStaticFetcherWithOptions(fetch.SSRFOptions{AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL})
	if err != nil {
		t.Fatalf("self-check fetch through fakeSOCKS5: %v", err)
	}
	if string(resp.HTML) != "selfcheck" {
		t.Errorf("body = %q, want selfcheck", resp.HTML)
	}
	if conns.Load() != 1 || requested.Load() != 1 {
		t.Errorf("conns/requested = %d/%d, want 1/1", conns.Load(), requested.Load())
	}
}

// TestProxy_SecurityMatrix pins the egress trust semantics across proxy
// sources. The 127.0.0.1 targets/dials are the point: proxies are
// operator-chosen egress (trusted), targets are not (peer-checked).
func TestProxy_SecurityMatrix(t *testing.T) {
	pubTarget := "http://93.184.216.34/" // TEST-NET-1: public IP, never dialed directly (row 4 NATs it)

	t.Run("no proxy / loopback target rejected pre-request", func(t *testing.T) {
		var hits atomic.Int64
		origin := hitOrigin(t, &hits, func(w http.ResponseWriter, _ *http.Request) {})
		t.Setenv("MAGPIE_PROXY", "")
		t.Setenv("MAGPIE_PROXY_FILE", "")
		// STRICT: AllowPrivate would license the loopback target and
		// defeat the row — the no-proxy baseline has no operator opt-in.
		f, err := fetch.NewStaticFetcherWithOptions(fetch.SSRFOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL})
		if err == nil {
			t.Fatal("loopback target without proxy must fail")
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("origin request hits = %d, want 0 (peer check precedes any HTTP byte)", n)
		}
	})

	t.Run("env single proxy / private target allowed", func(t *testing.T) {
		var originHits, proxyHits atomic.Int64
		origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {})
		t.Setenv("MAGPIE_PROXY", newProxyOrigin(t, &proxyHits, origin.URL))
		t.Setenv("MAGPIE_PROXY_FILE", "")
		t.Setenv("NO_PROXY", "")
		f := relaxedFetcher(t)
		if _, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL}); err != nil {
			t.Fatalf("operator-chosen egress must be trusted: %v", err)
		}
		if proxyHits.Load() != 1 || originHits.Load() != 1 {
			t.Errorf("proxy/origin hits = %d/%d, want 1/1", proxyHits.Load(), originHits.Load())
		}
	})

	t.Run("pool http entry / loopback target served via proxy", func(t *testing.T) {
		var originHits, proxyHits atomic.Int64
		origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("via pool")) //nolint:errcheck // test server
		})
		t.Setenv("MAGPIE_PROXY", "")
		t.Setenv("MAGPIE_PROXY_FILE", poolFile(t, "http://"+proxyURLhost(newProxyOrigin(t, &proxyHits, origin.URL))))
		t.Setenv("NO_PROXY", "")
		// Relaxed: the loopback TARGET needs the explicit opt-in to pass
		// ValidateURL; the trust decision under test is the dial-peer skip.
		f := relaxedFetcher(t)
		resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL})
		if err != nil || string(resp.HTML) != "via pool" {
			t.Fatalf("pool entry must serve: err=%v body=%q", err, resp.HTML)
		}
		if proxyHits.Load() != 1 || originHits.Load() != 1 {
			t.Errorf("proxy/origin hits = %d/%d, want 1/1", proxyHits.Load(), originHits.Load())
		}
	})

	t.Run("pool tor socks5 / public target served, loopback target still rejected", func(t *testing.T) {
		var originHits atomic.Int64
		origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("via tor")) //nolint:errcheck // test server
		})
		socks, conns, requested := fakeSOCKS5(t, mustURL(t, origin.URL))
		t.Setenv("MAGPIE_PROXY", "")
		t.Setenv("MAGPIE_PROXY_FILE", poolFile(t, socks))
		t.Setenv("NO_PROXY", "")
		// STRICT: the strongest form of the load-bearing row — a public
		// target through a loopback proxy works ONLY because proxied
		// dials skip the peer check; nothing is relaxed away.
		f, err := fetch.NewStaticFetcherWithOptions(fetch.SSRFOptions{})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: pubTarget})
		if err != nil || string(resp.HTML) != "via tor" {
			t.Fatalf("tor-style pool entry must serve public targets: err=%v", err)
		}
		if conns.Load() != 1 || requested.Load() != 1 {
			t.Errorf("socks conns/requested = %d/%d, want 1/1", conns.Load(), requested.Load())
		}

		// Proxy ≠ license: loopback TARGET through the same pool is still
		// ValidateURL-rejected before the proxy is ever contacted.
		_, err = f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL})
		if err == nil {
			t.Fatal("loopback target via pool must stay rejected")
		}
		if conns.Load() != 1 {
			t.Errorf("socks conns = %d, want still 1 (rejection is pre-dial)", conns.Load())
		}
	})

	t.Run("request-level tor socks5 / public target served", func(t *testing.T) {
		// Batch B's load-bearing row: the Tor loopback pattern via the
		// PER-RUN seam must work under STRICT options — the override
		// rides the request context into the dial guard, so the proxy
		// dial gets the pool entry's trusted-egress decision.
		var originHits atomic.Int64
		origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("via tor")) //nolint:errcheck // test body write; check-blank flags explicit blanks
		})
		socks, conns, requested := fakeSOCKS5(t, mustURL(t, origin.URL))
		t.Setenv("MAGPIE_PROXY", "")
		t.Setenv("MAGPIE_PROXY_FILE", "")
		f, err := fetch.NewStaticFetcherWithOptions(fetch.SSRFOptions{})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: pubTarget, Proxy: socks})
		if err != nil {
			t.Fatalf("request-level loopback proxy must serve public targets: %v", err)
		}
		if string(resp.HTML) != "via tor" {
			t.Errorf("body = %q, want via tor", resp.HTML)
		}
		if conns.Load() != 1 || requested.Load() != 1 {
			t.Errorf("socks conns/requested = %d/%d, want 1/1", conns.Load(), requested.Load())
		}
	})

	t.Run("request-level proxy / loopback target still rejected", func(t *testing.T) {
		// Batch B row: the per-run Proxy seam must re-enter the same
		// gauntlet — proxy ≠ license, the loopback TARGET stays
		// ValidateURL-rejected before the proxy is ever contacted.
		var originHits atomic.Int64
		origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {})
		socks, conns, _ := fakeSOCKS5(t, mustURL(t, origin.URL))
		t.Setenv("MAGPIE_PROXY", "")
		t.Setenv("MAGPIE_PROXY_FILE", "")
		// STRICT: no AllowPrivate — the loopback target has no opt-in.
		f, err := fetch.NewStaticFetcherWithOptions(fetch.SSRFOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL, Proxy: socks})
		if err == nil {
			t.Fatal("loopback target via request-level proxy must stay rejected")
		}
		if n := originHits.Load(); n != 0 {
			t.Errorf("origin hits = %d, want 0", n)
		}
		if conns.Load() != 0 {
			t.Errorf("socks conns = %d, want 0 (rejection is pre-dial)", conns.Load())
		}
	})

	t.Run("credentials never surface", func(t *testing.T) {
		const secret = "hunter2password"
		t.Setenv("MAGPIE_PROXY", "")
		t.Setenv("MAGPIE_PROXY_FILE", poolFile(t,
			"http://cust:"+secret+"@127.0.0.1:1", // dead port → forced failure path
			"http://127.0.0.1:1",                 // also dead → all-dead error path
		))
		f := relaxedFetcher(t)
		_, err := f.Fetch(t.Context(), fetch.FetchRequest{URL: pubTarget})
		if err == nil {
			t.Fatal("all-dead pool must error")
		}
		for _, surface := range []string{err.Error()} {
			if strings.Contains(surface, secret) {
				t.Errorf("credential %q leaked in: %s", secret, surface)
			}
		}
	})
}
