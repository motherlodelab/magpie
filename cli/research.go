package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/research"

	"github.com/spf13/cobra"
)

type researchOptions struct {
	Effort, Provider, Model, JudgeProvider, JudgeModel string
	From, To                                           string
	Allow, Deny, Prefer, Search                        []string
	Yes, JSON                                          bool
}

func newResearchCmd() *cobra.Command {
	var o researchOptions
	cmd := &cobra.Command{
		Use:   "research <question>",
		Short: "Deep research: plan, search, read, verify every quote, write a cited report",
		Long: `Research a question across the web and write a report whose every
citation is a footnote to a page magpie fetched and stored. The model
cites fact ids only; Go checks each quote against the stored page, and
resolves citations into footnotes (url, checked_at).

The plan is printed first and needs approval (--yes skips the prompt;
required when stdin is not a terminal; Ctrl-C before the plan is approved
means "don't run"). The cap is the global --max-cost (default $1.00):
research stops at 85% of it so the report still gets written. Once the
run is under way, Ctrl-C once writes from what's verified; twice stops
(facts are kept in the run).

By default only the keyless search backends run (duckduckgo, plus searxng
when MAGPIE_SEARXNG_URL is set); name keyed ones with --search-provider.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResearch(cmd.Context(), args[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Effort, "effort", "standard", "quick|standard|deep (1, 3 or 5 sub-researchers)")
	f.StringVar(&o.Provider, "provider", "", ProviderHelp+" (default: config)")
	f.StringVar(&o.Model, "model", "", "model (default: the provider's default)")
	f.StringVar(&o.JudgeProvider, "judge-provider", "", "provider for the verifier's judge (default: --provider)")
	f.StringVar(&o.JudgeModel, "judge-model", "", "judge model (default: the judge provider's default)")
	f.StringSliceVar(&o.Allow, "allow-domain", nil, "read only these domains and their subdomains (repeatable)")
	f.StringSliceVar(&o.Deny, "deny-domain", nil, "never read these domains (repeatable)")
	f.StringSliceVar(&o.Prefer, "prefer-domain", nil, "rank these domains up (repeatable)")
	f.StringSliceVar(&o.Search, "search-provider", nil, "search backends (default: duckduckgo, plus searxng when configured; keyed backends must be named; repeatable)")
	f.StringVar(&o.From, "from", "", "earliest published date to keep, YYYY-MM-DD")
	f.StringVar(&o.To, "to", "", "latest published date to keep, YYYY-MM-DD")
	f.BoolVar(&o.Yes, "yes", false, "run the drafted plan without asking")
	f.BoolVar(&o.JSON, "json", false, "print the whole report (facts, verdicts, unreadable pages) as JSON")
	return cmd
}

// errDeclined is the operator's "no" at the plan prompt: exit 0, not run.
var errDeclined = errors.New("research: plan declined")

func runResearch(ctx context.Context, question string, o researchOptions) error {
	if strings.TrimSpace(question) == "" {
		return fail(2, "research: question: required")
	}
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	from, ferr := parseDay("from", o.From)
	to, terr := parseDay("to", o.To)
	if err := errors.Join(ferr, terr); err != nil {
		return fail(2, "%v", err)
	}
	// Empty models take the provider's default — never another provider's
	// configured model (summarize's --provider-without---model trap).
	provider, model := cfg.ExtractProvider, o.Model
	if o.Provider != "" {
		provider = o.Provider
	}
	if model == "" {
		model = config.DefaultModel(provider)
		if provider == cfg.ExtractProvider && cfg.Model != "" {
			model = cfg.Model
		}
	}
	judgeModel := o.JudgeModel
	if o.JudgeProvider != "" && judgeModel == "" {
		judgeModel = config.DefaultModel(o.JudgeProvider)
	}
	capUSD := cfg.MaxCost
	if capUSD <= 0 {
		capUSD = research.DefaultMaxCostUSD
	}
	opts, err := research.Options{
		Provider: provider, Model: model, JudgeProvider: o.JudgeProvider, JudgeModel: judgeModel,
		MaxCostUSD: capUSD, Effort: o.Effort, Search: o.Search,
		Sources: research.SourcePolicy{Allow: o.Allow, Deny: o.Deny, Prefer: o.Prefer, From: from, To: to},
	}.Normalized()
	if err != nil {
		return fail(2, "%v", err)
	}
	if !o.Yes && !stdinIsTerminal() {
		return fail(2, "research: --yes is required when stdin is not a terminal (the plan needs approval)")
	}

	est := research.EstimateRun(opts)
	price := fmt.Sprintf("≈ $%.2f", est.USD)
	if est.Unpriced {
		price = "price unknown for " + model
	}
	fmt.Fprintf(os.Stderr, "estimate: ~%d LLM calls, ≤ %d tool calls, %s (cap $%.2f)\n", est.LLMCalls, est.ToolCalls, price, capUSD)

	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)

	// Ctrl-C before the plan is approved means "don't run" (approvePlan
	// reads it); once the run is approved, the watcher owns it: the first
	// writes from what's verified, the second stops.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctl := &research.Control{}
	var wg sync.WaitGroup
	defer wg.Wait() // after the close below: the watcher has an owner
	done := make(chan struct{})
	defer close(done)
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	rep, err := research.Run(ctx, scrapeDeps(db, cfg), research.Job{
		Question: question, Options: opts, Control: ctl, OnEvent: printEvent,
		Approve: func(p research.Plan) (research.Plan, error) {
			p, err := approvePlan(p, o.Yes, sig)
			if err == nil {
				wg.Go(func() { interrupts(sig, done, ctl, cancel) })
			}
			return p, err
		},
	})
	switch {
	case errors.Is(err, errDeclined):
		fmt.Fprintln(os.Stderr, "not run")
		return nil
	case err != nil && ctx.Err() != nil:
		return fmt.Errorf("research: stopped — facts kept in run %s", rep.RunID)
	case err != nil:
		return keyHint(err)
	}
	if o.JSON {
		doc, merr := marshalOut(rep, "research")
		if merr != nil {
			return merr
		}
		if err := writeOut("", doc); err != nil {
			return err
		}
	} else {
		fmt.Print(rep.Markdown)
	}
	fmt.Fprintf(os.Stderr, "run_id=%s status=%s spent=$%.4f\n", rep.RunID, rep.Status, rep.SpentUSD)
	return nil
}

// interrupts: the first Ctrl-C writes from what's verified, the second
// stops. Exits when done closes (the command returned).
func interrupts(sig <-chan os.Signal, done <-chan struct{}, ctl *research.Control, cancel context.CancelFunc) {
	for n := 0; ; n++ {
		select {
		case <-done:
			return
		case <-sig:
			if n > 0 {
				cancel()
				return
			}
			ctl.WriteNow()
			fmt.Fprintln(os.Stderr, "writing from what's verified; Ctrl-C again to stop")
		}
	}
}

// approvePlan prints the plan and asks, unless --yes. A Ctrl-C while the
// plan was drafted, or at the prompt, declines it.
func approvePlan(p research.Plan, yes bool, sig <-chan os.Signal) (research.Plan, error) {
	fmt.Fprintf(os.Stderr, "plan: %s\n", p.Brief)
	for i, a := range p.Angles {
		fmt.Fprintf(os.Stderr, "  %d. %s\n", i+1, a.Question)
		for _, q := range a.Queries {
			fmt.Fprintf(os.Stderr, "       - %s\n", q)
		}
	}
	select {
	case <-sig:
		return research.Plan{}, errDeclined
	default:
	}
	if yes {
		return p, nil
	}
	fmt.Fprint(os.Stderr, "Run this plan? [y/N] ")
	ans := make(chan string, 1)
	// ponytail: after a Ctrl-C at the prompt this reader stays blocked on
	// stdin until the process exits (moments later) — a blocking terminal
	// read can't be interrupted portably.
	in := os.Stdin // read here: the goroutine may outlive a swap
	go func() {
		var a string
		_, _ = fmt.Fscanln(in, &a) //nolint:errcheck // an empty or unreadable answer is a no
		ans <- a
	}()
	select {
	case <-sig:
		fmt.Fprintln(os.Stderr)
		return research.Plan{}, errDeclined
	case a := <-ans:
		if a = strings.ToLower(strings.TrimSpace(a)); a == "y" || a == "yes" {
			return p, nil
		}
	}
	return research.Plan{}, errDeclined
}

// printEvent is one activity line on stderr.
func printEvent(ev research.Event) {
	switch ev.Stage {
	case "read":
		fmt.Fprintf(os.Stderr, "read    %s\n", ev.URL)
	case "unreadable":
		fmt.Fprintf(os.Stderr, "skip    %s: %s\n", ev.URL, ev.Issue)
	case "facts":
		fmt.Fprintf(os.Stderr, "facts   %s from %s\n", ev.Detail, ev.URL)
	case "write":
		fmt.Fprintf(os.Stderr, "write   from %d usable facts\n", ev.Usable)
	case "done":
		fmt.Fprintf(os.Stderr, "done    %s ($%.4f of $%.2f)\n", ev.Detail, ev.SpentUSD, ev.CapUSD)
	default:
		fmt.Fprintf(os.Stderr, "%-7s %s\n", ev.Stage, ev.Detail)
	}
}

func parseDay(name, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("research: --%s %q: want YYYY-MM-DD", name, s)
	}
	return t, nil
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
