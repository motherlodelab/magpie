package vertical_test

import (
	"math"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestWooCommerce_Match(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("woocommerce_product")
	if !ok {
		t.Fatal("woocommerce_product not registered")
	}
	if !ex.OptIn {
		t.Error("woocommerce_product must be OptIn")
	}
	yes := []string{
		"https://northwind.example/product/merino-beanie",
		"https://shop.example/product/hat", // any host: shape match, explicit-only
	}
	no := []string{
		"https://shop.example/products/hoodie", // shopify's plural — NOT woo
		"https://northwind.example/shop/",
		"https://northwind.example/product-category/hats/", // category listing, not a product
		"file:///tmp/product/x",
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

// The permissive /product/ shape must never auto-fire: MatchURL skips
// OptIn extractors (TestOptInNeverSteals class, woo row).
func TestWooCommerce_NeverAutoFires(t *testing.T) {
	t.Parallel()
	if ex, ok := vertical.MatchURL("https://shop.example/product/x"); ok {
		t.Errorf("MatchURL(product URL) auto-fired %s — OptIn must never auto-match", ex.Info.Name)
	}
}

// --name dispatch smoke: Lookup + Extract works on a non-woo host; the
// OptIn matcher ignores the host entirely.
func TestWooCommerce_NameSmoke(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("woocommerce_product")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"anyhost.example/product/": {body: verticalFixture(t, "woo-product.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://anyhost.example/product/merino-beanie"))
	if err != nil {
		t.Fatalf("Extract via --name: %v", err)
	}
	if rec["title"] != "Merino Wool Beanie" {
		t.Errorf("title = %v", rec["title"])
	}
	if len(fx.requests()) != 1 || !strings.Contains(fx.requests()[0], "merino-beanie") {
		t.Errorf("requests = %v, want exactly the page URL", fx.requests())
	}
}

func TestWooCommerceExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("woocommerce_product")
	const page = "https://northwind.example/product/merino-beanie"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"northwind.example/product/": {body: verticalFixture(t, "woo-product.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Merino Wool Beanie" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["sku"] != "MWB-01" {
		t.Errorf("sku = %v", rec["sku"])
	}
	if rec["brand"] != "Northwind" {
		t.Errorf("brand = %v, want {name} object flattened", rec["brand"])
	}
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-24.5) > 1e-9 {
		t.Errorf("price.amount = %v, want 24.5", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	if rec["stock_status"] != "InStock" {
		t.Errorf("stock_status = %v", rec["stock_status"])
	}
	if r, _ := rec["rating"].(float64); math.Abs(r-4.8) > 1e-9 {
		t.Errorf("rating = %#v, want 4.8", rec["rating"])
	}
	if rec["reviews"] != 57.0 {
		t.Errorf("reviews = %#v, want 57 (float64)", rec["reviews"])
	}
	imgs, ok := rec["images"].([]string)
	if !ok || len(imgs) != 2 || !strings.HasSuffix(imgs[0], "beanie-1.jpg") {
		t.Errorf("images = %#v, want 2 URLs in order", rec["images"])
	}
	if rec["url"] != page {
		t.Errorf("url = %v, want page URL", rec["url"])
	}
}

func TestWooCommerceExtract_DOMFallback(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("woocommerce_product")
	const page = "https://northwind.example/product/merino-beanie"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"northwind.example/product/": {body: verticalFixture(t, "woo-product-dom.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Merino Wool Beanie" {
		t.Errorf("title = %v", rec["title"])
	}
	// Sale price wins over the struck-through regular price.
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-19.90) > 1e-9 {
		t.Errorf("price.amount = %v, want 19.90 (ins beats del)", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	if rec["sku"] != "MWB-01" {
		t.Errorf("sku = %v", rec["sku"])
	}
	if rec["stock_status"] != "In stock" {
		t.Errorf("stock_status = %v", rec["stock_status"])
	}
	// The .summary DOM shape carries no brand/rating/reviews — absent, never guessed.
	for _, k := range []string{"brand", "rating", "reviews"} {
		if _, ok := rec[k]; ok {
			t.Errorf("%s present in DOM-fallback record — omission broken", k)
		}
	}
}

func TestWooCommerceExtract_NoSource(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("woocommerce_product")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"shop.example/product/": {body: []byte(`<html><body>Nothing to see here.</body></html>`)},
	}}
	const page = "https://shop.example/product/gone"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: woocommerce_product:") ||
		!strings.Contains(err.Error(), page) {
		t.Fatalf("err = %v, want typed no-product error naming the URL", err)
	}
}
