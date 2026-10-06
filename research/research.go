// Package research holds the DR1 primitives for deep research: options,
// plan shape, query dedupe, URL ranking, spend governor. No LLM calls; the
// loop (DR2) composes them.
//
// Layering: research sits above scrape and crawl (it imports them, never
// the reverse); only surfaces (cli, mcp) import research.
package research

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

// Options configures one research run. JSON tags are the persisted form
// (research_runs.options); it holds no secrets — keys come from scrape.Deps.
type Options struct {
	Provider      string       `json:"provider"` // required; surfaces resolve config defaults first (scrape.Options convention)
	Model         string       `json:"model"`
	JudgeProvider string       `json:"judge_provider,omitempty"` // "" = Provider
	JudgeModel    string       `json:"judge_model,omitempty"`    // "" = Model (only when JudgeProvider is "")
	MaxCostUSD    float64      `json:"max_cost_usd"`             // the cap; required, > 0
	MaxToolCalls  int          `json:"max_tool_calls"`           // searches + page reads; 0 = the effort's default
	Effort        string       `json:"effort"`                   // quick|standard|deep; "" = standard
	Sources       SourcePolicy `json:"sources"`
	Search        []string     `json:"search,omitempty"` // search backends; empty = the keyless ones (duckduckgo, searxng when configured) — keyed backends must be named
}

// SourcePolicy says where a run may read. Domains are bare host suffixes
// (Normalized canonicalizes what users type); a domain covers its
// subdomains.
type SourcePolicy struct {
	Allow  []string `json:"allow,omitempty"`  // non-empty = only these domains and their subdomains
	Deny   []string `json:"deny,omitempty"`   // never read; beats Allow and Prefer
	Prefer []string `json:"prefer,omitempty"` // ranked up (×2)
	// Sessions are read with the user's login through Job.Session. They
	// never widen Allow, Deny beats them, and a page read with a login
	// never composes a search query (the taint, extractPage). "Only my
	// subscriptions" is Allow set to these domains.
	Sessions []string  `json:"sessions,omitempty"`
	From     time.Time `json:"from,omitzero"` // published-date range; applied to facts in DR2
	To       time.Time `json:"to,omitzero"`
}

// ponytail: recommended defaults from the evidence report ⌗ (Quick 1 ×
// 3–10 calls, Standard 2–4 × 10–15, Deep 5 capped), not measured optima;
// DR7's trajectory metrics (redundant-query share, wasted tail) replace them.
var efforts = map[string]struct{ SubResearchers, ToolCalls int }{
	"quick":    {1, 10},
	"standard": {3, 45},
	"deep":     {5, 100},
}

// Normalized is the one validator every surface (CLI, MCP, desktop bridge)
// calls: it returns a canonical copy — effort and tool-call defaults filled
// in, domains canonicalized, sorted and de-duplicated — or an error naming
// the field and the offending value. On error the returned Options is zero:
// no half-filled value to misuse. The caller's slices are never touched.
// A session domain the policy can never read (denied, or outside allow)
// is refused: it would attach a login to nothing.
func (o Options) Normalized() (Options, error) {
	if o.Provider == "" {
		return Options{}, fmt.Errorf("research: provider: required")
	}
	names := extract.ProviderNames()
	for _, p := range []struct{ field, name string }{{"provider", o.Provider}, {"judge_provider", o.JudgeProvider}} {
		if p.name != "" && !slices.Contains(names, p.name) {
			return Options{}, fmt.Errorf("research: %s: %q, want %s", p.field, p.name, strings.Join(names, "|"))
		}
	}
	if o.Effort == "" {
		o.Effort = "standard"
	}
	e, ok := efforts[o.Effort]
	if !ok {
		return Options{}, fmt.Errorf("research: effort: %q, want quick|standard|deep", o.Effort)
	}
	if c := o.MaxCostUSD; !(c > 0) || math.IsInf(c, 1) { // !(c > 0) also catches NaN
		return Options{}, fmt.Errorf("research: max_cost_usd: %v, want a finite amount > 0", c)
	}
	if o.MaxToolCalls < 0 {
		return Options{}, fmt.Errorf("research: max_tool_calls: %d, want >= 0", o.MaxToolCalls)
	}
	if o.MaxToolCalls == 0 {
		o.MaxToolCalls = e.ToolCalls
	}
	if o.JudgeProvider != "" && o.JudgeModel == "" {
		return Options{}, fmt.Errorf("research: judge_model: judge provider %q needs a judge model", o.JudgeProvider)
	}
	for _, l := range []struct {
		name string
		list *[]string
	}{{"allow", &o.Sources.Allow}, {"deny", &o.Sources.Deny}, {"prefer", &o.Sources.Prefer}, {"sessions", &o.Sources.Sessions}} {
		if len(*l.list) == 0 {
			continue
		}
		out := make([]string, 0, len(*l.list)) // a fresh slice: o is a copy, its slices are the caller's
		for _, s := range *l.list {
			d, err := normDomain(s)
			if err != nil {
				return Options{}, fmt.Errorf("research: sources.%s: %q: %w", l.name, s, err)
			}
			out = append(out, d)
		}
		slices.Sort(out)
		*l.list = slices.Compact(out)
	}
	if f, t := o.Sources.From, o.Sources.To; !f.IsZero() && !t.IsZero() && f.After(t) {
		return Options{}, fmt.Errorf("research: sources.from: %s is after to %s", f.Format(time.RFC3339), t.Format(time.RFC3339))
	}
	for _, d := range o.Sources.Sessions {
		// Overlap either way counts: allow markets.ft.com reads with an ft.com session there.
		inAllow := len(o.Sources.Allow) == 0 || matchesAny(d, o.Sources.Allow) ||
			slices.ContainsFunc(o.Sources.Allow, func(a string) bool { return strings.HasSuffix(a, "."+d) })
		if matchesAny(d, o.Sources.Deny) || !inAllow {
			return Options{}, fmt.Errorf("research: sources.sessions: %q: the source policy never reads it (denied, or outside allow)", d)
		}
	}
	if len(o.Search) > 0 {
		names := scrape.SearchProviderNames()
		out := make([]string, 0, len(o.Search)) // fresh: the caller's slice is never touched
		for _, b := range o.Search {
			if !slices.Contains(names, b) {
				return Options{}, fmt.Errorf("research: search: %q, want %s", b, strings.Join(names, "|"))
			}
			out = append(out, b)
		}
		slices.Sort(out)
		o.Search = slices.Compact(out)
	}
	return o, nil
}

