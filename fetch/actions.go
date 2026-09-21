package fetch

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Action is one browser-interaction step. Field meaning per verb:
// click/wait-for → Sel; type → Sel + Text; scroll → Text (arg, and Ms
// when numeric); wait → Ms; screenshot → Text (path); eval-js → Text.
// One flat struct — no tagged-union ceremony for seven verbs.
type Action struct {
	Verb string
	Sel  string
	Text string
	Ms   int
}

// ActionWaitCap bounds a single wait line: long pauses belong to the
// caller's budget, not to a DSL line that can silently eat it.
const ActionWaitCap = 30000

// actionSettle is the fixed pause after click/type/scroll — no
// network-idle heuristic; slower UIs use an explicit wait-for line.
const actionSettle = 500 * time.Millisecond

// ParseActions parses the line DSL: one action per input line.
//
//	click <sel> | type <sel> <text…> | scroll <n|top|bottom>
//	wait <ms> | wait-for <sel> | screenshot <path> | eval-js <expr…>
//
// type/screenshot/eval-js take the rest of the line (spaces need no
// quoting); '#' comments and blank lines are skipped. Errors carry the
// 1-based input line number and the offending verb. Pure: no I/O.
func ParseActions(lines []string) ([]Action, error) {
	out := make([]Action, 0, len(lines))
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ln := i + 1
		verb, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		var a Action
		switch verb {
		case "click":
			if rest == "" {
				return nil, fmt.Errorf("fetch: action line %d: click needs a selector", ln)
			}
			a = Action{Verb: verb, Sel: rest}
		case "type":
			sel, text, _ := strings.Cut(rest, " ")
			if strings.TrimSpace(sel) == "" || strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("fetch: action line %d: type needs a selector and text", ln)
			}
			a = Action{Verb: verb, Sel: sel, Text: text}
		case "scroll":
			switch rest {
			case "top", "bottom":
			default:
				n, err := strconv.Atoi(rest)
				if err != nil || n < 0 {
					return nil, fmt.Errorf("fetch: action line %d: scroll wants top|bottom|<pixels>", ln)
				}
				a.Ms = n
			}
			a.Verb, a.Text = verb, rest
		case "wait":
			ms, err := strconv.Atoi(rest)
			if err != nil || ms < 0 {
				return nil, fmt.Errorf("fetch: action line %d: wait wants milliseconds", ln)
			}
			if ms > ActionWaitCap {
				return nil, fmt.Errorf("fetch: action line %d: wait %d exceeds the %d ms cap (use repeated wait lines)", ln, ms, ActionWaitCap)
			}
			a = Action{Verb: verb, Ms: ms}
		case "wait-for":
			if rest == "" {
				return nil, fmt.Errorf("fetch: action line %d: wait-for needs a selector", ln)
			}
			a = Action{Verb: verb, Sel: rest}
		case "screenshot":
			if rest == "" {
				return nil, fmt.Errorf("fetch: action line %d: screenshot needs a path", ln)
			}
			a = Action{Verb: verb, Text: rest}
		case "eval-js":
			if rest == "" {
				return nil, fmt.Errorf("fetch: action line %d: eval-js needs an expression", ln)
			}
			a = Action{Verb: verb, Text: rest}
		default:
			return nil, fmt.Errorf("fetch: action line %d: unknown verb %q (want click|type|scroll|wait|wait-for|screenshot|eval-js)", ln, verb)
		}
		out = append(out, a)
	}
	return out, nil
}

