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
			Name:     "dev_to_article",
			Label:    "dev.to article",
			Desc:     "Article record from dev.to's server-rendered crayons markup (no JS render needed). Auto-fires on dev.to/{user}/{slug}.",
			Patterns: []string{"https://dev.to/{user}/{slug}"},
		},
		Match:   matchDevTo,
		Extract: extractDevTo,
	})
}

func matchDevTo(u *url.URL) bool {
	segs := pathSegs(u.Path)
	return (u.Scheme == "http" || u.Scheme == "https") &&
		hostIs(u, "dev.to") &&
		len(segs) == 2 && segs[0] != "t" // /t/{tag} listings are also 2-seg
}

func extractDevTo(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	doc, err := parseHTML(body, "dev_to_article")
	if err != nil {
		return nil, err
	}
	rec := map[string]any{"url": u.String()}
	if v := foldSpaces(doc.Find("h1.crayons-title").First().Text()); v != "" {
		rec["title"] = v
	}
	if v := foldSpaces(doc.Find(".crayons-article__header__author a").First().Text()); v != "" {
		rec["author"] = v
	}
	if v, ok := doc.Find("time[datetime]").First().Attr("datetime"); ok { // raw passthrough
		rec["published"] = v
	}
	var tags []string
	doc.Find("a.crayons-tag").Each(func(_ int, s *goquery.Selection) {
		if v := strings.TrimPrefix(foldSpaces(s.Text()), "#"); v != "" {
			tags = append(tags, v)
		}
	})
	if len(tags) > 0 {
		rec["tags"] = tags
	}
	reactions := firstNumber(doc.Find(".crayons-reaction__count--reaction").First().Text())
	if reactions == 0 {
		if aria, ok := doc.Find(".crayons-reaction").First().Attr("aria-label"); ok {
			reactions = firstNumber(aria)
		}
	}
	if reactions != 0 {
		rec["reactions"] = reactions
	}
	if n := firstNumber(doc.Find("#comments").First().Text()); n != 0 {
		rec["comments"] = n
	}
	bodySel := doc.Find(".crayons-article__body p")
	if bodySel.Length() == 0 {
		bodySel = doc.Find("p")
	}
	if v := foldSpaces(bodySel.First().Text()); v != "" {
		rec["description"] = v
	}
	if len(rec) == 1 {
		return nil, fmt.Errorf("vertical: dev_to_article: no article data at %s", u.String())
	}
	return rec, nil
}
