package vertical

import (
	"bytes"
	"context"
	"fmt"
	"net/url"

	"github.com/PuerkitoBio/goquery"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "etsy_listing",
			Label:    "Etsy listing",
			Desc:     "Listing record from Etsy product pages: schema.org Product JSON-LD first, og:price meta fallback. Auto-fires on etsy.com/listing/{id}.",
			Patterns: []string{"https://www.etsy.com/listing/{id}/{slug}"},
		},
		Match:   matchEtsy,
		Extract: extractEtsy,
	})
}

func matchEtsy(u *url.URL) bool {
	segs := pathSegs(u.Path)
	return (u.Scheme == "http" || u.Scheme == "https") &&
		hostIs(u, "etsy.com", "www.etsy.com") &&
		len(segs) >= 2 && segs[0] == "listing" && segs[1] != ""
}

func extractEtsy(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	rec := map[string]any{
		"listing_id": pathSegs(u.Path)[1],
		"url":        u.String(),
	}
	if m, ok := firstTypedBlock(body, "Product"); ok {
		if v := str(m, "name"); v != "" {
			rec["title"] = v
		}
		if p := offersPrice(m); p != nil {
			rec["price"] = p
		}
		if offers := offersMap(m); offers != nil {
			if v := str(child(offers, "seller"), "name"); v != "" {
				rec["seller"] = v
			}
			if v := schemaShort(str(offers, "availability")); v != "" {
				rec["availability"] = v
			}
		}
		if agg := child(m, "aggregateRating"); agg != nil {
			if n := num(agg, "ratingValue"); n != 0 {
				rec["rating"] = n
			}
			if n := num(agg, "reviewCount"); n != 0 {
				rec["reviews"] = n
			}
		}
		if len(rec) > 2 { // more than listing_id+url — real data
			return rec, nil
		}
	}
	// DOM fallback: no usable JSON-LD.
	doc, derr := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if derr != nil {
		return nil, fmt.Errorf("vertical: etsy_listing: parse html: %w", derr)
	}
	title := foldSpaces(doc.Find(`h1[data-test-id="listing-page-title"]`).First().Text())
	if title == "" {
		title = foldSpaces(doc.Find("h1").First().Text())
	}
	if title != "" {
		rec["title"] = title
	}
	if amt, ok := doc.Find(`meta[property="og:price:amount"]`).First().Attr("content"); ok {
		if n := numVal(amt); n != 0 {
			price := map[string]any{"amount": n}
			if c, _ := doc.Find(`meta[property="og:price:currency"]`).First().Attr("content"); c != "" {
				price["currency"] = c
			}
			rec["price"] = price
		}
	}
	if len(rec) > 2 {
		return rec, nil
	}
	return nil, fmt.Errorf("vertical: etsy_listing: no listing data at %s", u.String())
}
