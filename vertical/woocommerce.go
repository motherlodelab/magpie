package vertical

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "woocommerce_product",
			Label:    "WooCommerce product",
			Desc:     "Product record from WooCommerce shops (WC_Structured_Data JSON-LD first, .summary DOM fallback). Explicit --name only: matches any /product/ path.",
			Patterns: []string{"https://{woo-shop}/product/{slug}"},
		},
		Match:   matchWooCommerce,
		Extract: extractWooCommerce,
		OptIn:   true,
	})
}

// matchWooCommerce is deliberately permissive (any /product/ path — Woo's
// default permalink, no collision with shopify's plural) — hence OptIn.
func matchWooCommerce(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.Contains(u.Path, "/product/")
}

func extractWooCommerce(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	rec := map[string]any{"url": u.String()}
	if m, ok := firstTypedBlock(body, "Product"); ok {
		if v := str(m, "name", "title"); v != "" {
			rec["title"] = v
		}
		if p := offersPrice(m); p != nil {
			rec["price"] = p
		}
		if v := str(m, "sku"); v != "" {
			rec["sku"] = v
		}
		brand := ""
		switch b := m["brand"].(type) {
		case string:
			brand = b
		case map[string]any:
			brand = str(b, "name")
		}
		if brand != "" {
			rec["brand"] = brand
		}
		if v := schemaShort(str(child(m, "offers"), "availability")); v != "" {
			rec["stock_status"] = v
		}
		if agg := child(m, "aggregateRating"); agg != nil {
			if n := num(agg, "ratingValue"); n != 0 {
				rec["rating"] = n
			}
			if n := num(agg, "reviewCount"); n != 0 {
				rec["reviews"] = n
			}
		}
		if imgs := nameList(m["image"]); len(imgs) > 0 {
			rec["images"] = imgs
		}
		if len(rec) > 1 { // more than url — real data
			return rec, nil
		}
	}
	// DOM fallback: the .summary block Woo themes render.
	doc, derr := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if derr != nil {
		return nil, fmt.Errorf("vertical: woocommerce_product: parse html: %w", derr)
	}
	title := foldSpaces(doc.Find(".product_title").First().Text())
	if title == "" {
		title = foldSpaces(doc.Find(".entry-title").First().Text())
	}
	if title != "" {
		rec["title"] = title
	}
	// Sale price first (.price ins), regular price as fallback — the
	// sale price is the one you pay.
	amountSel := doc.Find(".price ins .woocommerce-Price-amount")
	if amountSel.Length() == 0 {
		amountSel = doc.Find(".price .woocommerce-Price-amount")
	}
	if p := parsePriceString(amountSel.First().Text()); p != nil {
		rec["price"] = p
	}
	if v := foldSpaces(doc.Find(".sku").First().Text()); v != "" {
		rec["sku"] = v
	}
	if v := foldSpaces(doc.Find(".stock").First().Text()); v != "" { // p.stock, .stock — one class covers both
		rec["stock_status"] = v
	}
	if len(rec) > 1 {
		return rec, nil
	}
	return nil, fmt.Errorf("vertical: woocommerce_product: no product data at %s", u.String())
}
