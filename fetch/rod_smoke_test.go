//go:build browser

package fetch_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/fetch"

	"github.com/go-rod/rod/lib/launcher"
)

// htmlPage serves html from a loopback origin: the browser path refuses
// data: URLs as the static path does (SSRF guard), and loopback rides the
// test-binary hatch.
func htmlPage(t *testing.T, html string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(html)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRodSmoke(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("no Chrome/Chromium found")
	}
	r := fetch.NewRodFetcher()
	defer r.Close()
	resp, err := r.Fetch(t.Context(), fetch.FetchRequest{URL: htmlPage(t, "<html><body><h1>hi</h1></body></html>")})
	if err != nil {
		t.Fatalf("rod fetch: %v", err)
	}
	if len(resp.HTML) == 0 {
		t.Fatal("empty html")
	}
}

// TestScreenshotPage (Phase G G.4, browser tier): the PNG is proven by
// magic bytes + IHDR dims parsed from bytes 16–24 — pixel comparisons
// are banned (they flake across Chrome versions).
func TestScreenshotPage(t *testing.T) {
	// launcher.LookPath only sees system Chrome; rod's managed download
	// (launcher.New) is the common CI/dev path — probe by launching and
	// skip only on launch failure (environment, not flake).
	page := htmlPage(t, `<html><head><title>shot</title></head><body style="margin:0"><div style="width:1200px;height:900px;background:#36c"></div></body></html>`)
	png, err := fetch.ScreenshotPage(t.Context(), page, 1280, 800)
	if err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("ScreenshotPage: %v", err)
	}
	if len(png) < 24 || png[0] != 0x89 || png[1] != 'P' || png[2] != 'N' || png[3] != 'G' {
		t.Fatalf("not a PNG (%d bytes)", len(png))
	}
	// IHDR: bytes 16–19 big-endian width, 20–23 height.
	w := int(png[16])<<24 | int(png[17])<<16 | int(png[18])<<8 | int(png[19])
	h := int(png[20])<<24 | int(png[21])<<16 | int(png[22])<<8 | int(png[23])
	if w < 100 || h < 100 {
		t.Errorf("IHDR dims %dx%d, want non-trivial full-page capture", w, h)
	}
	if w < 1200 {
		t.Errorf("IHDR width %d, want >= the 1200px content (full-page, not viewport-cropped)", w)
	}

	// Viewport: accepted without error (pixel-equality assertions banned).
	if _, err := fetch.ScreenshotPage(t.Context(), page, 640, 480); err != nil {
		t.Errorf("viewport capture: %v", err)
	}
}
