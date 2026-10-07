//go:build browser

package fetch_test

// M0b browser-tier proof: run cookies ride the REAL browser document
// request (static-path tests are the regression net and stay untouched).
// The parser pins live in cookies_internal_test.go.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/fetch"
)

func TestRod_CookieInjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// The body echoes what the ORIGIN saw on the document request —
		// Chromium's jar semantics, not our parser, are under test here.
		fmt.Fprintf(w, "<html><head><title>ck</title></head><body><p>CK=%s</p></body></html>", r.Header.Get("Cookie")) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)

	r := fetch.NewRodFetcher()
	defer r.Close()
	do := func(url, cookies string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := r.FetchWithActions(ctx, fetch.FetchRequest{URL: url, Cookies: cookies}, nil)
		if err != nil {
			if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
				t.Skipf("no browser available: %v", err)
			}
			t.Fatalf("FetchWithActions: %v", err)
		}
		return string(resp.HTML)
	}

	if body := do(srv.URL, "k=v"); !strings.Contains(body, "k=v") {
		t.Errorf("body %q missing k=v — cookie did not ride the document request", body)
	}

	// Host scoping, same jar: the cookie was set for 127.0.0.1; the same
	// handler on 127.0.0.2 is a DIFFERENT cookie domain — the jar must not
	// leak it across hosts. (Not the localhost name: the SSRF guard refuses
	// it in every mode, as the static path does.)
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1") {
		t.Skipf("origin host is %s, scoping probe needs 127.0.0.1", srv.URL)
	}
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("no 127.0.0.2 loopback address: %v", err)
	}
	other := httptest.NewUnstartedServer(srv.Config.Handler)
	_ = other.Listener.Close() //nolint:errcheck // replaced before Start
	other.Listener = l
	other.Start()
	t.Cleanup(other.Close)
	body := do(other.URL, "")
	if !strings.Contains(body, "CK=") {
		t.Fatalf("second origin never answered (%q) — the scoping probe proves nothing", body)
	}
	if strings.Contains(body, "k=v") {
		t.Errorf("127.0.0.1-scoped cookie leaked to the 127.0.0.2 domain: %q", body)
	}

	// Fresh browser, fresh jar: a cookie-less run on a new RodFetcher
	// (the scrape pattern — fetchBrowser builds one per run) starts
	// clean; no cross-session leak of the injected cookie.
	r2 := fetch.NewRodFetcher()
	defer r2.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := r2.FetchWithActions(ctx, fetch.FetchRequest{URL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("FetchWithActions: %v", err)
	}
	if strings.Contains(string(resp.HTML), "CK=k=v") {
		t.Errorf("injected cookie leaked into a fresh browser session: %q", resp.HTML)
	}
}
