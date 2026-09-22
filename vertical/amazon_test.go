package vertical_test

import (
	"math"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestAmazonProductMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("amazon_product")
	if !ok {
		t.Fatal("amazon_product not registered")
	}
	yes := []string{
		"https://www.amazon.com/dp/B08N5WRWNW",
		"https://amazon.com/dp/B08N5WRWNW",
		"https://www.amazon.com/gp/product/B08N5WRWNW/ref=s9",
	}
	no := []string{
		"https://www.amazon.com/s?k=headphones", // search, not a product
		"https://www.amazon.de/dp/B08N5WRWNW",   // regional TLD: ponytail ceiling
		"https://notamazon.com/dp/B08N5WRWNW",
		"file:///tmp/dp/B08N5WRWNW",
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

func TestAmazonProductExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("amazon_product")
	const page = "https://www.amazon.com/dp/B08N5WRWNW"
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"amazon.com/dp/": {body: verticalFixture(t, "amazon-product.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Sony WH-1000XM4 Wireless Headphones" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["asin"] != "B08N5WRWNW" { // from the URL, never the DOM
		t.Errorf("asin = %v", rec["asin"])
	}
	price, ok := rec["price"].(map[string]any)
	if !ok {
		t.Fatalf("price = %#v, want map", rec["price"])
	}
	if amt, _ := price["amount"].(float64); math.Abs(amt-289.99) > 1e-9 {
		t.Errorf("price.amount = %v, want 289.99", price["amount"])
	}
	if price["currency"] != "USD" {
		t.Errorf("price.currency = %v", price["currency"])
	}
	list, ok := rec["list_price"].(map[string]any)
	if !ok {
		t.Fatalf("list_price = %#v, want map", rec["list_price"])
	}
	if amt, _ := list["amount"].(float64); math.Abs(amt-349.99) > 1e-9 {
		t.Errorf("list_price.amount = %v, want 349.99", list["amount"])
	}
	if rec["rating"] != 4.5 { // "4.5 out of 5 stars" → float64
		t.Errorf("rating = %#v, want 4.5", rec["rating"])
	}
	if rec["reviews"] != 8234.0 { // "8,234 ratings" → float64
		t.Errorf("reviews = %#v, want 8234", rec["reviews"])
	}
	if rec["availability"] != "In Stock" {
		t.Errorf("availability = %v", rec["availability"])
	}
	feats, ok := rec["features"].([]string)
	if !ok || len(feats) != 3 || feats[0] != "Industry-leading noise cancellation" {
		t.Errorf("features = %#v, want 3 in order", rec["features"])
	}
	if rec["url"] != page {
		t.Errorf("url = %v, want page URL", rec["url"])
	}
}

func TestAmazonProductExtract_PriceStrings(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("amazon_product")
	cases := []struct {
		name  string
		html  string
		check func(t *testing.T, rec map[string]any)
	}{
		{"no list price", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">$10.00</span></div>`,
			func(t *testing.T, rec map[string]any) {
				if _, ok := rec["list_price"]; ok {
					t.Error("list_price present — omission broken")
				}
			}},
		{"comma thousands", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">$1,299.00</span></div>`,
			func(t *testing.T, rec map[string]any) {
				p := rec["price"].(map[string]any)
				if amt, _ := p["amount"].(float64); math.Abs(amt-1299) > 1e-9 {
					t.Errorf("amount = %v, want 1299", p["amount"])
				}
			}},
		{"unknown currency kept amount", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">CHF 49.00</span></div>`,
			func(t *testing.T, rec map[string]any) {
				p := rec["price"].(map[string]any)
				if _, ok := p["currency"]; ok {
					t.Error("currency present for unmapped symbol — guessing broken")
				}
			}},
		{"unparseable price omitted", `<span id="productTitle">X</span><div class="a-price"><span class="a-offscreen">Currently unavailable</span></div>`,
			func(t *testing.T, rec map[string]any) {
				if _, ok := rec["price"]; ok {
					t.Error("price present on unparseable string — must omit, never zero")
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
				"amazon.com/dp/": {body: []byte(tc.html)},
			}}
			rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.amazon.com/dp/B0TEST"))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			tc.check(t, rec)
		})
	}
}

func TestAmazonProductExtract_ChallengeStub(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("amazon_product")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"amazon.com/dp/": {body: []byte(`<html><body>Sorry! Something went wrong!</body></html>`)},
	}}
	const page = "https://www.amazon.com/dp/B0CHALLENGE"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: amazon_product:") ||
		!strings.Contains(err.Error(), page) {
		t.Fatalf("err = %v, want typed no-product error naming the URL", err)
	}
}
