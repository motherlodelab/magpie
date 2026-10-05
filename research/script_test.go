package research_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/research"
)

// The e2e corpus (plan Task 7's table). Every key is an absolute https://
// URL: RankURLs's egress gate silently drops anything else.
const (
	uAlpha   = "https://alpha.example/report"
	uBeta    = "https://beta.example/analysis"
	uDelta   = "https://delta.example/post"
	uGamma   = "https://gamma.example/rebuttal"
	uZeta    = "https://zeta.example/partial"
	uEpsilon = "https://epsilon.example/old"
	uLogin   = "https://login.example/members"
)

// Quotes are verbatim from the pages, except qM (a misquote the page lacks).
const (
	qA = "The alpha index rose 12 percent in 2025"
	qM = "The alpha index doubled in a single quarter"
	qB = "analysts confirmed the alpha index gained about twelve percent"
	qP = "its new battery lasts ten years under normal use"
	qG = "Lab tests found the Delta battery degraded within four years"
	qZ = "the alpha rise may continue into 2026 if demand holds"
	qO = "A 2019 report said delta batteries lasted only two years"

	cA        = "The alpha index rose 12% in 2025."
	cZ        = "The alpha rise will continue into 2026."
	cZSoft    = "The alpha rise may continue into 2026 if demand holds."
	angle1Q   = "How did the alpha index move in 2025?"
	angle2Q   = "What do critics say about the Delta battery?"
	e2eAsk    = "What happened to the alpha index and the Delta battery?"
	gapQuery  = "alpha follow-up"
	evilWrite = "Summary [%s][f99][%s]. See [here](https://evil.example/x) and https://evil.example/y."
)

func e2ePages(t *testing.T) map[string]page {
	t.Helper()
	ok := func(title string, paras ...string) page { return page{200, pad(t, title, paras...)} }
	return map[string]page{
		uAlpha:   ok("Alpha report", "The alpha index rose 12 percent in 2025 across all surveyed regions, the annual report found."),
		uBeta:    ok("Beta analysis", "Independent analysts confirmed the alpha index gained about twelve percent last year, citing regional data."),
		uDelta:   ok("Delta post", "Delta Corp says its new battery lasts ten years under normal use, according to the launch notes."),
		uGamma:   ok("Gamma rebuttal", "Lab tests found the Delta battery degraded within four years, contradicting the launch claims."),
		uZeta:    ok("Zeta partial", "Officials suggested the alpha rise may continue into 2026 if demand holds through the winter."),
		uEpsilon: ok("Epsilon old", "A 2019 report said delta batteries lasted only two years before needing a replacement."),
		uLogin:   {401, []byte(loginWall)},
	}
}

var e2eSerp = map[string][]string{
	"alpha index 2025":     {uAlpha},
	"alpha check":          {uBeta},
	gapQuery:               {uZeta},
	"alpha index regions":  nil,
	"delta battery claims": {uDelta, uLogin},
	"delta rebuttal":       {uGamma},
	"delta older reports":  {uEpsilon},
}

var angle1Queries = []string{"alpha index 2025", "alpha check", gapQuery, "alpha index regions"}

type xfact struct {
	Claim        string  `json:"claim"`
	Quote        string  `json:"quote"`
	Published    string  `json:"published"`
	Confidence   float64 `json:"confidence"`
	Pivotal      bool    `json:"pivotal"`
	CounterQuery string  `json:"counter_query"`
}

func extractJSON(facts []xfact, gaps []string, stance string) string {
	if facts == nil {
		facts = []xfact{}
	}
	if gaps == nil {
		gaps = []string{}
	}
	b, _ := json.Marshal(map[string]any{"facts": facts, "gaps": gaps, "stance": stance}) //nolint:errcheck // static shapes
	return string(b)
}

func planJSON(angles ...research.Angle) string {
	b, _ := json.Marshal(map[string]any{"brief": "Cover the alpha index and the Delta battery claims.", "angles": angles}) //nolint:errcheck // static shapes
	return string(b)
}

// sourceOf is the "Source: <url> — <title>" line's URL.
func sourceOf(material string) string {
	for _, l := range strings.Split(material, "\n") {
		if u, ok := strings.CutPrefix(l, "Source: "); ok {
			u, _, _ = strings.Cut(u, " — ")
			return u
		}
	}
	return ""
}

var factLine = regexp.MustCompile(`(?m)^\[(f\d+)\] (.*)$`)

