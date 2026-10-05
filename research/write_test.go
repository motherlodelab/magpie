package research_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/research"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 5, 12, 0, 0, 123456789, time.UTC)
	usable := []research.Fact{
		{FactID: "f1", URL: "https://a.example/1", Quote: "Alpha rose 12 percent", CheckedAt: at, Status: "verified"},
		{FactID: "f2", URL: "https://b.example/2", Quote: "Beta fell", CheckedAt: at, Status: "verified"},
		{FactID: "f3", URL: "https://c.example/3", Quote: "Gamma may rise", CheckedAt: at, Status: "softened", Note: "Gamma may rise"},
		{FactID: "f5", URL: "https://d.example/5", Quote: "Delta held", CheckedAt: at, Status: "contested"},
	}
	for _, tc := range []struct {
		name, in       string
		has, lacks     []string
		cited, dropped []string
	}{
		{"one citation", "Alpha rose [f1].", []string{"Alpha rose [^1].", "[^1]: https://a.example/1 — \"Alpha rose 12 percent\" (checked 2026-10-05T12:00:00.123456789Z)"}, nil,
			[]string{"f1"}, nil},
		{"first-cite numbering", "B [f2][f1]. A again [f1].", []string{"B [^1][^2].", "A again [^2].", "[^1]: https://b.example/2", "[^2]: https://a.example/1"}, nil,
			[]string{"f2", "f1"}, nil},
		{"comma list", "Both [f1, f3].", []string{"Both [^1][^2]."}, nil, []string{"f1", "f3"}, nil},
		{"unknown and unusable dropped", "X [f4][f99][f5].", []string{"X [^1].", "[^1]: https://d.example/5"}, []string{"f4", "f99"},
			[]string{"f5"}, []string{"f4", "f99"}},
		{"link to text", "See [the survey](https://evil.example/x) [f1].", []string{"See the survey [^1]."}, []string{"evil.example"}, []string{"f1"}, nil},
		{"bare URL removed", "Read https://evil.example/y and www.evil.example/z now [f1].", []string{"Read  and  now [^1]."}, []string{"evil.example"}, []string{"f1"}, nil},
		{"no citations, no sources", "Nothing cited here.", []string{"Nothing cited here."}, []string{"## Sources"}, nil, nil},
	} {
		md, cited, dropped := research.Resolve(tc.in, usable)
		for _, s := range tc.has {
			if !strings.Contains(md, s) {
				t.Errorf("%s: output lacks %q:\n%s", tc.name, s, md)
			}
		}
		for _, s := range tc.lacks {
			if strings.Contains(md, s) {
				t.Errorf("%s: output has %q:\n%s", tc.name, s, md)
			}
		}
		if !slices.Equal(cited, tc.cited) || !slices.Equal(dropped, tc.dropped) {
			t.Errorf("%s: cited %v dropped %v, want %v / %v", tc.name, cited, dropped, tc.cited, tc.dropped)
		}
		again, _, _ := research.Resolve(tc.in, usable)
		if again != md {
			t.Errorf("%s: not deterministic:\n%s\nvs\n%s", tc.name, md, again)
		}
		// The only URLs in a report are its cited facts' (US-1).
		for _, u := range regexp.MustCompile(`https?://[^\s"]+`).FindAllString(md, -1) {
			if !slices.ContainsFunc(usable, func(f research.Fact) bool { return f.URL == u && slices.Contains(cited, f.FactID) }) {
				t.Errorf("%s: foreign URL %q in output:\n%s", tc.name, u, md)
			}
		}
	}
}
