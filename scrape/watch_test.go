package scrape_test

// Package scrape_test (NOT the plan's `package scrape`): openScrapeDB/
// fakeDeps/fakeExtractor live in this external test package, and every
// CheckForChange surface it needs is exported — so the plan's helper-
// reuse intent wins over its package label (plan gap resolved in PR notes).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/motherlodelab/magpie/store"

	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
)

// webhookSink records POST bodies; the answer status is atomically
// flippable (200 delivery rows, 500 failure rows).
type webhookSink struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
	status int32
}

func newWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	ws := &webhookSink{}
	ws.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) //nolint:errcheck // test sink
		ws.mu.Lock()
		ws.bodies = append(ws.bodies, string(b))
		ws.mu.Unlock()
		code := int(atomic.LoadInt32(&ws.status))
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(ws.srv.Close)
	return ws
}

func (ws *webhookSink) posts() []string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return append([]string(nil), ws.bodies...)
}

// watchFetcher is a fake vertical.Fetcher for CheckForChange: a mutable
// body with a hit counter. Flip body between checks to drive the
// baseline → changed sequence without a second origin or any HTTP.
type watchFetcher struct {
	mu   sync.Mutex
	body string
	hits int
}

func (w *watchFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hits++
	return &fetch.FetchResponse{URL: req.URL, FinalURL: req.URL, StatusCode: 200,
		HTML: []byte(w.body), Headers: http.Header{}}, nil
}

func (w *watchFetcher) set(body string)                   { w.mu.Lock(); w.body = body; w.mu.Unlock() }
func (w *watchFetcher) CanHandle(fetch.FetchRequest) bool { return true }
func (w *watchFetcher) Close() error                      { return nil }

// pricePage builds a quality-gate-clean watch fixture whose ONLY delta
// is the price token — the diff stays tight.
func pricePage(price string) string {
	return "<html><head><title>Price Watch</title></head><body><p>The price is " + price +
		" dollars today. " + strings.Repeat("Honest filler prose keeps the gate satisfied. ", 8) + "</p></body></html>"
}

// TestWatch_TooLargeStillBaselines — QA W1: a change wider than the diff
// window is still a change. It used to error before PutSnapshot, so the
// old baseline stuck and every later check failed the same way.
func TestWatch_TooLargeStillBaselines(t *testing.T) {
	page := func(prefix string) string {
		words := make([]string, 2100)
		for i := range words {
			words[i] = fmt.Sprintf("%s%d", prefix, i)
		}
		return "<html><head><title>Big Watch</title></head><body><p>" + strings.Join(words, " ") + "</p></body></html>"
	}
	db := openScrapeDB(t)
	wf := &watchFetcher{body: page("a")}
	deps := scrape.Deps{DB: db, Fetcher: wf}
	url := "https://shop.example.com/big"
	check := func() scrape.WatchResult {
		t.Helper()
		res, err := scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static"})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		return res
	}
	check() // baseline
	wf.set(page("b"))
	if res := check(); !res.Changed || !strings.Contains(res.Diff, "too large") {
		t.Errorf("too-large change = changed %v, diff %q; want changed with a too-large note", res.Changed, res.Diff)
	}
	if n, err := db.TableCount("snapshots"); err != nil || n != 2 {
		t.Errorf("snapshot rows = %d (%v), want 2: the change must become the baseline", n, err)
	}
	if res := check(); res.Changed {
		t.Errorf("same body after a too-large change reported changed: the baseline did not move")
	}
}

