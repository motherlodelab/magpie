package crawl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/selector"
)

// domainState holds per-domain cache/heal runtime. mu serializes the
// extract workers sharing a domain (doc map + healer + flush counter).
type domainState struct {
	mu          sync.Mutex
	doc         selector.SelectorDoc
	loaded      bool
	healer      *selector.Healer
	cachedPages int // pages served from cache since last null-rate flush
}

// tryRelocate is the zero-LLM heal middle step: for each triggered field
// with a persisted fingerprint, re-find the element on retained new-
// template samples and validate against truth already paid for. Returns
// the fields still needing synthesis.
func (c *crawlContext) tryRelocate(st *domainState, domain string, triggered []string) []string {
	samples := st.healer.Samples()
	if len(samples) == 0 || len(st.doc.Fingerprints) == 0 {
		return triggered
	}
	changed := false
	remaining := triggered[:0]
	for _, f := range triggered {
		sel, ok := st.doc.Fields[f]
		fp, fpOK := st.doc.Fingerprints[f]
		if !ok || !fpOK || sel.Type != "css" || c.opts.Schema.Hints.Multiple[f] {
			remaining = append(remaining, f)
			continue
		}
		newSel, newFP, ok := selector.Relocate(samples, f, sel, fp, c.opts.Schema)
		if !ok {
			remaining = append(remaining, f)
			continue
		}
		if st.doc.Fingerprints == nil {
			st.doc.Fingerprints = map[string]selector.ElementFP{}
		}
		st.doc.Fields[f] = newSel
		st.doc.Fingerprints[f] = newFP
		changed = true
		fmt.Fprintf(os.Stderr, "warning: selector: field %q relocated without LLM\n", f)
	}
	if changed {
		if raw, err := json.Marshal(st.doc); err == nil {
			if perr := c.db.PutSelectors(domain, c.schemaHash, string(raw), len(samples)); perr != nil {
				fmt.Fprintf(os.Stderr, "warning: cache selectors: %v\n", perr)
			}
		}
	}
	return remaining
}

// healField re-synthesizes triggered fields only (full template when ≥50%
// broken), merging into the live doc so healthy fields serve throughout.
func (c *crawlContext) healField(ctx context.Context, st *domainState, domain, pageURL, html, md string, sidecar json.RawMessage, triggered []string) {
	// Zero-LLM relocation first; only fields it declines pay for synthesis.
	triggered = c.tryRelocate(st, domain, triggered)
	if len(triggered) == 0 {
		return
	}
	// Fresh ground truth on the current (new-template) page.
	if res, err := c.extractOne(ctx, md, sidecar, "synth", "heal sample: "+pageURL); err == nil {
		if validRequired(res.Record, requiredFields(c.opts.Schema)) {
			st.healer.Retain(selector.SynthSample{URL: pageURL, HTML: html, Sidecar: sidecar, Truth: res.Record})
		}
	}
	samples := st.healer.Samples()
	if len(samples) == 0 {
		return
	}
	only := triggered
	if selector.FullResynth(len(st.doc.Fields), len(triggered)) {
		only = nil // ≥50% broken → full redesign
	}
	doc, err := selector.Synthesize(ctx, samples, c.opts.Schema, c.opts.Propose, only)
	if err != nil {
		return
	}
	if only == nil {
		for f, sel := range doc.Fields {
			st.doc.Fields[f] = sel
		}
		for f, fp := range doc.Fingerprints {
			if st.doc.Fingerprints == nil {
				st.doc.Fingerprints = map[string]selector.ElementFP{}
			}
			st.doc.Fingerprints[f] = fp
		}
	} else {
		for _, f := range only {
			if sel, ok := doc.Fields[f]; ok {
				st.doc.Fields[f] = sel
				if fp, ok := doc.Fingerprints[f]; ok { // keep both maps in sync
					if st.doc.Fingerprints == nil {
						st.doc.Fingerprints = map[string]selector.ElementFP{}
					}
					st.doc.Fingerprints[f] = fp
				} else {
					delete(st.doc.Fingerprints, f)
				}
			} else {
				delete(st.doc.Fields, f) // still failing → per-page LLM
				delete(st.doc.Fingerprints, f)
			}
		}
	}
	if raw, err := json.Marshal(st.doc); err == nil {
		if perr := c.db.PutSelectors(domain, c.schemaHash, string(raw), len(samples)); perr != nil {
			fmt.Fprintf(os.Stderr, "warning: cache selectors: %v\n", perr)
		}
	}
}

// flushNullRates persists current null rates into fields_json.
// ponytail: a crash loses ≤25 pages of stats (ceiling = slightly stale
// trigger after --resume — self-corrects within 25 pages).
func (c *crawlContext) flushNullRates(st *domainState, domain string) {
	for f, sel := range st.doc.Fields {
		sel.NullRate = st.healer.NullRate(f)
		st.doc.Fields[f] = sel
	}
	if raw, err := json.Marshal(st.doc); err == nil {
		if perr := c.db.PutSelectors(domain, c.schemaHash, string(raw), st.doc.SamplesUsed); perr != nil {
			fmt.Fprintf(os.Stderr, "warning: cache selectors: %v\n", perr)
		}
	}
}

func hasNonCacheable(docJSON string, sch *extract.Schema) bool {
	var doc selector.SelectorDoc
	if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
		return true
	}
	for _, f := range selector.SchemaFields(sch) {
		if _, ok := doc.Fields[f]; !ok {
			return true
		}
	}
	return false
}

func requiredFields(sch *extract.Schema) []string {
	obj, ok := sch.Raw.(map[string]any)
	if !ok {
		return nil
	}
	req, ok := obj["required"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, r := range req {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func validRequired(rec map[string]any, required []string) bool {
	for _, f := range required {
		if v, ok := rec[f]; !ok || v == nil {
			return false
		}
	}
	return true
}
