package vertical

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "reddit",
			Label:    "Reddit",
			Desc:     "Post or subreddit via old.reddit HTML/.json (tokenless), or OAuth app-only .json when MAGPIE_REDDIT_* creds are set.",
			Patterns: []string{"https://www.reddit.com/r/{sub}/comments/{id}/", "https://www.reddit.com/r/{sub}/"},
		},
		Match:   matchReddit,
		Extract: extractReddit,
	})
	register(Extractor{
		Info: Info{
			Name:     "hackernews",
			Label:    "Hacker News",
			Desc:     "Story or comment via the Algolia HN items API.",
			Patterns: []string{"https://news.ycombinator.com/item?id={n}"},
		},
		Match:   matchHN,
		Extract: extractHN,
	})
}

var redditHosts = []string{"reddit.com", "www.reddit.com", "old.reddit.com", "new.reddit.com"}

// matchReddit accepts canonical hosts with an /r/{sub} path. sh.reddit.com
// shortlinks and bare /r/ are out.
func matchReddit(u *url.URL) bool {
	if !hostIs(u, redditHosts...) {
		return false
	}
	segs := pathSegs(u.Path)
	return len(segs) >= 2 && segs[0] == "r" && segs[1] != ""
}

func isPermalink(u *url.URL) bool {
	segs := pathSegs(u.Path)
	return len(segs) >= 4 && segs[0] == "r" && segs[2] == "comments"
}

func extractReddit(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	// OAuth-first: with all three MAGPIE_REDDIT_* vars set, ride the
	// app-only token against oauth.reddit.com .json for BOTH permalinks
	// and listings (anonymous listings are IP-blocked in practice).
	// Token failures degrade loudly — never a silent fallback to the
	// anonymous ladder, which would hide a misconfigured env.
	if os.Getenv("MAGPIE_REDDIT_CLIENT_ID") != "" || os.Getenv("MAGPIE_REDDIT_CLIENT_SECRET") != "" || os.Getenv("MAGPIE_REDDIT_UA") != "" {
		headers, aerr := redditAuth(ctx)
		if aerr != nil {
			return nil, aerr
		}
		api := redditOAuthHost + u.RequestURI() + ".json"
		jbody, jerr := fetchBytesAuth(ctx, f, api, headers)
		if jerr != nil {
			return nil, jerr
		}
		if isPermalink(u) {
			return redditThreadFromJSON(jbody, "https://www.reddit.com"+u.RequestURI())
		}
		return redditListingFromJSON(jbody, "https://www.reddit.com"+u.RequestURI())
	}
	old := "https://old.reddit.com" + u.RequestURI()
	www := "https://www.reddit.com" + u.RequestURI()
	if isPermalink(u) {
		// Permalinks go .json-first: the comment tree is the payload.
		// Subreddit listings stay HTML-first, and HTML remains the
		// permalink fallback (old summary shape) for any .json failure —
		// HTTP or decode, a shape drift must degrade, not break.
		if jbody, jerr := fetchBytes(ctx, f, old+".json"); jerr == nil {
			if rec, perr := redditThreadFromJSON(jbody, www); perr == nil {
				return rec, nil
			}
		}
		body, herr := fetchBytes(ctx, f, old)
		if herr != nil {
			return nil, herr
		}
		return redditPostFromHTML(body, www), nil
	}
	body, err := fetchBytes(ctx, f, old)
	if err != nil {
		return nil, err
	}
	return redditSubredditFromHTML(body, www), nil
}

// redditListingFromJSON parses the single-listing subreddit .json shape
// (children = t3 posts + t5 subreddit info) — NOT the permalink
// two-listing shape redditThreadFromJSON handles. The t5 child carries
// the subreddit title/description/subscribers; the first t3 title is a
// fallback so a sidebar-less listing still carries content.
func redditListingFromJSON(body []byte, url string) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("vertical: reddit .json: %w", err)
	}
	kids, _ := child(m, "data")["children"].([]any)
	rec := map[string]any{"kind": "subreddit", "url": url, "author": "", "comments": float64(0)}
	for _, kid := range kids {
		cm, _ := kid.(map[string]any)
		d := child(cm, "data")
		switch cm["kind"] {
		case "t5":
			rec["title"] = str(d, "title")
			rec["selftext"] = str(d, "public_description")
			rec["score"] = num(d, "subscribers")
		case "t3":
			if rec["title"] == nil {
				rec["title"] = str(d, "title")
			}
		}
	}
	if rec["title"] == nil {
		return nil, fmt.Errorf("vertical: reddit listing: no t5/t3 children")
	}
	return rec, nil
}

func redditPostFromHTML(body []byte, url string) map[string]any {
	doc, derr := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if derr != nil {
		return map[string]any{"kind": "post", "url": url}
	}
	title := strings.TrimSpace(doc.Find("a.title").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").First().Text())
	}
	return map[string]any{
		"kind":     "post",
		"title":    title,
		"author":   strings.TrimSpace(doc.Find("a.author").First().Text()),
		"score":    parseCount(doc.Find("div.score").First().Text()),
		"comments": parseCount(doc.Find("a.comments, a.bylink").First().Text()),
		"selftext": strings.TrimSpace(doc.Find("div.usertext-body").First().Text()),
		"url":      url,
	}
}