func TestWatch_BaselineThenChange(t *testing.T) {
	db := openScrapeDB(t)
	wf := &watchFetcher{body: pricePage("10")}
	sink := newWebhookSink(t)
	deps := scrape.Deps{DB: db, Fetcher: wf}
	url := "https://shop.example.com/p/1"

	res, err := scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static", Webhook: sink.srv.URL})
	if err != nil {
		t.Fatalf("baseline check: %v", err)
	}
	if res.Changed || res.Diff != "" || res.OldHash != "" {
		t.Errorf("baseline = %+v, want changed=false, empty diff, no old hash", res)
	}
	if res.WebhookStatus != "" {
		t.Errorf("WebhookStatus = %q, want empty on baseline", res.WebhookStatus)
	}
	if n := len(sink.posts()); n != 0 {
		t.Fatalf("no-change check fired %d webhook POSTs, want 0", n)
	}
	if n, err := db.TableCount("snapshots"); err != nil || n != 1 {
		t.Errorf("snapshot rows = %d (%v), want 1 after baseline", n, err)
	}

	wf.set(pricePage("20")) // the reprice
	res, err = scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static", Webhook: sink.srv.URL})
	if err != nil {
		t.Fatalf("change check: %v", err)
	}
	if !res.Changed || res.Diff == "" || res.OldHash == "" || res.OldHash == res.NewHash {
		t.Fatalf("change not reported: %+v", res)
	}
	if !strings.Contains(res.Diff, "+ 20") {
		t.Errorf("diff missing the +20 token:\n%s", res.Diff)
	}
	posts := sink.posts()
	if len(posts) != 1 {
		t.Fatalf("want exactly 1 webhook POST, got %d", len(posts))
	}
	var payload struct {
		URL     string `json:"url"`
		Changed bool   `json:"changed"`
		OldHash string `json:"old_hash"`
		NewHash string `json:"new_hash"`
		Diff    string `json:"diff"`
	}
	if err := json.Unmarshal([]byte(posts[0]), &payload); err != nil {
		t.Fatalf("webhook payload not the contracted JSON: %v", err)
	}
	if payload.URL != url || !payload.Changed || payload.OldHash != res.OldHash ||
		payload.NewHash != res.NewHash || payload.Diff != res.Diff {
		t.Errorf("payload mismatch: %+v vs result %+v", payload, res)
	}
	if res.WebhookStatus != "sent" {
		t.Errorf("WebhookStatus = %q, want sent", res.WebhookStatus)
	}
	// CheckedAt is the stored key (PutSnapshot's return), not a second clock read.
	if latest, ok, err := db.LatestSnapshot(url); err != nil || !ok || !res.CheckedAt.Equal(latest.CheckedAt) {
		t.Errorf("CheckedAt = %v, want the stored checked_at %v (%v, %v)", res.CheckedAt, latest.CheckedAt, ok, err)
	}
	// Snapshot rows == check count (2), including same-second inserts.
	if n, err := db.TableCount("snapshots"); err != nil || n != 2 {
		t.Errorf("snapshot rows = %d (%v), want 2", n, err)
	}

	// QA §3 (K3): an edit that only re-spaces a <pre> hashes differently but
	// words-diffs to nothing — not a change (no empty-diff notification),
	// yet the new snapshot is still the baseline. The one-word reprice
	// above is the positive control.
	spaced := func(gap string) string {
		return strings.Replace(pricePage("20"), "</body>", "<pre>sku"+gap+"A-1</pre></body>", 1)
	}
	wf.set(spaced(" "))
	if res, err = scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static", Webhook: sink.srv.URL}); err != nil || !res.Changed {
		t.Fatalf("adding the <pre> = %+v (%v), want a change", res, err)
	}
	wf.set(spaced("      "))
	res, err = scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static", Webhook: sink.srv.URL})
	if err != nil {
		t.Fatalf("whitespace check: %v", err)
	}
	if res.OldHash == res.NewHash {
		t.Fatalf("vacuous: the spacing edit didn't reach the markdown (hash %s both times)", res.NewHash)
	}
	if res.Changed || res.Diff != "" || res.WebhookStatus != "" {
		t.Errorf("whitespace-only edit = changed %v, diff %q, webhook %q; want no change", res.Changed, res.Diff, res.WebhookStatus)
	}
	if n := len(sink.posts()); n != 2 {
		t.Errorf("webhook POSTs = %d, want 2 (the reprice and the <pre>, not the re-spacing)", n)
	}
	if latest, ok, err := db.LatestWatchSnapshot(url); err != nil || !ok || latest.ContentHash != res.NewHash {
		t.Errorf("latest snapshot hash = %s (%v, %v), want the re-spaced page %s as the baseline", latest.ContentHash, ok, err, res.NewHash)
	}
}