// sleepCtx waits d or fails with the ctx error — the one ctx-bounded
// pause helper (fetch settle, action settle, and the wait verb share it).
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// FetchWithActions navigates, waits for load, runs the action lines in
// order, settles, and returns the final HTML. Fetch is this with nil
// actions — one code path, so non-action browser behavior cannot drift.
// With CaptureXHR set, matching XHR/fetch response bodies ride the
// response: subscribe BEFORE navigation, drain AFTER the settle and
// BEFORE page.Close (CDP evicts buffers on close).
func (r *RodFetcher) FetchWithActions(ctx context.Context, req FetchRequest, acts []Action) (*FetchResponse, error) {
	if err := r.ensureBrowser(); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, budget(req))
	defer cancel()
	var caps *xhrCollector
	if len(req.CaptureXHR) > 0 {
		patterns, err := ValidateXHRPatterns(req.CaptureXHR)
		if err != nil {
			return nil, err
		}
		caps = &xhrCollector{patterns: patterns}
	}
	page, err := r.openLoadedPage(cctx, req, caps, nil)
	if err != nil {
		return nil, err
	}
	if err := runActions(cctx, page, acts); err != nil {
		_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
		return nil, err
	}
	// ponytail: fixed 2s settle, no network-idle heuristic; ceiling = slow hydration, tunable later.
	if err := sleepCtx(cctx, 2*time.Second); err != nil {
		_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
		return nil, fmt.Errorf("fetch: settle: %w", err)
	}
	html, err := page.HTML()
	if err != nil {
		_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
		return nil, fmt.Errorf("fetch: read html: %w", err)
	}
	resp := &FetchResponse{URL: req.URL, FinalURL: req.URL, StatusCode: 200, HTML: []byte(html)}
	if caps != nil {
		resp.XHR = caps.drain(page) // body buffers evict at page.Close — drain first
	}
	if err := page.Close(); err != nil {
		return nil, fmt.Errorf("fetch: close page: %w", err)
	}
	return resp, nil
}

// openPage creates the page, optionally sets the lang header, subscribes
// XHR capture (between creation and navigation — early responses must be
// seen), then navigates. One create-then-navigate shape for both lang
// paths: the blank-page round trip is sub-ms next to the fixed 2s settle.
func (r *RodFetcher) openPage(cctx context.Context, req FetchRequest, caps *xhrCollector) (*rod.Page, error) {	page, err := r.browser.Context(cctx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("fetch: open page: %w", err)
	}
	if req.Lang != "" {
		if _, err := page.SetExtraHeaders([]string{"Accept-Language", req.Lang}); err != nil {
			_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
			return nil, fmt.Errorf("fetch: set lang header: %w", err)
		}
	}
	if caps != nil {
		wait := caps.subscribe(page)
		go wait() // push-consume network events until the page ctx dies (void callbacks never satisfy the loop)
	}
	if err := page.Navigate(req.URL); err != nil {
		_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
		return nil, fmt.Errorf("fetch: navigate: %w", err)
	}
	return page, nil
}

// navRetryable reports the WaitLoad navigation race: the page JS-redirected
// mid-load (consent walls, auth bounces), destroying the execution context
// rod was waiting on. A fresh navigation usually clears it; budget timeouts
// and real failures are not retryable.
func navRetryable(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "execution context was destroyed")
}

// openLoadedPage navigates and waits for load. prepare runs post-open,
// pre-wait (the viewport override). A retryable WaitLoad race gets exactly
// one fresh navigation; every other error surfaces as-is. The event
// consumer starts inside openPage, pre-navigation, on every attempt.
func (r *RodFetcher) openLoadedPage(cctx context.Context, req FetchRequest, caps *xhrCollector, prepare func(*rod.Page) error) (*rod.Page, error) {
	page, err := r.openPage(cctx, req, caps)
	if err != nil {
		return nil, err
	}
	if prepare != nil {
		if err := prepare(page); err != nil {
			_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
			return nil, err
		}
	}
	werr := page.WaitLoad()
	if werr == nil {
		return page, nil
	}
	_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
	if !navRetryable(werr) {
		return nil, fmt.Errorf("fetch: wait load: %w", werr)
	}
	page, err = r.openPage(cctx, req, caps)
	if err != nil {
		return nil, fmt.Errorf("fetch: wait load: %w", werr)
	}
	if prepare != nil {
		if err := prepare(page); err != nil {
			_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
			return nil, fmt.Errorf("fetch: wait load: %w", werr)
		}
	}
	if err := page.WaitLoad(); err != nil {
		_ = page.Close() //nolint:errcheck // error path; teardown failure unactionable
		return nil, fmt.Errorf("fetch: wait load: %w", err)
	}
	return page, nil
}

