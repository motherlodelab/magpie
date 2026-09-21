//go:build browser

package fetch_test

// Batch B browser tier: the per-run Proxy reaches the rod launcher as
// --proxy-server — the SOCKS CONNECT is observed by the fake, proving
// the flag (not the env pool) carried it.

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

func TestRod_ProxyLauncher(t *testing.T) {
	var originHits atomic.Int64
	origin := hitOrigin(t, &originHits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("via browser proxy")) //nolint:errcheck // test server
	})
	socks, conns, _ := fakeSOCKS5(t, mustURL(t, origin.URL))

	r := fetch.NewRodFetcher()
	r.Proxy = socks
	defer r.Close()
	resp, err := r.Fetch(t.Context(), fetch.FetchRequest{URL: origin.URL})
	if err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("rod fetch through request proxy: %v", err)
	}
	if !strings.Contains(string(resp.HTML), "via browser proxy") {
		t.Errorf("body = %q, want the origin response (routed through the request proxy)", resp.HTML)
	}
	if n := conns.Load(); n < 1 {
		t.Errorf("socks conns = %d, want >= 1 (--proxy-server must route through the request proxy)", n)
	}
}