// TestWatch_WebhookFailureNotFatal: a dead or 500ing sink never fails
// the check — the snapshot is already stored and cron re-runs are the retry.
func TestWatch_WebhookFailureNotFatal(t *testing.T) {
	// baseline first (no webhook on no-change), then the reprice fires
	// the webhook at a broken sink — the check must still succeed.
	runChecks := func(t *testing.T, sinkURL string, db *store.DB, wf *watchFetcher) scrape.WatchResult {
		t.Helper()
		deps := scrape.Deps{DB: db, Fetcher: wf}
		res, err := scrape.CheckForChange(context.Background(), deps, "https://shop.example.com/x", scrape.Options{Render: "static", Webhook: sinkURL})
		if err != nil || res.Changed {
			t.Fatalf("baseline = (%+v, %v), want clean no-change", res, err)
		}
		wf.set(pricePage("20"))
		res, err = scrape.CheckForChange(context.Background(), deps, "https://shop.example.com/x", scrape.Options{Render: "static", Webhook: sinkURL})
		if err != nil {
			t.Fatalf("check failed on broken sink: %v", err)
		}
		if !res.Changed {
			t.Fatalf("res = %+v, want changed", res)
		}
		return res
	}

	t.Run("sink 500", func(t *testing.T) {
		db := openScrapeDB(t)
		wf := &watchFetcher{body: pricePage("10")}
		sink := newWebhookSink(t)
		atomic.StoreInt32(&sink.status, 500)
		res := runChecks(t, sink.srv.URL, db, wf)
		if !strings.HasPrefix(res.WebhookStatus, "failed") {
			t.Errorf("WebhookStatus = %q, want failed prefix", res.WebhookStatus)
		}
		if n, err := db.TableCount("snapshots"); err != nil || n != 2 {
			t.Errorf("snapshot rows = %d (%v), want 2 (stored despite sink failure)", n, err)
		}
	})
	t.Run("sink down", func(t *testing.T) {
		db := openScrapeDB(t)
		wf := &watchFetcher{body: pricePage("10")}
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close() // closed port: connection refused, zero external dials
		res := runChecks(t, url, db, wf)
		if !strings.HasPrefix(res.WebhookStatus, "failed") {
			t.Errorf("WebhookStatus = %q, want failed prefix", res.WebhookStatus)
		}
	})
}

// TestWatch_ZeroLLM: CheckForChange clears a caller-supplied schema —
// markdown-only is enforced, not hoped for.
func TestWatch_ZeroLLM(t *testing.T) {
	db := openScrapeDB(t)
	fx := &fakeExtractor{script: map[string]any{"title": "x"}}
	deps := fakeDeps(db, fx, "test-key")
	deps.Fetcher = &watchFetcher{body: pricePage("10")}
	if _, err := scrape.CheckForChange(context.Background(), deps, "https://shop.example.com/z",
		scrape.Options{Render: "static", Schema: testSchema(t)}); err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := fx.total(); got != 0 {
		t.Errorf("extractor calls = %d, want 0 (schema cleared)", got)
	}
}