// runActions executes action lines in order under the fetch budget.
func runActions(ctx context.Context, page *rod.Page, acts []Action) error {
	for i, a := range acts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("fetch: action %d (%s): %w", i+1, a.Verb, err)
		}
		if err := runAction(ctx, page, a); err != nil {
			return fmt.Errorf("fetch: action %d (%s): %w", i+1, a.Verb, err)
		}
		switch a.Verb {
		case "click", "type", "scroll":
			if err := sleepCtx(ctx, actionSettle); err != nil {
				return fmt.Errorf("fetch: action %d (%s): %w", i+1, a.Verb, err)
			}
		}
	}
	return nil
}

func runAction(ctx context.Context, page *rod.Page, a Action) error {
	switch a.Verb {
	case "click":
		el, err := page.Element(a.Sel)
		if err != nil {
			return err
		}
		return el.Click(proto.InputMouseButtonLeft, 1)
	case "type":
		el, err := page.Element(a.Sel)
		if err != nil {
			return err
		}
		// Input is InsertText with NO select-all — bare Input appends to
		// pre-filled fields; SelectAllText-first gives replace semantics.
		if err := el.SelectAllText(); err != nil {
			return err
		}
		return el.Input(a.Text)
	case "scroll":
		switch a.Text {
		case "top":
			_, err := page.Eval("window.scrollTo(0, 0)")
			return err
		case "bottom":
			_, err := page.Eval("window.scrollTo(0, document.body.scrollHeight)")
			return err
		default:
			_, err := page.Eval("n => window.scrollBy(0, n)", a.Ms)
			return err
		}
	case "wait":
		return sleepCtx(ctx, time.Duration(a.Ms)*time.Millisecond)
	case "wait-for":
		_, err := page.Element(a.Sel) // auto-waits under the ctx budget
		return err
	case "screenshot":
		png, err := page.Screenshot(false, nil)
		if err != nil {
			return err
		}
		return os.WriteFile(a.Text, png, 0o644)
	case "eval-js":
		_, err := page.Eval(a.Text) // result discarded; arbitrary JS is inherent to the verb (documented)
		return err
	default:
		return fmt.Errorf("unknown verb %q", a.Verb)
	}
}

// screenshot is the shared capture path: nav → WaitLoad → actions →
// settle → full-page PNG.
func (r *RodFetcher) screenshot(ctx context.Context, rawURL string, width, height int, acts []Action) ([]byte, error) {
	if err := r.ensureBrowser(); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, screenshotBudget)
	defer cancel()
	page, err := r.openLoadedPage(cctx, FetchRequest{URL: rawURL}, nil, func(p *rod.Page) error {
		if width > 0 && height > 0 {
			return p.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: width, Height: height})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = page.Close() }() //nolint:errcheck // page teardown; failure unactionable
	if err := runActions(cctx, page, acts); err != nil {
		return nil, err
	}
	// Same fixed settle as Fetch — one policy, no divergence.
	if err := sleepCtx(cctx, 2*time.Second); err != nil {
		return nil, fmt.Errorf("fetch: settle: %w", err)
	}
	png, err := page.Screenshot(true, nil) // full-page, defaults
	if err != nil {
		return nil, fmt.Errorf("fetch: screenshot: %w", err)
	}
	return png, nil
}

// ScreenshotActions news a throwaway browser, runs the actions, and
// captures a full-page PNG — one session for click-then-capture flows
// (CLI-only at the MCP boundary: an action line would hand an agent a
// server-side file-write).
func ScreenshotActions(ctx context.Context, rawURL string, width, height int, acts []Action) ([]byte, error) {
	return NewRodFetcher().ScreenshotActions(ctx, rawURL, width, height, acts)
}

// ScreenshotActions captures through THIS fetcher (the CDP field is
// honored) — scrape's screenshot path sets CDP and calls the method.
func (r *RodFetcher) ScreenshotActions(ctx context.Context, rawURL string, width, height int, acts []Action) ([]byte, error) {
	return r.screenshot(ctx, rawURL, width, height, acts)
}
