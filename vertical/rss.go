package vertical

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "rss",
			Label:    "RSS/Atom feed",
			Desc:     "Feed items from RSS 2.0 or Atom XML, capped at 50 (fuel for watch/diff). Explicit-only: matches feed-shaped URL hints.",
			Patterns: []string{"https://{site}/feed", "https://{site}/rss", "https://{site}/feed.xml"},
		},
		Match:   matchFeed,
		Extract: extractRSS,
		OptIn:   true,
	})
}

// matchFeed is a hint, not proof (any /rss path might be a page) — OptIn.
func matchFeed(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.Contains(p, "/feed") || strings.Contains(p, "/rss") ||
		strings.HasSuffix(p, ".xml") || strings.HasSuffix(p, ".atom")
}

// rssItemCap keeps records bounded on fat feeds.
// ponytail: first 50 in document order, no date-windowing — upgrade path
// = since/limit parameters if a consumer ever needs deeper history.
const rssItemCap = 50

type rssDoc struct {
	Channel struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		Description string `xml:"description"`
		Items       []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			PubDate     string `xml:"pubDate"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

type atomDoc struct {
	Title    string     `xml:"title"`
	Subtitle string     `xml:"subtitle"`
	Links    []atomLink `xml:"link"`
	Entries  []struct {
		Title     string     `xml:"title"`
		Links     []atomLink `xml:"link"`
		Published string     `xml:"published"`
		Updated   string     `xml:"updated"`
		Summary   string     `xml:"summary"`
	} `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

// atomLinkHref picks a link: rel="alternate" first, then no-rel, then the
// first link of any rel (self-only feeds still get a URL).
func atomLinkHref(links []atomLink) string {
	var noRel, anyRel string
	for _, l := range links {
		if l.Rel == "alternate" {
			return l.Href
		}
		if l.Rel == "" && noRel == "" {
			noRel = l.Href
		}
		if anyRel == "" {
			anyRel = l.Href
		}
	}
	if noRel != "" {
		return noRel
	}
	return anyRel
}

func extractRSS(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, rssItemCap)
	rec := map[string]any{"items": items} // never nil, even for empty feeds
	switch rootElement(body) {
	case "rss":
		var doc rssDoc
		if err := xml.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("vertical: rss: not a feed at %s", u.String())
		}
		if v := strings.TrimSpace(doc.Channel.Title); v != "" {
			rec["title"] = v
		}
		if v := strings.TrimSpace(doc.Channel.Link); v != "" {
			rec["link"] = v
		}
		if v := strings.TrimSpace(doc.Channel.Description); v != "" {
			rec["description"] = v
		}
		for _, it := range doc.Channel.Items {
			if len(items) == rssItemCap {
				break
			}
			if m := feedItem(it.Title, it.Link, it.PubDate, it.Description); m != nil {
				items = append(items, m)
			}
		}
	case "feed":
		var doc atomDoc
		if err := xml.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("vertical: rss: not a feed at %s", u.String())
		}
		if v := strings.TrimSpace(doc.Title); v != "" {
			rec["title"] = v
		}
		if v := atomLinkHref(doc.Links); v != "" {
			rec["link"] = v
		}
		if v := strings.TrimSpace(doc.Subtitle); v != "" {
			rec["description"] = v
		}
		for _, e := range doc.Entries {
			if len(items) == rssItemCap {
				break
			}
			pub := e.Published
			if pub == "" {
				pub = e.Updated
			}
			if m := feedItem(e.Title, atomLinkHref(e.Links), pub, e.Summary); m != nil {
				items = append(items, m)
			}
		}
	default:
		return nil, fmt.Errorf("vertical: rss: not a feed at %s", u.String())
	}
	rec["items"] = items
	return rec, nil
}

// feedItem maps one feed entry, omitting empty fields; nil when the entry
// carries nothing at all.
func feedItem(title, link, published, summary string) map[string]any {
	m := map[string]any{}
	if v := strings.TrimSpace(title); v != "" {
		m["title"] = v
	}
	if v := strings.TrimSpace(link); v != "" {
		m["link"] = v
	}
	if v := strings.TrimSpace(published); v != "" {
		m["published"] = v
	}
	if v := strings.TrimSpace(summary); v != "" {
		m["summary"] = v
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// rootElement returns the first XML element name, skipping the prolog,
// comments and doctype — the rss-vs-feed dialect sniff.
func rootElement(body []byte) string {
	for i := 0; i+1 < len(body); i++ {
		if body[i] != '<' {
			continue
		}
		if c := body[i+1]; c == '?' || c == '!' {
			for ; i < len(body) && body[i] != '>'; i++ { // skip to prolog end
			}
			continue
		}
		for j := i + 1; j < len(body); j++ {
			switch body[j] {
			case '>', ' ', '\t', '\n', '\r', '/':
				return string(body[i+1 : j])
			}
		}
	}
	return ""
}
