package vertical

import (
	"context"
	"fmt"
	"net/url"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "article",
			Label:    "Article",
			Desc:     "Article record from schema.org Article/NewsArticle/BlogPosting JSON-LD. Explicit-only: matches any page.",
			Patterns: []string{"https://{news-site}/{article}"},
		},
		Match:   matchHTTP,
		Extract: extractArticle,
		OptIn:   true,
	})
}

// articleTypes: Contains("Article") already covers NewsArticle &
// SocialMediaPosting; BlogPosting is the one common spelling that doesn't.
var articleTypes = []string{"Article", "BlogPosting"}

func extractArticle(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	// Deliberately JSON-LD only: og/meta tags belong to the `og` extractor,
	// whose keys (og_title, og_image, canonical) never collide with ours.
	m, ok := firstTypedBlock(body, articleTypes...)
	if !ok {
		return nil, fmt.Errorf("vertical: article: no Article data at %s", u.String())
	}
	rec := map[string]any{"url": u.String()}
	if v := str(m, "headline"); v != "" {
		rec["headline"] = v
	}
	if authors := nameList(m["author"]); len(authors) > 0 {
		rec["authors"] = authors
	}
	if v := str(m, "datePublished"); v != "" {
		rec["datePublished"] = v
	}
	if v := str(m, "dateModified"); v != "" {
		rec["dateModified"] = v
	}
	if names := nameList(m["publisher"]); len(names) > 0 {
		rec["publisher"] = names[0]
	}
	if img := imageStr(m["image"]); img != "" {
		rec["image"] = img
	}
	return rec, nil
}

// imageStr picks the first usable image URL from string, array, or
// ImageObject spellings.
func imageStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, e := range t {
			if s := imageStr(e); s != "" {
				return s
			}
		}
	case map[string]any:
		return str(t, "url")
	}
	return ""
}
