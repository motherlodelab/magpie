package research_test

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/research"
)

// valid is the smallest Options that normalizes.
func valid() research.Options {
	return research.Options{Provider: "openai", Model: "gpt-4o-mini", MaxCostUSD: 1}
}

func mustNorm(t *testing.T, o research.Options) research.Options {
	t.Helper()
	n, err := o.Normalized()
	if err != nil {
		t.Fatalf("Normalized(%+v): %v", o, err)
	}
	return n
}

func TestNormalized_Defaults(t *testing.T) {
	t.Parallel()
	n := mustNorm(t, valid())
	if n.Effort != "standard" || n.MaxToolCalls != 45 {
		t.Errorf(`"" effort = (%q, %d), want (standard, 45)`, n.Effort, n.MaxToolCalls)
	}
	q := valid()
	q.Effort = "quick"
	if n := mustNorm(t, q); n.MaxToolCalls != 10 {
		t.Errorf("quick tools = %d, want 10", n.MaxToolCalls)
	}
	q.MaxToolCalls = 7
	if n := mustNorm(t, q); n.MaxToolCalls != 7 {
		t.Errorf("explicit tools = %d, want 7 kept", n.MaxToolCalls)
	}

	for _, tc := range []struct {
		name                 string
		jp, jm               string
		wantProvider, wantMd string
	}{
		{"no judge = the writer's pair", "", "", "openai", "gpt-4o-mini"},
		{"judge model alone = writer's provider", "", "gpt-4o", "openai", "gpt-4o"},
		{"judge pair wins", "anthropic", "claude-sonnet-5", "anthropic", "claude-sonnet-5"},
	} {
		o := valid()
		o.JudgeProvider, o.JudgeModel = tc.jp, tc.jm
		p, m := research.JudgeOf(mustNorm(t, o))
		if p != tc.wantProvider || m != tc.wantMd {
			t.Errorf("%s: judge = (%q, %q), want (%q, %q)", tc.name, p, m, tc.wantProvider, tc.wantMd)
		}
	}

	s := valid()
	in := []string{"searxng", "duckduckgo", "searxng"}
	s.Search = in
	if n := mustNorm(t, s); !reflect.DeepEqual(n.Search, []string{"duckduckgo", "searxng"}) {
		t.Errorf("search = %v, want sorted and compacted [duckduckgo searxng]", n.Search)
	}
	if !reflect.DeepEqual(in, []string{"searxng", "duckduckgo", "searxng"}) {
		t.Errorf("Normalized touched the caller's search slice: %v", in)
	}
}

func TestNormalized_Rejects(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name  string
		edit  func(*research.Options)
		field string // the error must name it
	}{
		{"empty provider", func(o *research.Options) { o.Provider = "" }, "provider"},
		{"cost 0", func(o *research.Options) { o.MaxCostUSD = 0 }, "max_cost_usd"},
		{"cost -1", func(o *research.Options) { o.MaxCostUSD = -1 }, "max_cost_usd"},
		{"cost NaN", func(o *research.Options) { o.MaxCostUSD = math.NaN() }, "max_cost_usd"},
		{"cost +Inf", func(o *research.Options) { o.MaxCostUSD = math.Inf(1) }, "max_cost_usd"},
		{"tools -1", func(o *research.Options) { o.MaxToolCalls = -1 }, "max_tool_calls"},
		{"effort max", func(o *research.Options) { o.Effort = "max" }, "effort"},
		{"judge provider without model", func(o *research.Options) { o.JudgeProvider = "anthropic" }, "judge_model"},
		{"from after to", func(o *research.Options) { o.Sources.From, o.Sources.To = day(2), day(1) }, "sources.from"},
		{"allow empty", func(o *research.Options) { o.Sources.Allow = []string{""} }, "sources.allow"},
		{"allow no dot", func(o *research.Options) { o.Sources.Allow = []string{"nature"} }, "sources.allow"},
		{"deny space", func(o *research.Options) { o.Sources.Deny = []string{"a b.com"} }, "sources.deny"},
		{"deny userinfo", func(o *research.Options) { o.Sources.Deny = []string{"x.com@evil"} }, "sources.deny"},
		{"prefer fragment", func(o *research.Options) { o.Sources.Prefer = []string{"ok.com", "x.com#y"} }, "sources.prefer"},
		{"unknown provider", func(o *research.Options) { o.Provider = "gpt" }, "provider"},
		{"unknown judge provider", func(o *research.Options) { o.JudgeProvider, o.JudgeModel = "other", "m" }, "judge_provider"},
		{"search bing", func(o *research.Options) { o.Search = []string{"searxng", "bing"} }, "search"},
	} {
		o := valid()
		tc.edit(&o)
		n, err := o.Normalized()
		if err == nil {
			t.Errorf("%s: Normalized = nil error, want one naming %q", tc.name, tc.field)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.field)
		}
		if !reflect.DeepEqual(n, research.Options{}) {
			t.Errorf("%s: Normalized on error = %+v, want zero (no half-filled value to misuse)", tc.name, n)
		}
	}
}

