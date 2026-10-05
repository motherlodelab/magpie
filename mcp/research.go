package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/research"
	"github.com/motherlodelab/magpie/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- research ---

// ResearchIn is the research tool input: a question (or run_id to poll).
type ResearchIn struct {
	Question        string     `json:"question,omitempty" jsonschema:"the research question (required unless run_id polls a previous run)"`
	Effort          string     `json:"effort,omitempty" jsonschema:"quick, standard (default) or deep"`
	MaxCostUSD      float64    `json:"max_cost_usd,omitempty" jsonschema:"hard USD cap for this run, at most the server's max cost (default: the server's max cost, else 1.00)"`
	Provider        string     `json:"provider,omitempty" jsonschema:"LLM provider (default server provider)"`
	Model           string     `json:"model,omitempty" jsonschema:"model (default server model)"`
	JudgeProvider   string     `json:"judge_provider,omitempty" jsonschema:"provider for the verifier's judge (default: same as provider)"`
	JudgeModel      string     `json:"judge_model,omitempty" jsonschema:"judge model (required with judge_provider)"`
	AllowDomains    StringList `json:"allow_domains,omitempty" jsonschema:"read only these domains and their subdomains"`
	DenyDomains     StringList `json:"deny_domains,omitempty" jsonschema:"never read these domains"`
	PreferDomains   StringList `json:"prefer_domains,omitempty" jsonschema:"rank these domains up"`
	From            string     `json:"from,omitempty" jsonschema:"earliest published date to keep, YYYY-MM-DD"`
	To              string     `json:"to,omitempty" jsonschema:"latest published date to keep, YYYY-MM-DD"`
	SearchProviders StringList `json:"search_providers,omitempty" jsonschema:"search backends (default: duckduckgo, plus searxng when configured; keyed backends must be named)"`
	RunID           string     `json:"run_id,omitempty" jsonschema:"poll a previous run instead of starting one (send it without question); a new run's id arrives in its first progress message"`
}

// ResearchOut is the research tool output.
type ResearchOut struct {
	RunID      string          `json:"run_id" jsonschema:"run id (pass back as run_id to poll)"`
	Status     string          `json:"status" jsonschema:"running, writing, done, failed or stopped"`
	Report     string          `json:"report,omitempty" jsonschema:"markdown report; citations are footnotes to stored snapshots"`
	Cited      []FactOut       `json:"cited,omitempty" jsonschema:"the facts the report cites, in footnote order"`
	Verified   int             `json:"verified" jsonschema:"facts that passed verification (verified + softened + contested)"`
	Dropped    int             `json:"dropped" jsonschema:"facts the verifier dropped"`
	Unreadable []UnreadableOut `json:"unreadable,omitempty" jsonschema:"pages that could not be read, with why"`
	Usage      map[string]any  `json:"usage,omitempty" jsonschema:"accumulated LLM usage"`
}

// FactOut is one cited fact: the claim, its exact quote, and the pinned
// snapshot (url, checked_at) it was verified against.
type FactOut struct {
	ID        string `json:"id"`
	Claim     string `json:"claim"`
	Quote     string `json:"quote"`
	URL       string `json:"url"`
	CheckedAt string `json:"checked_at"`
	Status    string `json:"status"`
	Note      string `json:"note,omitempty"`
}

// UnreadableOut is one page the run couldn't read.
type UnreadableOut struct {
	URL   string `json:"url"`
	Issue string `json:"issue"`
}

