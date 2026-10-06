package research

import (
	"cmp"
	"net/url"
	"slices"
	"strings"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/scrape"
)

// DedupeQueries returns the candidates worth issuing, in their original
// text and order: those whose normalized form matches no issued query and
// no earlier candidate; empty-after-normalizing candidates drop.
// Normalized = lowercase, split on whitespace, trim .,;:!?"'()[] from each
// term's ends, drop empty terms, sort — "Best laptops 2026?" == "2026 best
// laptops"; "C# generics" != "C generics" (only term ends are trimmed).
// issued is passed in each time (no hidden state): a run issues ≤ ~100
// queries, so re-normalizing them costs nothing.
// ponytail: equal-after-normalizing only — "cheap laptops" vs "budget
// laptops" both pass. Upgrade: semantic (embedding) dedupe; measured gains
// are small and it adds a dependency ⌗.
func DedupeQueries(issued, candidates []string) []string {
	seen := make(map[string]bool, len(issued)+len(candidates))
	for _, q := range issued {
		seen[queryKey(q)] = true
	}
	out := []string{}
	for _, q := range candidates {
		k := queryKey(q)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, q)
	}
	return out
}

func queryKey(q string) string {
	var terms []string
	for _, f := range strings.Fields(strings.ToLower(q)) {
		if t := strings.Trim(f, `.,;:!?"'()[]`); t != "" {
			terms = append(terms, t)
		}
	}
	slices.Sort(terms)
	return strings.Join(terms, " ")
}

// ScoredURL is one rank-stage verdict; URL is canonical (crawl.Canonicalize),
// the key the loop's visited set and the snapshot pin share.
type ScoredURL struct {
	URL       string
	Score     float64
	Lists     int // result lists the URL appeared in — agreement
	Preferred bool
}

// lowValueSegs are path segments that rarely hold citable facts.
// ponytail: a fixed list; the upgrade is DR7 data on which reads produced
// zero facts.
var lowValueSegs = map[string]bool{
	"login": true, "signin": true, "sign-in": true, "signup": true, "sign-up": true,
	"register": true, "account": true, "cart": true, "checkout": true, "search": true,
	"tag": true, "tags": true, "category": true, "categories": true,
}

// actionSegs mark paths where a logged-in GET may act (log out, change a
// setting, pay). On a session host they — and lowValueSegs (login, account,
// cart, checkout, …) — are never read: read-only means no side effects,
// and a SERP is untrusted.
// ponytail: a fixed segment list; a side-effecting GET on an innocuous
// path, or one in the query string, gets through. Upgrade: a per-domain
// path allow-list.
var actionSegs = map[string]bool{"logout": true, "log-out": true, "signout": true, "sign-out": true,
	"settings": true, "preferences": true, "billing": true, "subscribe": true, "unsubscribe": true,
	"delete": true, "remove": true, "cancel": true, "password": true, "admin": true}

// RankURLs fuses search result lists (one per query × backend call, each in
// rank order) into one ranking, best first. Score = Σ 1/(60+rank) over the
// lists a URL appears in (reciprocal-rank fusion: agreement and position both
// lift), ×2 on a preferred domain, ×0.5 once on a low-value path (the root,
// or a login/cart/tag-style segment). Rank is the hit's index + 1 — Position
// is ignored, caller-built lists aren't trusted — and a URL repeated within
// one list counts once, at its best rank there.
//
// This is the egress gate (spec §6.3: in the rank stage, not the prompt).
// Dropped: URLs that don't canonicalize; every scheme but http/https — a
// SERP is untrusted and must never route a read to file:// (fetch reads it
// under MAGPIE_ALLOW_FILE=1), ftp:// or a scheme-relative //host; denied
// domains (deny beats allow and prefer); when Allow is set, every domain
// outside it; and on a session host (Sessions), every action or account
// path (actionSegs ∪ lowValueSegs) — a read there carries the user's login.
//
// Ties break on best rank, then URL. Visiting the top K and skipping
// already-visited URLs are the caller's. policy must come from
// Options.Normalized (bare, lowercase domains).
func RankURLs(lists [][]scrape.SearchHit, policy SourcePolicy) []ScoredURL {
	type acc struct {
		u           *url.URL
		rrf         float64
		lists, best int
	}
	byURL := map[string]*acc{}
	for _, list := range lists {
		inList := map[string]bool{}
		for i, h := range list {
			canon, err := crawl.Canonicalize(h.URL)
			if err != nil || inList[canon] { // index order: the first occurrence is the best rank
				continue
			}
			u, err := url.Parse(canon)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
				continue
			}
			inList[canon] = true
			rank := i + 1
			a := byURL[canon]
			if a == nil {
				a = &acc{u: u, best: rank}
				byURL[canon] = a
			}
			a.rrf += 1.0 / float64(60+rank)
			a.lists++
			a.best = min(a.best, rank)
		}
	}

	type ranked struct {
		ScoredURL
		best int
	}
	rs := make([]ranked, 0, len(byURL))
	for canon, a := range byURL {
		host := policyHost(a.u)
		if matchesAny(host, policy.Deny) || (len(policy.Allow) > 0 && !matchesAny(host, policy.Allow)) ||
			(matchesAny(host, policy.Sessions) && sessionUnsafe(a.u.Path)) {
			continue
		}
		r := ranked{ScoredURL{URL: canon, Score: a.rrf, Lists: a.lists}, a.best}
		if matchesAny(host, policy.Prefer) {
			r.Score *= 2
			r.Preferred = true
		}
		if lowValuePath(a.u.Path) {
			r.Score *= 0.5
		}
		rs = append(rs, r)
	}
	slices.SortFunc(rs, func(x, y ranked) int {
		return cmp.Or(cmp.Compare(y.Score, x.Score), cmp.Compare(x.best, y.best), strings.Compare(x.URL, y.URL))
	})
	out := make([]ScoredURL, len(rs))
	for i, r := range rs {
		out[i] = r.ScoredURL
	}
	return out
}

// matchesAny: host is one of domains or a subdomain of one (the
// fetch/http.go suffix pattern).
func matchesAny(host string, domains []string) bool {
	return slices.ContainsFunc(domains, func(d string) bool {
		return host == d || strings.HasSuffix(host, "."+d)
	})
}

// policyHost is the host the source policy matches: www. and a trailing
// dot trimmed — "example.com." names the same host, and without the trim
// it slips past a deny.
func policyHost(u *url.URL) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), ".")
}

// sessionOf is the session domain covering rawURL's host, or "".
func sessionOf(rawURL string, sessions []string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := policyHost(u)
	for _, d := range sessions {
		if matchesAny(host, []string{d}) {
			return d
		}
	}
	return ""
}

// sessionUnsafe: a path segment in actionSegs or lowValueSegs (the root is
// fine) — never read with a login.
func sessionUnsafe(p string) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), func(seg string) bool {
		seg = strings.ToLower(seg)
		return actionSegs[seg] || lowValueSegs[seg]
	})
}

func lowValuePath(p string) bool {
	if p == "/" {
		return true
	}
	return slices.ContainsFunc(strings.Split(p, "/"), func(seg string) bool {
		return lowValueSegs[strings.ToLower(seg)]
	})
}
