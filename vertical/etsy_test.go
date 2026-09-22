package vertical_test

import (
	"math"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestEtsyListingMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("etsy_listing")
	if !ok {
		t.Fatal("etsy_listing not registered")
	}
	yes := []string{
		"https://www.etsy.com/listing/1283746291/handmade-ceramic-mug",
		"https://etsy.com/listing/1283746291",
	}
	no := []string{
		"https://www.etsy.com/shop/ClayAndKilnStudio", // shop page, not a listing
		"https://www.etsy.com/listing/",               // no id
		"https://ebay.com/itm/123456",                 // wrong marketplace
		"https://notetsy.com/listing/1283746291/x",
		"file:///tmp/listing/1283746291",
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

func TestEtsyListingExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("etsy_listing")
	const page = "https://www.etsy.com/listing/1283746291/handmade-ceramic-mug"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"etsy.com/listing/": {body: verticalFixture(t, "etsy-listing.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Handmade Ceramic Mug, 12oz" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["listing_id"] != "1283746291" { // from the URL, never the DOM
		t.Errorf("listing_id = %v", rec["listing_id"])
	}
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-32) > 1e-9 {
		t.Errorf("price.amount = %v, want 32", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	if rec["seller"] != "ClayAndKilnStudio" {
		t.Errorf("seller = %v", rec["seller"])
	}
	if r, _ := rec["rating"].(float64); math.Abs(r-4.9) > 1e-9 {
		t.Errorf("rating = %#v, want 4.9", rec["rating"])
	}
	if rec["reviews"] != 214.0 {
		t.Errorf("reviews = %#v, want 214 (float64)", rec["reviews"])
	}
	if rec["availability"] != "InStock" {
		t.Errorf("availability = %v, want InStock", rec["availability"])
	}
	if rec["url"] != page {
		t.Errorf("url = %v, want page URL", rec["url"])
	}
}

func TestEtsyListingExtract_DOMFallback(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("etsy_listing")
	const page = "https://www.etsy.com/listing/1283746291/handmade-ceramic-mug"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"etsy.com/listing/": {body: verticalFixture(t, "etsy-listing-dom.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Handmade Ceramic Mug, 12oz" {
		t.Errorf("title = %v", rec["title"])
	}
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map from og:price metas", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-32) > 1e-9 {
		t.Errorf("price.amount = %v, want 32", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	if rec["listing_id"] != "1283746291" {
		t.Errorf("listing_id = %v", rec["listing_id"])
	}
	// The og-meta shape carries no seller/rating/reviews/availability —
	// those keys must be absent, never guessed.
	for _, k := range []string{"seller", "rating", "reviews", "availability"} {
		if _, ok := rec[k]; ok {
			t.Errorf("%s present in DOM-fallback record — omission broken", k)
		}
	}
}

func TestEtsyListingExtract_NoSource(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("etsy_listing")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"etsy.com/listing/": {body: []byte(`<html><body>Sorry, we can't find that listing.</body></html>`)},
	}}
	const page = "https://www.etsy.com/listing/9999999999/gone"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: etsy_listing:") ||
		!strings.Contains(err.Error(), page) {
		t.Fatalf("err = %v, want typed no-listing error naming the URL", err)
	}
}
