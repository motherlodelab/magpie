package vertical

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "shopify_product",
			Label:    "Shopify product",
			Desc:     "Product record via the store's /products/{handle}.js endpoint. Explicit-only: matches any /products/ path.",
			Patterns: []string{"https://{shop}/products/{handle}"},
		},
		Match:   matchShopify,
		Extract: extractShopify,
		OptIn:   true,
	})
	register(Extractor{
		Info: Info{
			Name:     "ecommerce_product",
			Label:    "E-commerce product",
			Desc:     "Product record from embedded JSON-LD (first Product block). Explicit-only: matches any page.",
			Patterns: []string{"https://{any-product-page}"},
		},
		Match:   matchEcommerce,
		Extract: extractEcommerce,
		OptIn:   true,
	})
}

// matchShopify is deliberately permissive (any /products/ path) — hence OptIn.
func matchShopify(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.Contains(u.Path, "/products/")
}

// matchEcommerce is maximally permissive — hence OptIn, never auto-fired.
func matchEcommerce(u *url.URL) bool {
	return u.Scheme == "http" || u.Scheme == "https"
}

func extractShopify(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	segs := pathSegs(u.Path)
	handle := ""
	for i, s := range segs {
		if s == "products" && i+1 < len(segs) {
			handle = strings.TrimSuffix(segs[i+1], ".js")
			break
		}
	}
	if handle == "" {
		return nil, fmt.Errorf("vertical: shopify: no product handle in %s", u.Path)
	}
	m, err := fetchJSON(ctx, f, u.Scheme+"://"+u.Host+"/products/"+handle+".js")
	if err != nil {
		return nil, err
	}
	// price arrives as integer cents → major units as float (compared with
	// 1e-9 tolerance in tests — 1999/100 is not exact in binary).
	price := num(m, "price") / 100
	var images []any
	if raw, _ := m["images"].([]any); raw != nil {
		for _, im := range raw {
			if imM, _ := im.(map[string]any); imM != nil {
				if src := str(imM, "src"); src != "" {
					images = append(images, src)
				}
			}
		}
	}
	if images == nil {
		images = []any{}
	}
	// NOTE: the .js payload carries no currency — the key is omitted, never guessed.
	return map[string]any{
		"title":  str(m, "title"),
		"vendor": str(m, "vendor"),
		"price":  price,
		"type":   str(m, "type", "product_type"),
		"images": images,
		"url":    u.Scheme + "://" + u.Host + "/products/" + handle,
	}, nil
}

func extractEcommerce(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	if m, ok := firstTypedBlock(body, "Product"); ok {
		return productMap(m, u.String()), nil
	}
	return nil, fmt.Errorf("vertical: ecommerce: no product data at %s", u.String())
}

func productMap(m map[string]any, pageURL string) map[string]any {
	brand := ""
	switch b := m["brand"].(type) {
	case string:
		brand = b
	case map[string]any:
		brand = str(b, "name")
	}
	offers := child(m, "offers")
	if offers == nil {
		if arr, _ := m["offers"].([]any); len(arr) > 0 {
			offers, _ = arr[0].(map[string]any)
		}
	}
	var price any
	var currency, availability string
	if offers != nil {
		price = offers["price"]
		currency = str(offers, "priceCurrency")
		availability = str(offers, "availability")
	}
	name := str(m, "name")
	if name == "" {
		name = str(m, "title")
	}
	url := str(m, "url")
	if url == "" {
		url = pageURL
	}
	return map[string]any{
		"name":         name,
		"brand":        brand,
		"price":        price,
		"currency":     currency,
		"availability": availability,
		"url":          url,
	}
}

// offersMap normalizes a Product block's offers (single object or first
// array element) — the productMap shape, shared by the marketplace verticals.
func offersMap(m map[string]any) map[string]any {
	if offers := child(m, "offers"); offers != nil {
		return offers
	}
	if arr, _ := m["offers"].([]any); len(arr) > 0 {
		return anyMap(arr[0])
	}
	return nil
}

// offersPrice builds the {amount, currency} record price from a Product
// block's offers; nil when no numeric price (omission over guessing).
func offersPrice(m map[string]any) map[string]any {
	offers := offersMap(m)
	if offers == nil {
		return nil
	}
	amount := num(offers, "price")
	if amount == 0 {
		return nil
	}
	out := map[string]any{"amount": amount}
	if c := str(offers, "priceCurrency"); c != "" {
		out["currency"] = c
	}
	return out
}

// schemaShort strips the schema.org namespace prefix from enum-ish URLs
// ("https://schema.org/NewCondition" → "NewCondition") — the suffix is
// the honest enum; the full URL is namespace leak.
func schemaShort(s string) string {
	for _, p := range []string{"https://schema.org/", "http://schema.org/"} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

// firstNumber parses the first number in free text ("8,234 ratings" →
// 8234, "4.5 out of 5 stars" → 4.5); 0 when none.
func firstNumber(s string) float64 {
	s = strings.ReplaceAll(s, ",", "")
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.'
	}) {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			return v
		}
	}
	return 0
}

// parsePriceString extracts an en-US dot-decimal price from free DOM text
// ("US $89.99" → {amount: 89.99, currency: "USD"}). Unknown symbol ⇒
// currency omitted, amount kept; no number ⇒ nil (omit the whole key).
// ponytail: en-US only — regional formats ("€12,50") misparse by design;
// upgrade alongside any regional-TLD amazon/ebay wave.
func parsePriceString(s string) map[string]any {
	currency := ""
	for _, sc := range [][2]string{{"$", "USD"}, {"€", "EUR"}, {"£", "GBP"}} {
		if strings.Contains(s, sc[0]) {
			currency = sc[1]
			break
		}
	}
	n := firstNumber(s)
	if n == 0 {
		return nil
	}
	out := map[string]any{"amount": n}
	if currency != "" {
		out["currency"] = currency
	}
	return out
}
