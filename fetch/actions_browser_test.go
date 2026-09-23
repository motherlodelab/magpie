//go:build browser

package fetch_test

// Browser-tier proofs for the actions executor (rod-only, Chrome needed):
// mid-flow screenshot bytes, post-screenshot action execution, the
// ScreenshotPage delegation smoke, and the rod lang extra-header.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/fetch"
)

// TestScreenshotActions_MidFlow: the screenshot verb writes a PNG with
// magic bytes, and a second action AFTER it still executes (capture is
// mid-flow, not terminal — proven by the throwing eval-js).
func TestScreenshotActions_MidFlow(t *testing.T) {
	page := `data:text/html,<html><head><title>shot</title></head><body style="margin:0"><div style="width:800px;height:600px;background:#36c">prose placeholder body text</div></body></html>`
	pngPath := filepath.Join(t.TempDir(), "mid.png")
	acts, err := fetch.ParseActions([]string{"screenshot " + pngPath, "eval-js () => { throw new Error('second action ran'); }"})
	if err != nil {
		t.Fatalf("ParseActions: %v", err)
	}
	_, err = fetch.ScreenshotActions(context.Background(), page, 0, 0, acts)
	if err == nil || !strings.Contains(err.Error(), "second action ran") {
		t.Fatalf("err = %v, want the post-screenshot action to have executed", err)
	}
	raw, rerr := os.ReadFile(pngPath)
	if rerr != nil {
		t.Fatalf("screenshot file missing: %v", rerr)
	}
	if len(raw) < 8 || raw[0] != 0x89 || raw[1] != 'P' || raw[2] != 'N' || raw[3] != 'G' {
		t.Fatalf("screenshot file not a PNG (%d bytes)", len(raw))
	}
}

// TestScreenshotActions_NilActsMatchesScreenshotPage: the delegation
// smoke — nil actions takes the exact ScreenshotPage path (both succeed,
// both produce PNG bytes).
func TestScreenshotActions_NilActsMatchesScreenshotPage(t *testing.T) {
	page := `data:text/html,<html><head><title>shot</title></head><body style="margin:0"><div style="width:800px;height:600px;background:#36c">prose placeholder body text</div></body></html>`
	pngA, err := fetch.ScreenshotActions(context.Background(), page, 640, 480, nil)
	if err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("ScreenshotActions(nil): %v", err)
	}
	pngB, err := fetch.ScreenshotPage(context.Background(), page, 640, 480)
	if err != nil {
		t.Fatalf("ScreenshotPage: %v", err)
	}
	if len(pngA) < 8 || pngA[0] != 0x89 || pngB[0] != 0x89 {
		t.Fatal("not PNG bytes")
	}
}

// TestRod_LangExtraHeader: the lang header arrives at the origin through
// the REAL browser document request. Assert ONLY the lang header — rod
// drops Profile headers today (pre-existing), never pinned here.
func TestRod_LangExtraHeader(t *testing.T) {
	var mu sync.Mutex
	var seenLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenLang = r.Header.Get("Accept-Language")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>lang</title></head><body><p>body text for the page.</p></body></html>")) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)

	r := fetch.NewRodFetcher()
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.FetchWithActions(ctx, fetch.FetchRequest{URL: srv.URL, Lang: "fr-CA,fr;q=0.9"}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("FetchWithActions: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenLang == "" {
		t.Fatal("origin never saw the document request")
	}
	if !strings.HasPrefix(seenLang, "fr-CA") {
		t.Errorf("Accept-Language = %q, want fr-CA,fr;q=0.9 through the browser", seenLang)
	}
}

// TestRod_RunHeadersReachBrowserPath: FetchRequest.Headers ride the
// BROWSER document request too (M0c) — the origin echoes every received
// header into the body, so one response proves the UA, the probe header,
// AND the lang pair all arrived (a second SetExtraHeaders call would
// have clobbered the lang — the merge is load-bearing).
func TestRod_RunHeadersReachBrowserPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		var b strings.Builder
		b.WriteString("<html><body>")
		b.WriteString("<p>ua=" + r.Header.Get("User-Agent") + "</p>")
		b.WriteString("<p>probe=" + r.Header.Get("X-Probe") + "</p>")
		b.WriteString("<p>lang=" + r.Header.Get("Accept-Language") + "</p>")
		b.WriteString("<p>body text padding so the page is not empty.</p>")
		b.WriteString("</body></html>")
		_, _ = w.Write([]byte(b.String())) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)

	r := fetch.NewRodFetcher()
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.FetchWithActions(ctx, fetch.FetchRequest{
		URL:     srv.URL,
		Lang:    "de-DE,de;q=0.9",
		Headers: []string{"User-Agent: m0c-test/1", "X-Probe: 1"},
	}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("FetchWithActions: %v", err)
	}
	body := string(res.HTML)
	for _, want := range []string{"m0c-test/1", "probe=1", "de-DE"} {
		if !strings.Contains(body, want) {
			t.Errorf("origin did not see %q; body: %s", want, body)
		}
	}
}
