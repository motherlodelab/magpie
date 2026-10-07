//go:build browser

package fetch // internal: the lookup and launch seams are unexported

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
)

// skipNoBrowser skips on an environment without a launchable Chrome — the
// browser tier's existing convention.
func skipNoBrowser(t *testing.T, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser")) {
		t.Skipf("no browser available: %v", err)
	}
}

// TestOpenPage_RefusesNonPublic (desktop QA B2): the browser path refuses
// what the static path refuses — loopback, link-local metadata, file:// —
// with the typed ErrPrivateAddress before any tab loads. Vacuity guard:
// the same loopback fetch with AllowPrivate reaches the server.
func TestOpenPage_RefusesNonPublic(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" { // Chrome's /favicon.ico probe is not the page
			hits.Add(1)
		}
		_, _ = w.Write([]byte("<html><body><p>internal</p></body></html>")) //nolint:errcheck // test server
	}))
	defer srv.Close()

	r := NewRodFetcher()
	r.SSRF = SSRFOptions{} // strict, despite the test-binary hatch
	t.Cleanup(func() { _ = r.Close() })
	for _, u := range []string{srv.URL + "/", "http://169.254.169.254/latest/meta-data/", "file:///etc/hosts"} {
		_, err := r.FetchWithActions(t.Context(), FetchRequest{URL: u}, nil)
		skipNoBrowser(t, err)
		if !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("FetchWithActions(%q) err = %v, want ErrPrivateAddress", u, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("loopback server hit %d times — the browser dialed a refused target", n)
	}

	open := NewRodFetcher()
	open.SSRF = SSRFOptions{AllowPrivate: true}
	t.Cleanup(func() { _ = open.Close() })
	resp, err := open.FetchWithActions(t.Context(), FetchRequest{URL: srv.URL + "/"}, nil)
	skipNoBrowser(t, err)
	if err != nil || !strings.Contains(string(resp.HTML), "internal") || hits.Load() != 1 {
		t.Fatalf("AllowPrivate fetch = (err %v, hits %d) — the refusal above proves nothing", err, hits.Load())
	}
}

// redirectWorld: /start 302s to the loopback /secret; secretHits counts
// whether Chrome ever reached it. Served on 127.0.0.1, reached by Chrome
// as public.test via host-resolver-rules, validated as public by a fake
// lookup — so only the per-hop check can stop the second request.
func redirectWorld(t *testing.T) (startURL string, secretHits *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, srv.URL+"/secret", http.StatusFound) // srv.URL is http://127.0.0.1:<port>
		case "/secret":
			hits.Add(1)
			_, _ = w.Write([]byte("<html><body>internal</body></html>")) //nolint:errcheck // test server
		}
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	return "http://public.test:" + port + "/start", &hits
}

// publicLookup answers public.test with a public address for ValidateURL;
// everything else falls through to the real resolver.
func publicLookup(ctx context.Context, host string) ([]net.IP, error) {
	if host == "public.test" {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// TestOpenPage_RefusesRedirectToNonPublic (desktop QA B2): a public entry
// that 302s to loopback is aborted at the document request — /secret is
// never hit and the error is the typed refusal. Vacuity guard: the same
// redirect with AllowPrivate is followed and /secret is hit once.
func TestOpenPage_RefusesRedirectToNonPublic(t *testing.T) {
	start, secretHits := redirectWorld(t)
	rod := func(o SSRFOptions) *RodFetcher {
		r := NewRodFetcher()
		r.SSRF, r.lookup = o, publicLookup
		r.launch = func(l *launcher.Launcher) { l.Set("host-resolver-rules", "MAP public.test 127.0.0.1") }
		t.Cleanup(func() { _ = r.Close() })
		return r
	}

	_, err := rod(SSRFOptions{}).FetchWithActions(t.Context(), FetchRequest{URL: start}, nil)
	skipNoBrowser(t, err)
	if !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("redirect to loopback err = %v, want ErrPrivateAddress", err)
	}
	if n := secretHits.Load(); n != 0 {
		t.Fatalf("/secret hit %d times — the redirect hop was followed", n)
	}

	resp, err := rod(SSRFOptions{AllowPrivate: true}).FetchWithActions(t.Context(), FetchRequest{URL: start}, nil)
	if err != nil || !strings.Contains(string(resp.HTML), "internal") || secretHits.Load() != 1 {
		t.Fatalf("AllowPrivate redirect = (err %v, /secret hits %d) — the refusal above proves nothing", err, secretHits.Load())
	}
}
