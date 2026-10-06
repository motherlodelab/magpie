package research_test

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/research"
	"github.com/motherlodelab/magpie/scrape"
)

func hit(u string) scrape.SearchHit { return scrape.SearchHit{URL: u} }

func list(urls ...string) []scrape.SearchHit {
	out := make([]scrape.SearchHit, len(urls))
	for i, u := range urls {
		out[i] = hit(u)
	}
	return out
}

// fillers returns n distinct neutral URLs on host.
func fillers(host string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("https://%s/page/%d", host, i+1)
	}
	return out
}

func urlsOf(got []research.ScoredURL) []string {
	out := make([]string, len(got))
	for i, s := range got {
		out[i] = s.URL
	}
	return out
}

func find(t *testing.T, got []research.ScoredURL, u string) (int, research.ScoredURL) {
	t.Helper()
	for i, s := range got {
		if s.URL == u {
			return i, s
		}
	}
	t.Fatalf("%q missing from %q", u, urlsOf(got))
	return -1, research.ScoredURL{}
}

func TestDedupeQueries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		issued, candidates []string
		want               []string
	}{
		{"reordered, cased, punctuated", []string{"2026 best LAPTOPS"}, []string{"Best laptops 2026?"}, []string{}},
		{"only term ends trim", nil, []string{"C# generics", "C generics"}, []string{"C# generics", "C generics"}},
		{"empty after normalizing", nil, []string{"   ", "?!", "go"}, []string{"go"}},
		{"within-candidates duplicate keeps the first", nil, []string{"Go generics", "generics go", "rust"}, []string{"Go generics", "rust"}},
		{"original text and order", []string{"a b"}, []string{"Zeta!", "b a", "  Alpha  "}, []string{"Zeta!", "  Alpha  "}},
		{"nil issued", nil, []string{"x"}, []string{"x"}},
		{"no candidates", []string{"x"}, nil, []string{}},
	} {
		got := research.DedupeQueries(tc.issued, tc.candidates)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: DedupeQueries(%q, %q) = %q, want %q", tc.name, tc.issued, tc.candidates, got, tc.want)
		}
	}
}

// TestRankURLs_Agreement: rank 5 in 3 lists (3/65) beats rank 1 in 1 list
// (1/61). Positions are deliberately wrong — the list index is the rank —
// and a repeat within one list counts once.
func TestRankURLs_Agreement(t *testing.T) {
	t.Parallel()
	const agreed, solo = "https://agreed.com/a", "https://solo.com/a"
	l1 := list(append(append([]string{solo}, fillers("f1.com", 3)...), agreed, agreed)...)
	l2 := list(append(fillers("f2.com", 4), agreed)...)
	l3 := list(append(fillers("f3.com", 4), agreed)...)
	for i := range l1 {
		l1[i].Position = 1 // stale: must be ignored
	}
	got := research.RankURLs([][]scrape.SearchHit{l1, l2, l3}, research.SourcePolicy{})

	if got[0].URL != agreed {
		t.Fatalf("top = %q, want %q (order %q)", got[0].URL, agreed, urlsOf(got))
	}
	if got[0].Lists != 3 {
		t.Errorf("Lists = %d, want 3", got[0].Lists)
	}
	if want := 3.0 / 65; math.Abs(got[0].Score-want) > 1e-12 {
		t.Errorf("Score = %v, want %v (the in-list repeat must not count)", got[0].Score, want)
	}
	if _, s := find(t, got, solo); math.Abs(s.Score-1.0/61) > 1e-12 || s.Lists != 1 {
		t.Errorf("solo = %+v, want Score 1/61, Lists 1", s)
	}
}

func TestRankURLs_Canonical(t *testing.T) {
	t.Parallel()
	got := research.RankURLs([][]scrape.SearchHit{
		list("https://Example.com/a?utm_source=x"),
		list("https://example.com/a"),
	}, research.SourcePolicy{})
	if len(got) != 1 || got[0].URL != "https://example.com/a" || got[0].Lists != 2 {
		t.Errorf("got %+v, want one canonical https://example.com/a with Lists 2", got)
	}
}

