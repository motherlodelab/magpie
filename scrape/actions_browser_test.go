//go:build browser

package scrape_test

// Browser-tier e2e for the actions DSL: an httptest origin whose script
// appends 10 rows per #load-more click. Driving scrape.Run (the exported
// seam — fetchWithActions is unexported by design): the BEFORE/AFTER pair
// proves causality — row 30 exists in the markdown only when actions ran.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/scrape"
)

func loadMoreOrigin(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		list := ""
		for i := 1; i <= 10; i++ {
			list += fmt.Sprintf(`<li class="row">Item %d carries distinctive extraction prose for the pipeline.</li>`, i)
		}
		page := `<!doctype html><html><head><title>Load More</title></head><body>
			<h1>Load More Fixture</h1>
			<ul id="list">` + list + `</ul>
			<button id="load-more" onclick="
				var ul = document.getElementById('list');
				var base = ul.children.length;
				for (var i = 1; i <= 20; i++) {
					var li = document.createElement('li');
					li.className = 'row';
					li.textContent = 'Item ' + (base + i) + ' carries distinctive extraction prose for the pipeline.';
					ul.appendChild(li);
				}
			">Load more</button>
			<p>Closing prose paragraph keeps the page structure honest for extraction.</p>
			</body></html>`
		_, _ = w.Write([]byte(page)) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestScrape_ActionsLoadMore(t *testing.T) {
	db := openScrapeDB(t)
	deps := scrape.Deps{DB: db}
	url := loadMoreOrigin(t)
	// Options.Actions is the raw line list — scrape validates and parses.
	acts := []string{"click #load-more", "wait-for .row:nth-of-type(30)"}

	// WITH actions: the browser clicks, rows 11–30 render, row 30 lands.
	res, err := scrape.Run(context.Background(), deps, url, scrape.Options{Render: "auto", Actions: acts})
	if err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("Run with actions: %v", err)
	}
	if !strings.Contains(res.Markdown, "Item 30") {
		t.Errorf("actions markdown missing row 30:\n%s", res.Markdown)
	}

	// WITHOUT actions: only the 10 server-rendered rows exist — row 30
	// was never served (the causality half of the proof).
	res2, err := scrape.Run(context.Background(), deps, url, scrape.Options{Render: "static"})
	if err != nil {
		t.Fatalf("Run without actions: %v", err)
	}
	if strings.Contains(res2.Markdown, "Item 30") {
		t.Errorf("static markdown has row 30 — fixture leaked:\n%s", res2.Markdown)
	}
	if !strings.Contains(res2.Markdown, "Item 10") {
		t.Errorf("static markdown lost the server-rendered rows:\n%s", res2.Markdown)
	}
}

// TestScrape_BrowserPathCarriesHeadersAndCookies (M0c2): fetchBrowser
// used to rebuild a partial FetchRequest (URL/Lang/CaptureXHR only),
// silently dropping Headers and Cookies on every browser fetch — the
// actions/escalation path re-emulated the device UA and started logged
// out while the static path carried both. The echo origin proves both
// fields reach the browser request through the exported scrape.Run seam.
func TestScrape_BrowserPathCarriesHeadersAndCookies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(fmt.Sprintf(`<!doctype html><html><head><title>echo</title></head><body>
<p id="ua">UA: %s</p><p id="sid">SID: %s</p>
<p>%s</p></body></html>`, r.UserAgent(), r.URL.Query().Get("sid"), strings.Repeat("padding prose for the quality gate ", 20)))) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)

	deps := scrape.Deps{DB: openScrapeDB(t)}
	res, err := scrape.Run(context.Background(), deps, srv.URL+"/echo?sid=jar-sid-42", scrape.Options{
		Render:  "auto",
		Headers: []string{"User-Agent: m0c2-test/1", "X-Probe: yes"},
		Cookies: "sid=jar-sid-42",
		Actions: []string{"wait 1"}, // actions force the fetchBrowser path
	})
	if err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("scrape.Run: %v", err)
	}
	if !strings.Contains(res.Markdown, "m0c2-test/1") {
		t.Errorf("browser path dropped the User-Agent header: %s", res.Markdown[:min(len(res.Markdown), 300)])
	}
	if !strings.Contains(res.Markdown, "jar-sid-42") {
		t.Errorf("browser path dropped the cookie jar: %s", res.Markdown[:min(len(res.Markdown), 300)])
	}
}
