//go:build live

package vertical_test

import (
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

// Live captures of real pages, committed under testdata/vertical/. Still
// hermetic — the fake fetcher serves the committed bytes; only the tag
// quarantines recapture churn. Loose shape asserts only: captures drift
// on recapture by design, so exact values are never pinned here.

func TestLiveJobPosting(t *testing.T) {
	ex, _ := vertical.Lookup("job_posting")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"jobs.example": {body: verticalFixture(t, "job-posting-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://jobs.example/x"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] == "" {
		t.Errorf("title = %v, want non-empty", rec["title"])
	}
}

func TestLiveEvent(t *testing.T) {
	ex, _ := vertical.Lookup("event")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"events.example": {body: verticalFixture(t, "event-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://events.example/x"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["name"] == "" {
		t.Errorf("name = %v, want non-empty", rec["name"])
	}
}

func TestLiveLocalBusiness(t *testing.T) {
	ex, _ := vertical.Lookup("local_business")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"biz.example": {body: verticalFixture(t, "local-business-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://biz.example/"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["name"] == "" {
		t.Errorf("name = %v, want non-empty", rec["name"])
	}
}

func TestLiveArticle(t *testing.T) {
	ex, _ := vertical.Lookup("article")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"news.example": {body: verticalFixture(t, "article-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://news.example/x"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["headline"] == "" {
		t.Errorf("headline = %v, want non-empty", rec["headline"])
	}
}

func TestLiveRss(t *testing.T) {
	ex, _ := vertical.Lookup("rss")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"blog.example": {body: verticalFixture(t, "rss-live.xml")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://blog.example/feed.xml"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	items, _ := rec["items"].([]any)
	if len(items) < 1 || len(items) > 50 {
		t.Errorf("items = %d, want in [1,50]", len(items))
	}
}

// Phase W captures. Same loose contract: non-empty primary keys only —
// marketplace pages drift per recapture and get A/B-instrumented, so no
// exact values are ever pinned here.
// etsy_listing has NO live test: every capture attempt hit a DataDome
// challenge (static + browser, settle waits included). Hand fixtures pin
// the contract; recapture when a proxy tier exists. PR note required.

func TestLiveWooCommerce(t *testing.T) {
	ex, _ := vertical.Lookup("woocommerce_product")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"shop.example/product/": {body: verticalFixture(t, "woo-product-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://shop.example/product/capture"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] == "" || rec["url"] == "" {
		t.Errorf("title/url = %v/%v, want non-empty", rec["title"], rec["url"])
	}
}

func TestLiveSubstack(t *testing.T) {
	ex, _ := vertical.Lookup("substack_post")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"demo.substack.com/api/v1/posts/": {body: verticalFixture(t, "substack-post-live.json")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://demo.substack.com/p/capture"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] == "" || rec["text"] == "" {
		t.Errorf("title/text = %v/%v, want non-empty", rec["title"], rec["text"])
	}
}

func TestLiveDevTo(t *testing.T) {
	ex, _ := vertical.Lookup("dev_to_article")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"dev.to/": {body: verticalFixture(t, "dev-to-article-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://dev.to/user/capture"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] == "" || rec["url"] == "" {
		t.Errorf("title/url = %v/%v, want non-empty", rec["title"], rec["url"])
	}
}

func TestLiveEbayItem(t *testing.T) {
	ex, _ := vertical.Lookup("ebay_item")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"ebay.com/itm/": {body: verticalFixture(t, "ebay-item-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.ebay.com/itm/1234567890"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] == "" || rec["url"] == "" {
		t.Errorf("title/url = %v/%v, want non-empty", rec["title"], rec["url"])
	}
}

func TestLiveAmazonProduct(t *testing.T) {
	ex, _ := vertical.Lookup("amazon_product")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"amazon.com/dp/": {body: verticalFixture(t, "amazon-product-live.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.amazon.com/dp/B0CAPTURE"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] == "" || rec["url"] == "" {
		t.Errorf("title/url = %v/%v, want non-empty", rec["title"], rec["url"])
	}
}
