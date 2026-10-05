package research_test

import (
	"strings"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/research"
)

func TestQuoteFound(t *testing.T) {
	t.Parallel()
	page := "The **alpha index** rose 12 percent in 2025 — according to the [annual survey](https://a.example/s).\n\n" +
		"Costs | rose | sharply in every region\n\nShe said “prices won’t fall”\u00a0soon… maybe\n\nA \\*literal\\* asterisk line."
	for _, tc := range []struct {
		name, quote string
		want        bool
	}{
		{"exact", "The alpha index rose 12 percent in 2025", true},
		{"case and spaces", "the  ALPHA index\nrose 12 percent in 2025", true},
		{"dash folded", "in 2025 - according to the annual survey", true},
		{"link text", "according to the annual survey", true},
		{"table pipe", "Costs rose sharply in every region", true},
		{"curly vs straight", `She said "prices won't fall" soon`, true},
		{"nbsp and ellipsis", "prices won't fall\" soon... maybe", true},
		{"escapes", "A *literal* asterisk line", true},
		{"absent", "The beta index fell 3 percent in 2024", false},
		{"too short", "rose 12 percent", false}, // < 20 runes after folding matches anywhere
		{"too long", strings.Repeat("alpha ", 101), false},
		// Folds are symmetric: a quote that matches only through a change the
		// page doesn't have (a dash where the page has none) stays unfound.
		{"no false fold", "The alpha index - rose 12 percent", false},
	} {
		if got := research.QuoteFound(page, tc.quote); got != tc.want {
			t.Errorf("%s: QuoteFound(%q) = %v, want %v", tc.name, tc.quote, got, tc.want)
		}
	}
}

func TestInRange(t *testing.T) {
	t.Parallel()
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		published string
		from, to  time.Time
		want      bool
	}{
		{"", from, to, true},
		{"garbage", from, to, true},
		{"2024-06-01", from, to, true},
		{"2023-12-31", from, to, false},
		{"2024-12-31T23:59:00Z", from, to, true}, // the To day itself
		{"2025-01-01", from, to, false},
		{"1999-01-01", time.Time{}, to, true}, // zero bounds are open
		{"2099-01-01", from, time.Time{}, true},
	} {
		if got := research.InRange(tc.published, tc.from, tc.to); got != tc.want {
			t.Errorf("InRange(%q, %v, %v) = %v, want %v", tc.published, tc.from, tc.to, got, tc.want)
		}
	}
}

func TestSettle(t *testing.T) {
	t.Parallel()
	base := func(ev ...research.Stance) research.Pivot {
		return research.Pivot{ID: "f1", Claim: "Alpha rose", Domain: "a.example", Status: "verified", Evidence: ev}
	}
	for _, tc := range []struct {
		name      string
		p         research.Pivot
		status    string
		noteHas   []string
		noteExact string
	}{
		{"contradicted", base(research.Stance{"contradicts", "b.example", []string{"f9"}}, research.Stance{"supports", "c.example", []string{"f7"}}),
			"contested", []string{"f9", "b.example"}, ""},
		{"stance without ids is ignored", base(research.Stance{"contradicts", "b.example", nil}),
			"softened", nil, "Per a.example: Alpha rose"},
		{"same registrable domain doesn't corroborate", base(research.Stance{"supports", "a.example", []string{"f4"}}),
			"softened", nil, "Per a.example: Alpha rose"},
		{"other domain corroborates", base(research.Stance{"supports", "c.example", []string{"f7"}}),
			"verified", []string{"corroborated by f7 (c.example)"}, ""},
		{"softened keeps its note when corroborated",
			research.Pivot{ID: "f1", Claim: "Alpha rose", Domain: "a.example", Status: "softened", Note: "Alpha may have risen",
				Evidence: []research.Stance{{"supports", "c.example", []string{"f7"}}}},
			"softened", nil, "Alpha may have risen"},
		{"softened without support",
			research.Pivot{ID: "f1", Claim: "Alpha rose", Domain: "a.example", Status: "softened", Note: "Alpha may have risen"},
			"softened", nil, "Per a.example: Alpha may have risen"},
	} {
		status, note := research.Settle(tc.p)
		if status != tc.status {
			t.Errorf("%s: status = %q, want %q", tc.name, status, tc.status)
		}
		if tc.noteExact != "" && note != tc.noteExact {
			t.Errorf("%s: note = %q, want %q", tc.name, note, tc.noteExact)
		}
		for _, s := range tc.noteHas {
			if !strings.Contains(note, s) {
				t.Errorf("%s: note %q lacks %q", tc.name, note, s)
			}
		}
	}
}

func TestRegDomain(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"www.bbc.co.uk":      "bbc.co.uk",
		"news.alpha.example": "alpha.example",
		"a.example":          "a.example",
		"127.0.0.1":          "127.0.0.1",
		"localhost":          "localhost",
	} {
		if got := research.RegDomain(in); got != want {
			t.Errorf("RegDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
