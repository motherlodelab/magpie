package vertical

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/PuerkitoBio/goquery"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "trustpilot",
			Label:    "Trustpilot",
			Desc:     "Business profile reviews from the server-rendered JSON-LD embedded in review pages (no API key, no JS render).",
			Patterns: []string{"https://www.trustpilot.com/review/{domain}"},
		},
		Match:   matchTrustpilot,
		Extract: extractTrustpilot,
	})
}

func matchTrustpilot(u *url.URL) bool {
	return hostIs(u, "trustpilot.com", "www.trustpilot.com") &&
		len(pathSegs(u.Path)) >= 2 && pathSegs(u.Path)[0] == "review" && pathSegs(u.Path)[1] != ""
}

func extractTrustpilot(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, "https://www.trustpilot.com"+u.Path)
	if err != nil {
		return nil, err
	}
	doc, err := parseHTML(body, "trustpilot")
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	reviews := []any{}
	found := false
	doc.Find("script[type='application/ld+json']").Each(func(_ int, s *goquery.Selection) {
		var decoded any
		if err := json.Unmarshal([]byte(s.Text()), &decoded); err != nil {
			return // not-JSON-LD script block; keep scanning
		}
		found = true
		for _, org := range ldNodes(decoded, "Organization") {
			if _, ok := out["name"]; !ok {
				out["name"] = str(org, "name")
			}
			if agg := child(org, "aggregateRating"); agg != nil {
				if _, ok := out["rating_value"]; !ok {
					out["rating_value"] = num(agg, "ratingValue")
					out["review_count"] = num(agg, "reviewCount")
				}
			}
		}
		for _, rv := range ldNodes(decoded, "Review") {
			rating := num(child(rv, "reviewRating"), "ratingValue")
			reviews = append(reviews, map[string]any{
				"author": str(child(rv, "author"), "name"),
				"rating": rating,
				"date":   str(rv, "datePublished"),
				"body":   str(rv, "reviewBody"),
			})
		}
	})
	if !found {
		// Loud, never an empty map: a redesign that drops the JSON-LD
		// must break here, not ship silent empty records.
		return nil, fmt.Errorf("vertical: trustpilot: no JSON-LD on %s", u)
	}
	out["reviews"] = reviews
	return out, nil
}

// ldNodes walks decoded JSON-LD (object, array, or @graph) collecting
// every node whose @type matches typ.
func ldNodes(v any, typ string) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			if s, _ := t["@type"].(string); s == typ {
				out = append(out, t)
			}
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}