// normDomain turns what users type into a bare host suffix:
// "https://www.Nature.com/articles?x" → "nature.com", "*.bbc.co.uk" →
// "bbc.co.uk", "Example.COM." → "example.com", "example.com:8080" →
// "example.com".
func normDomain(s string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(d, "://") {
		u, err := url.Parse(d)
		if err != nil {
			return "", fmt.Errorf("not a domain")
		}
		d = u.Hostname()
	} else {
		d, _, _ = strings.Cut(d, "/")
		d, _, _ = strings.Cut(d, ":")
	}
	d = strings.TrimPrefix(d, "*.")
	d = strings.TrimPrefix(d, "www.")
	d = strings.TrimSuffix(d, ".")
	if d == "" || !strings.Contains(d, ".") || strings.ContainsAny(d, " \t@?#") {
		return "", fmt.Errorf("not a domain")
	}
	return d, nil
}

// judge returns the judge's (provider, model): its own pair when
// JudgeProvider is set (Normalized guarantees the model), else the writer's
// provider with JudgeModel when set — a judge on the same provider may use a
// different model — else the writer's pair.
func (o Options) judge() (provider, model string) {
	if o.JudgeProvider != "" {
		return o.JudgeProvider, o.JudgeModel
	}
	if o.JudgeModel != "" {
		return o.Provider, o.JudgeModel
	}
	return o.Provider, o.Model
}

// Plan is the scope step's output and the plan editor's input; it is
// persisted as research_runs.plan at launch. It has no effort field: the
// effort is the user's (Options.Effort), the plan proposes angles within it.
type Plan struct {
	Brief  string  `json:"brief"`
	Angles []Angle `json:"angles"` // one per sub-researcher
}

// Angle is one sub-researcher's assignment.
type Angle struct {
	Question string   `json:"question"`
	Queries  []string `json:"queries,omitempty"` // seed queries; DR2 dedupes before issuing
}

// Validate checks a plan from the model or the editor against the run's
// normalized options: a brief, 1..SubResearchers angles, each with a
// question (spec §9 risk 5 — fan-out is bounded in Go, not the prompt).
func (p Plan) Validate(o Options) error {
	if strings.TrimSpace(p.Brief) == "" {
		return fmt.Errorf("research: plan: brief: required")
	}
	limit := efforts[o.Effort].SubResearchers
	if n := len(p.Angles); n < 1 || n > limit {
		return fmt.Errorf("research: plan: angles: %d, want 1..%d for effort %q", n, limit, o.Effort)
	}
	for i, a := range p.Angles {
		if strings.TrimSpace(a.Question) == "" {
			return fmt.Errorf("research: plan: angles[%d]: question: required", i)
		}
	}
	return nil
}

// Fact is the claim ledger row (DR0); an alias, never a parallel type.
type Fact = store.Fact