func TestRankURLs_Policy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy research.SourcePolicy
		in     []string // one list, in rank order
		want   []string // URLs in output order
	}{
		{"deny covers subdomains and www",
			research.SourcePolicy{Deny: []string{"example.com"}},
			[]string{"https://example.com/p", "https://sub.example.com/p", "https://www.example.com/p", "https://other.com/p"},
			[]string{"https://other.com/p"}},
		{"deny beats allow and prefer",
			research.SourcePolicy{Allow: []string{"example.com"}, Prefer: []string{"ads.example.com"}, Deny: []string{"ads.example.com"}},
			[]string{"https://ads.example.com/p", "https://example.com/p"},
			[]string{"https://example.com/p"}},
		{"allow restricts (a suffix match is a label match)",
			research.SourcePolicy{Allow: []string{"nature.com"}},
			[]string{"https://evil.com/x", "https://notnature.com/x", "https://www.nature.com/x", "https://nature.com.evil.com/x"},
			[]string{"https://www.nature.com/x"}},
		{"prefer x2 lifts rank 3 (2/63) over rank 1 (1/61)",
			research.SourcePolicy{Prefer: []string{"pref.com"}},
			[]string{"https://neutral.com/a", "https://filler.com/a", "https://pref.com/a"},
			[]string{"https://pref.com/a", "https://neutral.com/a", "https://filler.com/a"}},
		{"a trailing-dot host is the same host",
			research.SourcePolicy{Deny: []string{"example.com"}},
			[]string{"https://example.com./p", "https://other.com/p"},
			[]string{"https://other.com/p"}},
	} {
		got := research.RankURLs([][]scrape.SearchHit{list(tc.in...)}, tc.policy)
		if !slices.Equal(urlsOf(got), tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, urlsOf(got), tc.want)
		}
		for _, s := range got {
			if want := slices.Contains(tc.policy.Prefer, "pref.com") && s.URL == "https://pref.com/a"; s.Preferred != want {
				t.Errorf("%s: %q Preferred = %v, want %v", tc.name, s.URL, s.Preferred, want)
			}
		}
	}
}

// TestRankURLs_Schemes is the egress gate's first rule: a SERP hit never
// routes a read anywhere but http(s).
func TestRankURLs_Schemes(t *testing.T) {
	t.Parallel()
	got := research.RankURLs([][]scrape.SearchHit{list(
		"file:///etc/passwd",
		"file://localhost/etc/passwd",
		"ftp://x.com/a",
		"//x.com/a",
		"javascript:alert(1)",
		"mailto:a@b.com",
		"https://ok.com/a",
		"http://ok.com/b",
	)}, research.SourcePolicy{})
	if want := []string{"https://ok.com/a", "http://ok.com/b"}; !slices.Equal(urlsOf(got), want) {
		t.Errorf("got %q, want only %q", urlsOf(got), want)
	}
}

// TestRankURLs_PathPenalty: at equal RRF the low-value paths rank last, and
// prefer's ×2 cancels the root penalty's ×0.5 EXACTLY (powers of two are
// lossless in floats), so root+preferred ties a clean neutral URL.
func TestRankURLs_PathPenalty(t *testing.T) {
	t.Parallel()
	pol := research.SourcePolicy{Prefer: []string{"penalized.com"}}
	got := research.RankURLs([][]scrape.SearchHit{ // each URL rank 1 in its own list: equal RRF
		list("https://plain.com/tag/go"),
		list("https://plain.com/login"),
		list("https://plain.com/"),
		list("https://plain.com/blog/post"),
		list("https://penalized.com/"), // root ×0.5, preferred ×2 → ×1 net
	}, pol)

	want := []string{
		"https://penalized.com/", "https://plain.com/blog/post", // 1/61, tie broken by URL
		"https://plain.com/", "https://plain.com/login", "https://plain.com/tag/go", // ×0.5
	}
	if !slices.Equal(urlsOf(got), want) {
		t.Errorf("order = %q, want %q", urlsOf(got), want)
	}
	_, root := find(t, got, "https://penalized.com/")
	_, clean := find(t, got, "https://plain.com/blog/post")
	if root.Score != clean.Score { // ==, not ≈: ×2 and ×0.5 are exact
		t.Errorf("root+prefer score %v != neutral %v — penalty not exactly cancelled", root.Score, clean.Score)
	}
	if _, login := find(t, got, "https://plain.com/login"); login.Score != clean.Score/2 {
		t.Errorf("login score %v, want exactly half of %v", login.Score, clean.Score)
	}
}