// TestWatch_WebhookScope — H.7 gate test 1. Two proofs in one name:
// (a) the webhook client's AllowPrivate reaches a loopback sink;
// (b) the scrape path keeps the default guard even inside the SAME
// CheckForChange that carries the AllowPrivate webhook client.
//
// Under plain `go test` the default fetcher relaxes AllowPrivate
// (fetch/ssrf.go), so the rejection half MUST pin MAGPIE_STRICT_SSRF=1
// or it fails backwards — the loopback target would pass the guard and
// the test would "fail" for the opposite reason it expects. Serial: no
// t.Parallel anywhere in this file.
func TestWatch_WebhookScope(t *testing.T) {
	t.Run("webhook reaches loopback sink", func(t *testing.T) {
		db := openScrapeDB(t)
		wf := &watchFetcher{body: pricePage("10")}
		sink := newWebhookSink(t)
		deps := scrape.Deps{DB: db, Fetcher: wf}
		res, err := scrape.CheckForChange(context.Background(), deps,
			"https://shop.example.com/p/1", scrape.Options{Render: "static", Webhook: sink.srv.URL})
		if err != nil {
			t.Fatalf("baseline check: %v", err)
		}
		if res.Changed {
			t.Fatal("baseline must not report change")
		}
		if n := len(sink.posts()); n != 0 {
			t.Fatalf("no-change check must not fire the webhook, got %d POSTs", n)
		}

		wf.set(pricePage("20")) // the reprice
		res, err = scrape.CheckForChange(context.Background(), deps,
			"https://shop.example.com/p/1", scrape.Options{Render: "static", Webhook: sink.srv.URL})
		if err != nil {
			t.Fatalf("change check: %v", err)
		}
		if !res.Changed || res.Diff == "" || res.OldHash == res.NewHash {
			t.Fatalf("change not reported: changed=%v diff=%q", res.Changed, res.Diff)
		}
		posts := sink.posts()
		if len(posts) != 1 {
			t.Fatalf("want exactly 1 webhook POST, got %d", len(posts))
		}
		var payload struct {
			URL     string `json:"url"`
			Changed bool   `json:"changed"`
			OldHash string `json:"old_hash"`
			NewHash string `json:"new_hash"`
			Diff    string `json:"diff"`
		}
		if err := json.Unmarshal([]byte(posts[0]), &payload); err != nil {
			t.Fatalf("webhook payload not the contracted JSON: %v", err)
		}
		if payload.URL != res.URL || !payload.Changed ||
			payload.OldHash != res.OldHash || payload.NewHash != res.NewHash {
			t.Errorf("payload mismatch: %+v vs result %+v", payload, res)
		}
		if res.WebhookStatus != "sent" {
			t.Errorf("WebhookStatus = %q, want sent", res.WebhookStatus)
		}
	})

	t.Run("scrape path keeps default guard while webhook stays loopback-capable", func(t *testing.T) {
		t.Setenv("MAGPIE_STRICT_SSRF", "1") // serial test: no t.Parallel above
		db := openScrapeDB(t)
		sink := newWebhookSink(t)

		// The REAL fetcher — constructed AFTER Setenv so isTestBinary()
		// sees the strict pin. A watchFetcher here would bypass the
		// guard and make this subtest vacuous.
		static, err := fetch.NewStaticFetcher()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cerr := static.Close(); cerr != nil {
				t.Errorf("close: %v", cerr)
			}
		})
		deps := scrape.Deps{DB: db, Fetcher: static}
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<p>hi</p>")) //nolint:errcheck // test server
		}))
		defer origin.Close()

		_, err = scrape.CheckForChange(context.Background(), deps, origin.URL, scrape.Options{Render: "static", Webhook: sink.srv.URL})
		if err == nil {
			t.Fatal("loopback scrape target must be rejected under MAGPIE_STRICT_SSRF — AllowPrivate leaked into the scrape path")
		}
		if !errors.Is(err, fetch.ErrPrivateAddress) {
			t.Errorf("rejection should wrap fetch.ErrPrivateAddress, got: %v", err)
		}
		if n := len(sink.posts()); n != 0 {
			t.Errorf("failed scrape must not fire the webhook, got %d POSTs", n)
		}
	})
}