// idOf finds a fact id in the writer's material by a claim substring.
func idOf(material, claim string) string {
	for _, m := range factLine.FindAllStringSubmatch(material, -1) {
		if strings.Contains(m[2], claim) {
			return m[1]
		}
	}
	return ""
}

// unusableID is the lowest fact id the writer wasn't given: a fact the
// verifier dropped.
func unusableID(material string) string {
	for i := 1; ; i++ {
		if id := fmt.Sprintf("f%d", i); !strings.Contains(material, "["+id+"]") {
			return id
		}
	}
}

func judgeJSON(material string, verdict func(claim string) (string, string)) string {
	type v struct {
		ID      string `json:"id"`
		Verdict string `json:"verdict"`
		Claim   string `json:"claim"`
		Reason  string `json:"reason"`
	}
	out := []v{}
	for _, l := range strings.Split(material, "\n") {
		parts := strings.SplitN(l, " | ", 3)
		if len(parts) != 3 {
			continue
		}
		verd, claim := verdict(parts[1])
		out = append(out, v{parts[0], verd, claim, "scripted"})
	}
	b, _ := json.Marshal(map[string]any{"verdicts": out}) //nolint:errcheck // static shapes
	return string(b)
}

const replanDone = `{"done":true,"reason":"covered","angles":[]}`

// defaultScript is the e2e model: plan Task 7's table, verdict by verdict.
func defaultScript(task, material string) (string, error) {
	switch task {
	case "plan":
		return planJSON(
			research.Angle{Question: angle1Q, Queries: []string{"alpha index 2025", "alpha index regions"}},
			research.Angle{Question: angle2Q, Queries: []string{"delta battery claims", "delta older reports"}},
		), nil
	case "extract":
		target := strings.Contains(material, "Target claim to check:")
		switch sourceOf(material) {
		case uAlpha:
			return extractJSON([]xfact{
				{Claim: cA, Quote: qA, Published: "2025-12-01", Confidence: 0.9, Pivotal: true, CounterQuery: "alpha check"},
				{Claim: "The alpha index doubled in one quarter.", Quote: qM, Confidence: 0.4},
			}, []string{gapQuery}, "none"), nil
		case uBeta:
			stance := "none"
			if target {
				stance = "supports"
			}
			return extractJSON([]xfact{{Claim: "Analysts confirmed the alpha index gained about 12%.", Quote: qB, Confidence: 0.8}}, nil, stance), nil
		case uDelta:
			return extractJSON([]xfact{{Claim: "The Delta battery lasts ten years.", Quote: qP, Confidence: 0.7, Pivotal: true, CounterQuery: "delta rebuttal"}}, nil, "none"), nil
		case uGamma:
			stance := "none"
			if target {
				stance = "contradicts"
			}
			return extractJSON([]xfact{{Claim: "Lab tests found the Delta battery degraded within four years.", Quote: qG, Confidence: 0.8}}, nil, stance), nil
		case uZeta:
			return extractJSON([]xfact{{Claim: cZ, Quote: qZ, Confidence: 0.5}}, nil, "none"), nil
		case uEpsilon:
			return extractJSON([]xfact{{Claim: "Delta batteries lasted two years in 2019.", Quote: qO, Published: "2019-05-01", Confidence: 0.6}}, nil, "none"), nil
		}
		return extractJSON(nil, nil, "none"), nil
	case "judge":
		return judgeJSON(material, func(claim string) (string, string) {
			if claim == cZ {
				return "partial", cZSoft
			}
			return "supported", ""
		}), nil
	case "replan":
		return replanDone, nil
	case "write":
		return fmt.Sprintf(evilWrite, idOf(material, cA), unusableID(material)), nil
	}
	return "", fmt.Errorf("fake: no script for task %q", task)
}

// factBy is the one fact at url whose claim contains claimSub. Fact ids are
// nondeterministic across sub-researchers: look up, never index.
func factBy(t *testing.T, facts []research.Fact, url, claimSub string) research.Fact {
	t.Helper()
	var out []research.Fact
	for _, f := range facts {
		if f.URL == url && strings.Contains(f.Claim, claimSub) {
			out = append(out, f)
		}
	}
	if len(out) != 1 {
		t.Fatalf("facts at %s with %q: %d, want 1 (%+v)", url, claimSub, len(out), facts)
	}
	return out[0]
}

func e2eJob(e *env, mod func(*research.Job)) research.Job {
	return e.job(e2eAsk, func(j *research.Job) {
		j.Options.Sources.From = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		if mod != nil {
			mod(j)
		}
	})
}