func redditSubredditFromHTML(body []byte, url string) map[string]any {
	doc, derr := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if derr != nil {
		return map[string]any{"kind": "subreddit", "url": url}
	}
	title := strings.TrimSpace(doc.Find("title").First().Text())
	desc := strings.TrimSpace(doc.Find(".side .usertext-body").First().Text())
	if desc == "" {
		if c, ok := doc.Find("meta[name='description']").First().Attr("content"); ok {
			desc = strings.TrimSpace(c)
		}
	}
	return map[string]any{
		"kind":     "subreddit",
		"title":    title,
		"author":   "",
		"score":    parseCount(doc.Find(".subscribers .number, span.subscribers").First().Text()),
		"comments": float64(0),
		"selftext": desc,
		"url":      url,
	}
}

// redditThreadFromJSON parses the two-listing permalink .json shape:
// [0] = post listing, [1] = comments listing. Emits the flat post fields
// plus a nested comments tree (author/score/body/created/replies).
func redditThreadFromJSON(body []byte, url string) (map[string]any, error) {
	var listing []any
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("vertical: reddit .json: %w", err)
	}
	if len(listing) == 0 {
		return nil, fmt.Errorf("vertical: reddit .json: empty listing")
	}
	kids, _ := child(anyMap(listing[0]), "data")["children"].([]any)
	if len(kids) == 0 {
		return nil, fmt.Errorf("vertical: reddit .json: no children")
	}
	data, _ := kids[0].(map[string]any)
	post, _ := data["data"].(map[string]any)
	if post == nil {
		return nil, fmt.Errorf("vertical: reddit .json: bad child shape")
	}
	var comments []any
	if len(listing) > 1 {
		w := &redditCommentWalker{}
		comments = w.walk(listing[1], 0)
	}
	return map[string]any{
		"kind":     "post",
		"title":    str(post, "title"),
		"author":   str(post, "author"),
		"score":    num(post, "score"),
		"selftext": str(post, "selftext"),
		"comments": comments,
		"url":      url,
	}, nil
}

// Comment-tree caps. ponytail: depth 10 / total 200; deeper needs the
// `more`-object API pagination as the upgrade path.
const (
	redditMaxDepth    = 10
	redditMaxComments = 200
)

// redditCommentWalker flattens the recursive replies field into comment
// maps, tracking total count across all levels. Children that are not
// comments (the `more` placeholder) are skipped without error.
type redditCommentWalker struct{ total int }

func (w *redditCommentWalker) walk(listing any, depth int) []any {
	out := []any{}
	kids, _ := child(anyMap(listing), "data")["children"].([]any)
	for _, kid := range kids {
		if w.total >= redditMaxComments {
			return out
		}
		cm, _ := kid.(map[string]any)
		if kind, _ := cm["kind"].(string); kind != "t1" {
			continue
		}
		d := child(cm, "data")
		if d == nil {
			continue
		}
		w.total++
		c := map[string]any{
			"author":  str(d, "author"),
			"score":   num(d, "score"),
			"body":    str(d, "body"),
			"created": num(d, "created_utc"),
			"replies": []any{},
		}
		if depth+1 < redditMaxDepth {
			c["replies"] = w.walk(d["replies"], depth+1)
		}
		out = append(out, c)
	}
	return out
}

// parseCount pulls the first integer from label text ("42 comments" → 42,
// "12.3k" → 12300, "•" → 0); never errors.
func parseCount(s string) float64 {
	s = strings.ReplaceAll(strings.TrimSpace(strings.ToLower(s)), ",", "")
	if s == "" || s == "•" {
		return 0
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r != '.' && (r < '0' || r > '9') && r != 'k' && r != 'm'
	})
	for _, tok := range fields {
		if tok == "" || tok == "." {
			continue
		}
		mult := 1.0
		if strings.HasSuffix(tok, "k") {
			mult, tok = 1000, strings.TrimSuffix(tok, "k")
		} else if strings.HasSuffix(tok, "m") {
			mult, tok = 1000000, strings.TrimSuffix(tok, "m")
		}
		if f, err := strconv.ParseFloat(tok, 64); err == nil {
			return f * mult
		}
	}
	return 0
}

func matchHN(u *url.URL) bool {
	return hostIs(u, "news.ycombinator.com") && strings.Trim(u.Path, "/") == "item" && u.Query().Get("id") != ""
}

func extractHN(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	id := u.Query().Get("id")
	m, err := fetchJSON(ctx, f, "https://hn.algolia.com/api/v1/items/"+id)
	if err != nil {
		return nil, err
	}
	kind := "story"
	if _, hasParent := m["parent_id"]; hasParent && m["parent_id"] != nil {
		kind = "comment"
	}
	link := str(m, "url")
	if link == "" {
		link = "https://news.ycombinator.com/item?id=" + id
	}
	var comments float64
	if kids, _ := m["children"].([]any); kids != nil {
		comments = float64(len(kids)) // top-level only, no recursion
	}
	out := map[string]any{
		"kind":     kind,
		"title":    str(m, "title"),
		"url":      link,
		"points":   num(m, "points"),
		"author":   str(m, "author"),
		"comments": comments,
		"text":     str(m, "text"),
	}
	if kind == "comment" {
		out["parent_id"] = num(m, "parent_id")
	}
	return out, nil
}