// TestWatch_ChangeMetrics pins the per-change metrics (desktop D10): zero
// on the baseline and on an unchanged re-check; on a change, the net word
// delta and the link targets gained/lost — sorted, so two of each are
// inserted out of order.
func TestWatch_ChangeMetrics(t *testing.T) {
	db := openScrapeDB(t)
	linkPage := func(extra string, links ...string) string { // pricePage owns the gate padding
		var a strings.Builder
		for _, l := range links {
			a.WriteString(` <a href="` + l + `">link</a>`)
		}
		return pricePage("10" + extra + a.String())
	}
	wf := &watchFetcher{body: linkPage("", "/keep", "/old", "/b-old")}
	deps := scrape.Deps{DB: db, Fetcher: wf}
	url := "https://shop.example.com/p/1"
	check := func() scrape.WatchResult {
		t.Helper()
		res, err := scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	zero := func(stage string, r scrape.WatchResult) {
		t.Helper()
		if r.WordsDelta != 0 || len(r.LinksAdded) != 0 || len(r.LinksRemoved) != 0 {
			t.Errorf("%s metrics = %d %q %q, want all zero", stage, r.WordsDelta, r.LinksAdded, r.LinksRemoved)
		}
	}

	zero("baseline", check())

	wf.set(linkPage(" Three more words.", "/keep", "/new", "/a-new"))
	res := check()
	if !res.Changed {
		t.Fatalf("change not reported: %+v", res)
	}
	if res.WordsDelta != 3 {
		t.Errorf("WordsDelta = %d, want 3", res.WordsDelta)
	}
	const base = "https://shop.example.com"
	if got, want := strings.Join(res.LinksAdded, " "), base+"/a-new "+base+"/new"; got != want {
		t.Errorf("LinksAdded = %q, want %q (sorted)", got, want)
	}
	if got, want := strings.Join(res.LinksRemoved, " "), base+"/b-old "+base+"/old"; got != want {
		t.Errorf("LinksRemoved = %q, want %q (sorted)", got, want)
	}

	zero("unchanged re-check", check())
}

// TestCheckForChange_IgnoresReads: a scrape or research read of a watched
// URL between two checks (RecordSnapshot) must not become watch's baseline —
// the next check still reports the change and diffs against its own last
// check (desktop DR1 correction 10).
func TestCheckForChange_IgnoresReads(t *testing.T) {
	db := openScrapeDB(t)
	wf := &watchFetcher{body: pricePage("10")}
	deps := scrape.Deps{DB: db, Fetcher: wf}
	url := "https://shop.example.com/p/read"
	if _, err := scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static"}); err != nil {
		t.Fatalf("baseline check: %v", err)
	}
	wf.set(pricePage("20"))
	read, err := scrape.Run(context.Background(), deps, url, scrape.Options{Render: "static"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := db.RecordSnapshot(url, read.Markdown); err != nil {
		t.Fatalf("RecordSnapshot: %v", err)
	}
	res, err := scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static"})
	if err != nil {
		t.Fatalf("check after read: %v", err)
	}
	if !res.Changed || !strings.Contains(res.Diff, "+ 20") {
		t.Errorf("check after a read = changed %v, diff %q; want the 10 → 20 change", res.Changed, res.Diff)
	}
}

// TestCheckForChange_RowIsWatch — QA ST8: a check records its run row as
// command "watch" (fetch telemetry stays), so no run list counts it; a
// scrape on the same deps is still a listed "scrape" run.
func TestCheckForChange_RowIsWatch(t *testing.T) {
	db := openScrapeDB(t)
	deps := scrape.Deps{DB: db, Fetcher: &watchFetcher{body: pricePage("10")}}
	url := "https://shop.example.com/p/kind"
	if _, err := scrape.CheckForChange(context.Background(), deps, url, scrape.Options{Render: "static"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := scrape.Run(context.Background(), deps, url, scrape.Options{Render: "static"}); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	n, err := db.TableCount("run_history")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListRuns(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(listed) != 1 || listed[0].Command != "scrape" {
		t.Errorf("rows = %d, listed = %+v; want 2 rows, only the scrape listed", n, listed)
	}
}