func TestNormalized_Domains(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://www.Nature.com/articles?x": "nature.com",
		"*.bbc.co.uk":                       "bbc.co.uk",
		"Example.COM.":                      "example.com",
		"example.com:8080":                  "example.com",
	} {
		o := valid()
		o.Sources.Allow = []string{in}
		if got := mustNorm(t, o).Sources.Allow; !slices.Equal(got, []string{want}) {
			t.Errorf("normDomain(%q) = %q, want %q", in, got, want)
		}
	}

	o := valid()
	o.Sources.Deny = []string{"www.nature.com", "Nature.com", "bbc.co.uk"}
	before := slices.Clone(o.Sources.Deny)
	n := mustNorm(t, o)
	if want := []string{"bbc.co.uk", "nature.com"}; !slices.Equal(n.Sources.Deny, want) {
		t.Errorf("deny = %q, want %q (sorted, duplicates collapsed)", n.Sources.Deny, want)
	}
	if !slices.Equal(o.Sources.Deny, before) {
		t.Errorf("caller's slice mutated: %q, was %q", o.Sources.Deny, before)
	}
}

func TestOptions_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(research.SourcePolicy{})
	if err != nil || string(b) != "{}" {
		t.Errorf("zero SourcePolicy = %s (%v), want {}", b, err)
	}

	ict := time.FixedZone("ICT", 7*3600)
	o := valid()
	o.JudgeProvider, o.JudgeModel = "anthropic", "claude-sonnet-5"
	o.Sources = research.SourcePolicy{
		Allow: []string{"nature.com"}, Deny: []string{"ads.example.com"}, Prefer: []string{"arxiv.org"},
		Sessions: []string{"nature.com"},
		From:     time.Date(2025, 6, 1, 12, 0, 0, 0, ict), // non-UTC: DeepEqual on it would fail (loc), .Equal holds
		To:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	o.Search = []string{"brave", "duckduckgo"}
	n := mustNorm(t, o)
	b, err = json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"from":`) {
		t.Errorf("a set From was dropped: %s", b)
	}
	var back research.Options
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Sources.From.Equal(n.Sources.From) || !back.Sources.To.Equal(n.Sources.To) {
		t.Errorf("times = (%v, %v), want (%v, %v)", back.Sources.From, back.Sources.To, n.Sources.From, n.Sources.To)
	}
	// DeepEqual on the time-less rest only: a round-tripped time.Time keeps
	// its instant, not its *Location.
	back.Sources.From, back.Sources.To = n.Sources.From, n.Sources.To
	if !reflect.DeepEqual(back, n) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back, n)
	}
}

func TestPlan_Validate(t *testing.T) {
	t.Parallel()
	standard := mustNorm(t, valid())
	q := valid()
	q.Effort = "quick"
	quick := mustNorm(t, q)
	angles := func(n int) []research.Angle {
		out := make([]research.Angle, n)
		for i := range out {
			out[i] = research.Angle{Question: "q", Queries: []string{"seed"}}
		}
		return out
	}

	if err := (research.Plan{Brief: "b", Angles: angles(3)}).Validate(standard); err != nil {
		t.Errorf("3 angles on standard: %v, want ok (the inclusive bound)", err)
	}
	for _, tc := range []struct {
		name string
		p    research.Plan
		o    research.Options
		want string
	}{
		{"empty brief", research.Plan{Brief: "  ", Angles: angles(1)}, standard, "brief"},
		{"no angles", research.Plan{Brief: "b"}, standard, "angles"},
		{"4 on standard", research.Plan{Brief: "b", Angles: angles(4)}, standard, "angles"},
		{"2 on quick", research.Plan{Brief: "b", Angles: angles(2)}, quick, "angles"},
		{"blank question", research.Plan{Brief: "b", Angles: append(angles(1), research.Angle{Question: " "})}, standard, "angles[1]"},
	} {
		err := tc.p.Validate(tc.o)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

// TestNormalized_Sessions: session domains canonicalize like allow/deny/
// prefer, and one the policy can never read is refused before any spend.
func TestNormalized_Sessions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  research.SourcePolicy
		want []string // nil = an error naming errSub
		err  []string
	}{
		{"canonical", research.SourcePolicy{Sessions: []string{"https://www.FT.com/x"}}, []string{"ft.com"}, nil},
		{"duplicates", research.SourcePolicy{Sessions: []string{"ft.com", "FT.com."}}, []string{"ft.com"}, nil},
		{"denied", research.SourcePolicy{Deny: []string{"ft.com"}, Sessions: []string{"ft.com"}}, nil, []string{`sources.sessions: "ft.com"`, "never reads it"}},
		{"denied parent", research.SourcePolicy{Deny: []string{"ft.com"}, Sessions: []string{"markets.ft.com"}}, nil, []string{`sources.sessions: "markets.ft.com"`}},
		{"outside allow", research.SourcePolicy{Allow: []string{"nature.com"}, Sessions: []string{"ft.com"}}, nil, []string{`sources.sessions: "ft.com"`}},
		{"inside allow", research.SourcePolicy{Allow: []string{"ft.com"}, Sessions: []string{"markets.ft.com"}}, []string{"markets.ft.com"}, nil},
		{"allow narrower", research.SourcePolicy{Allow: []string{"markets.ft.com"}, Sessions: []string{"ft.com"}}, []string{"ft.com"}, nil},
		{"garbage", research.SourcePolicy{Sessions: []string{"foo"}}, nil, []string{`sources.sessions: "foo": not a domain`}},
	} {
		o := valid()
		o.Sources = tc.src
		before := slices.Clone(o.Sources.Sessions)
		n, err := o.Normalized()
		switch {
		case tc.err != nil:
			for _, sub := range tc.err {
				if err == nil || !strings.Contains(err.Error(), sub) {
					t.Errorf("%s: err = %v, want one containing %q", tc.name, err, sub)
				}
			}
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case !slices.Equal(n.Sources.Sessions, tc.want):
			t.Errorf("%s: sessions = %q, want %q", tc.name, n.Sources.Sessions, tc.want)
		}
		if !slices.Equal(o.Sources.Sessions, before) {
			t.Errorf("%s: caller's slice mutated: %q, was %q", tc.name, o.Sources.Sessions, before)
		}
	}

	o := valid()
	o.Sources.Sessions = []string{"ft.com"}
	b, err := json.Marshal(mustNorm(t, o))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"sources":{"sessions":["ft.com"]}`) || strings.Contains(s, "session_domains") || strings.Contains(s, `"web"`) {
		t.Errorf("marshalled = %s, want sessions nested under sources and no session_domains/web key", s)
	}
	var legacy research.Options // a pre-DR6 row: the web key is gone, not refused
	if err := json.Unmarshal([]byte(`{"provider":"openai","max_cost_usd":1,"sources":{"web":false},"session_domains":["x.com"]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Normalized(); err != nil {
		t.Errorf("legacy row: %v", err)
	}
}
