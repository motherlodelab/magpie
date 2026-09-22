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
			Name:     "substack_post",
			Label:    "Substack post",
			Desc:     "Post record from a Substack publication's public /api/v1/posts/{slug} endpoint (no key). Auto-fires on *.substack.com/p/{slug}.",
			Patterns: []string{"https://{publication}.substack.com/p/{slug}"},
		},
		Match:   matchSubstack,
		Extract: extractSubstack,
	})
}

// hostSuffix matches a URL host equal to suffix or ending in ".suffix".
// Single caller so far (substack) — promote to vertical.go on a second.
func hostSuffix(u *url.URL, suffix string) bool {
	h := strings.ToLower(u.Hostname())
	return h == suffix || strings.HasSuffix(h, "."+suffix)
}

func matchSubstack(u *url.URL) bool {
	segs := pathSegs(u.Path)
	return (u.Scheme == "http" || u.Scheme == "https") &&
		hostSuffix(u, "substack.com") &&
		len(segs) >= 2 && segs[0] == "p" && segs[1] != ""
}

func extractSubstack(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	slug := pathSegs(u.Path)[1]
	// Same-origin public API (no key) — works on custom domains too, so
	// --name substack_post can extract from any /p/{slug} host.
	apiURL := u.Scheme + "://" + u.Host + "/api/v1/posts/" + slug
	m, err := fetchJSON(ctx, f, apiURL)
	if err != nil {
		return nil, err
	}
	rec := map[string]any{"url": u.String()} // the page URL, never the API endpoint
	if v := str(m, "title"); v != "" {
		rec["title"] = v
	}
	if v := str(m, "subtitle"); v != "" {
		rec["subtitle"] = v
	}
	// Author rides as publishedBylines:[{name}] today; bylines/author are
	// the older API shapes — same string either way.
	for _, k := range []string{"publishedBylines", "bylines", "author"} {
		if arr, _ := m[k].([]any); len(arr) > 0 {
			if v := str(anyMap(arr[0]), "name"); v != "" {
				rec["author"] = v
				break
			}
		}
	}
	if v := str(m, "post_date"); v != "" { // raw passthrough, no date parsing
		rec["published"] = v
	}
	if n := num(m, "reaction_count", "like_count"); n != 0 { // reaction_count is the current shape
		rec["likes"] = n
	}
	if n := num(m, "comment_count"); n != 0 {
		rec["comments"] = n
	}
	if v := str(m, "cover_image"); v != "" {
		rec["cover_image"] = v
	}
	if v := str(m, "body_html"); v != "" {
		rec["text"] = stripTags(v)
	}
	if len(rec) == 1 { // url only — nothing decoded
		return nil, fmt.Errorf("vertical: substack_post: no post data at %s", u.String())
	}
	return rec, nil
}
