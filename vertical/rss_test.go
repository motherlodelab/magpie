package vertical_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestRssExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("rss")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"blog.example": {body: verticalFixture(t, "feed-rss.xml")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://blog.example/feed.xml"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Example Blog" || rec["link"] != "https://blog.example/" || rec["description"] != "Mostly scrapers" {
		t.Errorf("channel = %v/%v/%v", rec["title"], rec["link"], rec["description"])
	}
	items, ok := rec["items"].([]any)
	if !ok || len(items) != 4 {
		t.Fatalf("items = %#v, want 4", rec["items"])
	}
	first, _ := items[0].(map[string]any)
	// published is the raw pubDate string — passthrough, no time parsing.
	want := map[string]any{
		"title": "RSS item one", "link": "https://blog.example/one",
		"published": "Mon, 14 Sep 2026 08:00:00 GMT", "summary": "First RSS entry",
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("item[0] = %#v, want %#v", first, want)
	}
}

func TestRssAtomExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("rss")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"blog.example": {body: verticalFixture(t, "feed-atom.xml")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://blog.example/feed.xml"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["link"] != "https://blog.example/" { // rel="alternate" wins; rel="self" excluded
		t.Errorf("feed link = %v, want alternate href", rec["link"])
	}
	if rec["description"] != "Mostly scrapers" {
		t.Errorf("description = %v, want subtitle", rec["description"])
	}
	items, _ := rec["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	get := func(i int) map[string]any { m, _ := items[i].(map[string]any); return m }
	// entry1: updated-only ⇒ published falls back to updated.
	if get(0)["published"] != "2026-09-18T08:00:00Z" {
		t.Errorf("entry1 published = %v, want updated fallback", get(0)["published"])
	}
	// entry2: published+updated ⇒ its own published wins.
	if get(1)["published"] != "2026-09-17T08:00:00Z" {
		t.Errorf("entry2 published = %v, want own published", get(1)["published"])
	}
	// entry3: link with no rel ⇒ href used.
	if get(2)["link"] != "https://blog.example/third" {
		t.Errorf("entry3 link = %v, want no-rel href", get(2)["link"])
	}
}

func TestRssCap(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("rss")
	var b strings.Builder
	b.WriteString("<rss><channel><title>Big</title>")
	for i := 0; i < 55; i++ {
		fmt.Fprintf(&b, "<item><title>item-%d</title><link>https://blog.example/%d</link></item>", i, i)
	}
	b.WriteString("</channel></rss>")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"blog.example": {body: []byte(b.String())},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://blog.example/feed.xml"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	items, _ := rec["items"].([]any)
	if len(items) != 50 {
		t.Fatalf("items = %d, want capped 50", len(items))
	}
	first, _ := items[0].(map[string]any)
	last, _ := items[49].(map[string]any)
	if first["title"] != "item-0" || last["title"] != "item-49" {
		t.Errorf("cap kept %v..%v, want first 50 in document order", first["title"], last["title"])
	}
}

func TestRssEmptyFeed_ItemsNotNil(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("rss")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"blog.example": {body: []byte(`<rss><channel><title>t</title></channel></rss>`)},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://blog.example/feed"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	items, ok := rec["items"].([]any)
	if !ok || items == nil || len(items) != 0 {
		t.Errorf("items = %#v (ok=%v), want non-nil empty slice", rec["items"], ok)
	}
}

func TestRssNonFeedError(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("rss")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"blog.example": {body: []byte(`<html><head><title>Blog</title></head><body>not a feed</body></html>`)},
	}}
	const page = "https://blog.example/feed"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: rss:") || !strings.Contains(err.Error(), page) {
		t.Fatalf("err = %v, want typed error naming the URL", err)
	}
}

func TestRssMatch_Table(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("rss")
	yes := []string{
		"https://blog.example/feed", "https://blog.example/rss",
		"https://blog.example/x.xml", "https://blog.example/y.atom",
		"https://blog.example/feed.xml",
	}
	no := []string{"https://blog.example/post", "ftp://blog.example/feed", "file:///feed.xml"}
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
