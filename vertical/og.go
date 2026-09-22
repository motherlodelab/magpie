package vertical

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "og",
			Label:    "Open Graph",
			Desc:     "Generic OG/Twitter meta extraction for ANY URL. Explicit --vertical og only — never auto-fires (OptIn).",
			Patterns: []string{"<any URL>"},
		},
		// Match matches everything — MatchURL skips OptIn extractors, so
		// this can never auto-fire (pinned by TestOG_NeverAutoFires).
		Match:   func(*url.URL) bool { return true },
		Extract: extractOG,
		OptIn:   true,
	})
}

// extractOG pulls the OG/Twitter meta set plus title/description/
// canonical/first-h1 from any fetched HTML into a flat map. Only
// non-empty fields land (honest records, no null noise).
func extractOG(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	doc, err := parseHTML(body, "og")
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	set := func(key, val string) {
		if val != "" {
			out[key] = val
		}
	}
	for _, p := range []string{"title", "description", "image", "url", "type"} {
		c, _ := doc.Find(`meta[property="og:` + p + `"]`).First().Attr("content")
		set("og_"+p, foldSpaces(c))
	}
	for _, n := range []string{"card", "title", "description", "image"} {
		c, _ := doc.Find(`meta[name="twitter:` + n + `"]`).First().Attr("content")
		set("twitter_"+n, foldSpaces(c))
	}
	set("title", foldSpaces(doc.Find("title").First().Text()))
	if _, ok := out["description"]; !ok {
		c, _ := doc.Find("meta[name='description']").First().Attr("content")
		set("description", foldSpaces(c))
	}
	if c, ok := doc.Find("link[rel=canonical]").First().Attr("href"); ok {
		set("canonical", foldSpaces(c))
	}
	set("h1", foldSpaces(doc.Find("h1").First().Text()))
	if len(out) == 0 {
		return nil, fmt.Errorf("vertical: og: no OG/meta fields found on %s", u)
	}
	return out, nil
}

// foldSpaces collapses runs of whitespace in attribute/text values (OG
// descriptions are frequently multiline).
func foldSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
