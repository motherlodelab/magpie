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
			Name:     "ebay_item",
			Label:    "eBay item",
			Desc:     "Item record from eBay listings: schema.org Product JSON-LD first, modern DOM fallback. Auto-fires on ebay.com/itm/{id}.",
			Patterns: []string{"https://www.ebay.com/itm/{id}"},
		},
		Match:   matchEbay,
		Extract: extractEbay,
	})
}

func matchEbay(u *url.URL) bool {
	segs := pathSegs(u.Path)
	return (u.Scheme == "http" || u.Scheme == "https") &&
		hostIs(u, "ebay.com", "www.ebay.com") &&
		len(segs) >= 2 && segs[0] == "itm" && ebayItemID(segs[1]) != ""
}

// ebayItemID extracts the numeric item id from an /itm/ segment; query
// strings and slug suffixes never pollute the id.
func ebayItemID(seg string) string {
	var b strings.Builder
	for _, r := range seg {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func extractEbay(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	rec := map[string]any{
		"item_id": ebayItemID(pathSegs(u.Path)[1]), // from the URL, never the DOM
		"url":     u.String(),                      // the page URL — a "url" inside the JSON-LD is decoy-shaped
	}
	if m, ok := firstTypedBlock(body, "Product"); ok {
		if v := str(m, "name"); v != "" {
			rec["title"] = v
		}
		if p := offersPrice(m); p != nil {
			rec["price"] = p
		}
		if v := schemaShort(str(child(m, "offers"), "itemCondition")); v != "" {
			rec["condition"] = v
		}
		if v := str(child(child(m, "offers"), "seller"), "name"); v != "" {
			rec["seller"] = v
		}
		if v := schemaShort(str(child(m, "offers"), "availability")); v != "" {
			rec["availability"] = v
		}
		if len(rec) > 2 { // more than item_id+url — real data
			return rec, nil
		}
	}
	// DOM fallback: the x- component classes eBay serves server-side.
	doc, derr := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if derr != nil {
		return nil, fmt.Errorf("vertical: ebay_item: parse html: %w", derr)
	}
	if v := foldSpaces(doc.Find("#itemTitle").First().Text()); v != "" {
		rec["title"] = v
	}
	if p := parsePriceString(doc.Find(".x-price-primary").First().Text()); p != nil {
		rec["price"] = p
	}
	if v := strings.TrimPrefix(foldSpaces(doc.Find(".x-item-details").First().Text()), "Condition:"); v != "" {
		rec["condition"] = foldSpaces(v)
	}
	if v := foldSpaces(doc.Find(".x-sellercard-atf__info__about-seller").First().Text()); v != "" {
		rec["seller"] = v
	}
	if len(rec) > 2 {
		return rec, nil
	}
	return nil, fmt.Errorf("vertical: ebay_item: no item data at %s", u.String())
}
