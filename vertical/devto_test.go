package vertical_test

import (
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestDevToMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("dev_to_article")
	if !ok {
		t.Fatal("dev_to_article not registered")
	}
	yes := []string{
		"https://dev.to/alexdev/go-generics",
		"https://dev.to/a/b",
	}
	no := []string{
		"https://dev.to/t/golang",                  // tag listing (also 2-seg)
		"https://dev.to/alexdev/generics/comments", // 3 segs — comment subpath
		"https://dev.to/about",                     // 1 seg
		"https://notdev.to/a/b",                    // exact host only
		"file:///tmp/a/b",
	}
	for _, raw := range yes {
		if !ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = false, want true", raw)
		}
	}
	for _, raw := range no {
		if ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = true, want false", raw)
		}
	}
}

func TestDevToExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("dev_to_article")
	const page = "https://dev.to/alexdev/go-generics"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"dev.to/alexdev/": {body: verticalFixture(t, "dev-to-article.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Go generics in the wild" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["author"] != "Alex Devlin" {
		t.Errorf("author = %v", rec["author"])
	}
	if rec["published"] != "2026-07-02T10:30:00Z" { // time[datetime], raw
		t.Errorf("published = %v", rec["published"])
	}
	tags, ok := rec["tags"].([]string)
	if !ok || len(tags) != 3 || tags[0] != "go" || tags[1] != "generics" || tags[2] != "tutorial" {
		t.Errorf("tags = %#v, want [go generics tutorial] in order", rec["tags"])
	}
	if rec["reactions"] != 128.0 {
		t.Errorf("reactions = %#v, want 128 (float64)", rec["reactions"])
	}
	if rec["comments"] != 12.0 {
		t.Errorf("comments = %#v, want 12 (float64)", rec["comments"])
	}
	if rec["description"] != "Writing scrapers in Go teaches you things." {
		t.Errorf("description = %v", rec["description"])
	}
	if rec["url"] != page {
		t.Errorf("url = %v, want page URL", rec["url"])
	}
}

// A page missing the optional blocks (no tags, no reaction counts, no
// comments header) yields a smaller record — keys absent, never guessed.
func TestDevToExtract_MissingFields(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("dev_to_article")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"dev.to/min/": {body: []byte(`<html><body>
<h1 class="crayons-title">Bare article</h1>
<div class="crayons-article__body"><p>Only a lede.</p></div>
</body></html>`)},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://dev.to/min/bare"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Bare article" || rec["description"] != "Only a lede." {
		t.Errorf("rec = %#v, want title+description", rec)
	}
	for _, k := range []string{"author", "published", "tags", "reactions", "comments"} {
		if _, ok := rec[k]; ok {
			t.Errorf("%s present on a source without it — omission broken", k)
		}
	}
}

func TestDevToExtract_NoSource(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("dev_to_article")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"dev.to/gone/": {body: []byte(`<html><body></body></html>`)},
	}}
	const page = "https://dev.to/gone/deleted"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: dev_to_article:") ||
		!strings.Contains(err.Error(), page) {
		t.Fatalf("err = %v, want typed no-article error naming the URL", err)
	}
}