// TestRankURLs_Deterministic: equal scores order by best rank, then URL.
// z.com's root at rank 1 (½·1/61) ties a.com at rank 62 (1/122) exactly,
// and the better rank wins despite the later URL.
func TestRankURLs_Deterministic(t *testing.T) {
	t.Parallel()
	in := [][]scrape.SearchHit{
		list("https://z.com/"),
		list(append(fillers("f.com", 61), "https://a.com/p")...),
		list("https://d.com/p"),
		list("https://c.com/p"),
	}
	got := research.RankURLs(in, research.SourcePolicy{})
	iz, z := find(t, got, "https://z.com/")
	ia, a := find(t, got, "https://a.com/p")
	if z.Score != a.Score {
		t.Fatalf("fixture broken: scores %v vs %v must tie", z.Score, a.Score)
	}
	if iz > ia {
		t.Errorf("tie: z.com (best rank 1) at %d after a.com (best rank 62) at %d", iz, ia)
	}
	ic, _ := find(t, got, "https://c.com/p")
	id, _ := find(t, got, "https://d.com/p")
	if ic > id {
		t.Errorf("tie on score and rank: c.com at %d after d.com at %d, want URL order", ic, id)
	}
	for range 10 {
		if again := research.RankURLs(in, research.SourcePolicy{}); !reflect.DeepEqual(again, got) {
			t.Fatalf("RankURLs not deterministic:\n%q\nvs\n%q", urlsOf(again), urlsOf(got))
		}
	}
	if got := research.RankURLs(nil, research.SourcePolicy{}); len(got) != 0 {
		t.Errorf("RankURLs(nil) = %+v, want empty", got)
	}
	if got := research.RankURLs([][]scrape.SearchHit{{}, nil}, research.SourcePolicy{}); len(got) != 0 {
		t.Errorf("RankURLs(empty lists) = %+v, want empty", got)
	}
}

// TestRankURLs_SessionActionPaths: on a session host an action or account
// path is never read — the read would carry the login. Public hosts keep
// today's scoring: logout untouched, a lowValueSegs path ×0.5.
func TestRankURLs_SessionActionPaths(t *testing.T) {
	t.Parallel()
	var dropped []string
	for _, p := range []string{"logout", "log-out", "signout", "settings/email", "billing", "delete?id=1",
		"account", "cart", "checkout", "search?q=x", "tag/x", "category/y", "LOGOUT"} {
		dropped = append(dropped, "https://paper.example/"+p)
	}
	dropped = append(dropped, "https://www.paper.example/logout", "https://news.paper.example/logout")
	kept := []string{"https://paper.example/articles/1", "https://paper.example/"}
	got := research.RankURLs([][]scrape.SearchHit{
		list(append(slices.Clone(dropped), kept...)...),
		list("https://pub.example/logout"),
		list("https://pub.example/account"),
		list("https://pub.example/news"),
	}, research.SourcePolicy{Sessions: []string{"paper.example"}})

	var want []string
	for _, u := range append(kept, "https://pub.example/logout", "https://pub.example/account", "https://pub.example/news") {
		c, err := crawl.Canonicalize(u)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, c)
	}
	if g := urlsOf(got); !slices.Equal(sorted(g), sorted(want)) {
		t.Errorf("kept = %q, want exactly %q", g, want)
	}
	_, out := find(t, got, want[2])
	_, acct := find(t, got, want[3])
	_, news := find(t, got, want[4])
	if out.Score != news.Score || acct.Score != news.Score*0.5 {
		t.Errorf("public scores: logout %v, account %v, news %v — want logout = news, account = news/2 (unchanged)", out.Score, acct.Score, news.Score)
	}
}

func sorted(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }
