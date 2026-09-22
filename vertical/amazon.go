package vertical

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "amazon_product",
			Label:    "Amazon product",
			Desc:     "Product record from Amazon's buy-box DOM (title, sale+list price, rating, reviews, bullets, availability). Auto-fires on amazon.com/dp/{asin} and /gp/product/{asin}.",
			Patterns: []string{"https://www.amazon.com/dp/{asin}"},
		},
		Match:   matchAmazon,
		Extract: extractAmazon,
	})
}

// ponytail: amazon.com only — regional TLDs (amazon.de/co.uk/…) need their
// own price formats and DOM quirks; extend the host list when repricing
// needs them.
func matchAmazon(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" || !hostIs(u, "amazon.com", "www.amazon.com") {
		return false
	}
	segs := pathSegs(u.Path)
	if len(segs) >= 2 && segs[0] == "dp" && segs[1] != "" {
		return true
	}
	return len(segs) >= 3 && segs[0] == "gp" && segs[1] == "product" && segs[2] != ""
}

func extractAmazon(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	// Amazon's JSON-LD is thin/inconsistent — DOM-first by design.
	doc, err := parseHTML(body, "amazon_product")
	if err != nil {
		return nil, err
	}
	rec := map[string]any{
		"asin": amazonASIN(u), // from the URL, never the DOM
		"url":  u.String(),
	}
	if v := foldSpaces(doc.Find("#productTitle").First().Text()); v != "" {
		rec["title"] = v
	}
	// Sale price: the first .a-price NOT nested in the strikethrough
	// .basisPrice block; list price: the .a-offscreen inside .basisPrice.
	priceText, listText := "", ""
	doc.Find(".a-price").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if s.Closest(".basisPrice").Length() > 0 {
			return true // strikethrough list price, not the sale price
		}
		if t := strings.TrimSpace(s.Find(".a-offscreen").First().Text()); t != "" {
			priceText = t
			return false
		}
		return true
	})
	if t := strings.TrimSpace(doc.Find(".basisPrice .a-offscreen").First().Text()); t != "" {
		listText = t
	}
	if p := parsePriceString(priceText); p != nil {
		rec["price"] = p
	}
	if p := parsePriceString(listText); p != nil {
		rec["list_price"] = p
	}
	rating := firstNumber(doc.Find(`span[data-hook="rating-out-of-text"]`).First().Text())
	if rating == 0 {
		if aria, ok := doc.Find("#acrPopover").First().Attr("title"); ok {
			rating = firstNumber(aria)
		}
	}
	if rating != 0 {
		rec["rating"] = rating
	}
	if n := firstNumber(doc.Find("#acrCustomerReviewText").First().Text()); n != 0 {
		rec["reviews"] = n
	}
	if v := foldSpaces(doc.Find("#availability span").First().Text()); v != "" {
		rec["availability"] = v
	}
	var features []string
	doc.Find("#feature-bullets li span.a-list-item").Each(func(_ int, s *goquery.Selection) {
		if v := foldSpaces(s.Text()); v != "" {
			features = append(features, v)
		}
	})
	if len(features) > 0 {
		rec["features"] = features
	}
	if len(rec) == 2 { // asin+url only — challenge stub or stripped page
		return nil, fmt.Errorf("vertical: amazon_product: no product data at %s", u.String())
	}
	return rec, nil
}

// amazonASIN pulls the id from /dp/{asin} or /gp/product/{asin}/… shapes.
// Guard mirrors matchAmazon — --name can call Extract on any URL and a
// garbage asin must trace to the URL, not to a trusting slice.
func amazonASIN(u *url.URL) string {
	segs := pathSegs(u.Path)
	if len(segs) >= 2 && segs[0] == "dp" {
		return segs[1]
	}
	if len(segs) >= 3 && segs[0] == "gp" && segs[1] == "product" {
		return segs[2]
	}
	return ""
}