func handleResearch(d Deps) func(context.Context, *sdk.CallToolRequest, ResearchIn) (*sdk.CallToolResult, ResearchOut, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in ResearchIn) (*sdk.CallToolResult, ResearchOut, error) {
		question := strings.TrimSpace(in.Question)
		switch {
		case in.RunID != "" && question != "":
			// A client echoing inputs must not start a second paid run.
			return nil, ResearchOut{}, fmt.Errorf("mcp: research: run_id polls a previous run: drop question (or drop run_id to start a new run)")
		case in.RunID != "":
			return pollResearch(d, in.RunID)
		case question == "":
			return nil, ResearchOut{}, fmt.Errorf("mcp: research: question: required (or run_id to poll a previous run)")
		}
		from, err := researchDay("from", in.From)
		if err != nil {
			return nil, ResearchOut{}, err
		}
		to, err := researchDay("to", in.To)
		if err != nil {
			return nil, ResearchOut{}, err
		}
		provider, model := d.DefaultProvider, d.DefaultModel
		if in.Provider != "" && in.Provider != provider {
			provider, model = in.Provider, config.DefaultModel(in.Provider) // never another provider's model
		}
		if in.Model != "" {
			model = in.Model
		}
		// The server's cap is a ceiling a client may lower, never raise.
		capUSD := in.MaxCostUSD
		if d.MaxCost > 0 && (capUSD <= 0 || capUSD > d.MaxCost) {
			capUSD = d.MaxCost
		}
		if capUSD <= 0 {
			capUSD = research.DefaultMaxCostUSD
		}
		// Minted here, not in Run, so the first progress message can carry
		// it: a client that times out still has a run to poll.
		runID := store.NewRunID()
		// No human in the loop: the plan is auto-approved and goes out as a
		// progress message like every other event.
		var onEvent func(research.Event)
		if token := req.Params.GetProgressToken(); token != nil {
			seq := 0 // OnEvent is serial
			onEvent = func(ev research.Event) {
				seq++
				msg := ev.Detail
				if msg == "" {
					msg = ev.URL
				}
				if seq == 1 {
					msg = "run_id " + runID + " · " + msg
				}
				_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ //nolint:errcheck // progress is best-effort; a failed notify must not fail the run
					ProgressToken: token, Progress: float64(seq), Message: ev.Stage + ": " + msg,
				})
			}
		}
		rep, err := research.Run(ctx, d.ScrapeDeps, research.Job{
			RunID: runID, Question: question, OnEvent: onEvent,
			Options: research.Options{
				Provider: provider, Model: model, JudgeProvider: in.JudgeProvider, JudgeModel: in.JudgeModel,
				MaxCostUSD: capUSD, Effort: in.Effort, Search: []string(in.SearchProviders),
				Sources: research.SourcePolicy{Allow: []string(in.AllowDomains), Deny: []string(in.DenyDomains),
					Prefer: []string(in.PreferDomains), From: from, To: to},
			},
		})
		if err != nil {
			if rep.RunID != "" {
				return nil, ResearchOut{}, fmt.Errorf("mcp: research: run %s (%s): %w", rep.RunID, rep.Status, err)
			}
			return nil, ResearchOut{}, fmt.Errorf("mcp: research: %w", err)
		}
		out := researchOut(rep.RunID, rep.Status, rep.Markdown, rep.Facts, rep.Unreadable)
		for _, id := range rep.Cited {
			for _, f := range rep.Facts {
				if f.FactID == id {
					out.Cited = append(out.Cited, FactOut{ID: f.FactID, Claim: f.Claim, Quote: f.Quote, URL: f.URL,
						CheckedAt: f.CheckedAt.UTC().Format(time.RFC3339Nano), Status: f.Status, Note: f.Note})
				}
			}
		}
		if info, ierr := d.DB.GetRun(rep.RunID); ierr == nil {
			out.Usage = runUsage(info)
		}
		return nil, out, nil
	}
}

// pollResearch reads a run back from the store: no LLM, no fetch.
// ponytail: cited facts aren't stored per run, so a poll returns the
// report (its Sources footnotes carry the citations) without Cited.
func pollResearch(d Deps, runID string) (*sdk.CallToolResult, ResearchOut, error) {
	rr, err := d.DB.GetResearchRun(runID)
	if err != nil {
		return nil, ResearchOut{}, fmt.Errorf("mcp: research: %w", err)
	}
	facts, err := d.DB.Facts(runID)
	if err != nil {
		return nil, ResearchOut{}, fmt.Errorf("mcp: research: %w", err)
	}
	urls, err := d.DB.CrawlURLs(runID)
	if err != nil {
		return nil, ResearchOut{}, fmt.Errorf("mcp: research: %w", err)
	}
	var unreadable []research.Unreadable
	for _, c := range urls {
		if c.Status == "error" {
			issue, _, _ := strings.Cut(c.Msg, ": ")
			unreadable = append(unreadable, research.Unreadable{URL: c.URL, Issue: issue})
		}
	}
	out := researchOut(runID, rr.Status, rr.Report, facts, unreadable)
	if info, ierr := d.DB.GetRun(runID); ierr == nil {
		out.Usage = runUsage(info)
	}
	return nil, out, nil
}

func researchOut(runID, status, report string, facts []research.Fact, unreadable []research.Unreadable) ResearchOut {
	out := ResearchOut{RunID: runID, Status: status, Report: report}
	for _, f := range facts {
		switch f.Status {
		case "verified", "softened", "contested":
			out.Verified++
		case "dropped":
			out.Dropped++
		}
	}
	for _, u := range unreadable {
		out.Unreadable = append(out.Unreadable, UnreadableOut{URL: u.URL, Issue: u.Issue})
	}
	return out
}

func researchDay(name, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("mcp: research: %s: %q, want YYYY-MM-DD", name, s)
	}
	return t, nil
}
