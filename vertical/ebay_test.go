package vertical_test

import (
	"math"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestEbayItemMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("ebay_item")
	if !ok {
		t.Fatal("ebay_item not registered")
	}
	yes := []string{
		"https://www.ebay.com/itm/1234567890",
		"https://ebay.com/itm/1234567890?nordt=true",
		"https://www.ebay.com/itm/1234567890-vintage-camera",
	}
	no := []string{
		"https://www.ebay.com/itm/",               // no id
		"https://www.ebay.com/itm/notanumber",     // digits-only id guard
		"https://www.ebay.com/b/Photo-Video/bn_1", // category browse
		"https://etsy.com/listing/1234567890",     // wrong marketplace
		"https://www.ebay.de/itm/1234567890",      // regional TLD: ponytail ceiling
		"file:///tmp/itm/123",
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

func TestEbayItemExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("ebay_item")
	const page = "https://www.ebay.com/itm/1234567890"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"ebay.com/itm/": {body: verticalFixture(t, "ebay-item.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Vintage Polaroid SX-70 Land Camera" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["item_id"] != "1234567890" { // from the URL, never the DOM
		t.Errorf("item_id = %v", rec["item_id"])
	}
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-89.99) > 1e-9 {
		t.Errorf("price.amount = %v, want 89.99", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	if rec["condition"] != "NewCondition" { // schema.org/ prefix stripped
		t.Errorf("condition = %v, want NewCondition", rec["condition"])
	}
	if rec["seller"] != "camera-gear-2010" {
		t.Errorf("seller = %v", rec["seller"])
	}
	if rec["availability"] != "InStock" {
		t.Errorf("availability = %v", rec["availability"])
	}
	if rec["url"] != page { // the decoy "url" in the block must NOT win
		t.Errorf("url = %v, want page URL", rec["url"])
	}
}

func TestEbayItemExtract_DOMFallback(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("ebay_item")
	const page = "https://www.ebay.com/itm/1234567890"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"ebay.com/itm/": {body: verticalFixture(t, "ebay-item-dom.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Vintage Polaroid SX-70 Land Camera" {
		t.Errorf("title = %v", rec["title"])
	}
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map parsed from 'US $89.99'", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-89.99) > 1e-9 {
		t.Errorf("price.amount = %v, want 89.99", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	if rec["condition"] != "New" {
		t.Errorf("condition = %v, want 'Condition:' label stripped", rec["condition"])
	}
	if rec["seller"] != "camera-gear-2010" {
		t.Errorf("seller = %v", rec["seller"])
	}
}

// The query string rides in u.RawQuery, not the path — the id must stay
// digits-only regardless of query clutter.
func TestEbayItemExtract_IDFromQueryShapedPath(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("ebay_item")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"ebay.com/itm/": {body: verticalFixture(t, "ebay-item.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.ebay.com/itm/123456?nordt=true&orig_cvip=true"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["item_id"] != "123456" {
		t.Errorf("item_id = %v, want 123456", rec["item_id"])
	}
}

func TestEbayItemExtract_NoSource(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("ebay_item")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"ebay.com/itm/": {body: []byte(`<html><body>This listing was ended by the seller.</body></html>`)},
	}}
	const page = "https://www.ebay.com/itm/9999999999"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: ebay_item:") ||
		!strings.Contains(err.Error(), page) {
		t.Fatalf("err = %v, want typed no-item error naming the URL", err)
	}
}
