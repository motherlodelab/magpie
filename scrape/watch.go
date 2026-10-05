package scrape

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/webhook"
)

// WatchResult is one watch check. WebhookStatus: "" (not fired —
// no change or no webhook), "sent", or "failed: …".
type WatchResult struct {
	URL           string
	Changed       bool
	OldHash       string
	NewHash       string
	Diff          string
	WebhookStatus string
	CheckedAt     time.Time
	// WordsDelta is the new snapshot's word count minus the old one's
	// (strings.Fields, as DiffWords counts) — net, so a same-length rewrite
	// reads 0. LinksAdded/LinksRemoved are link targets (clean.MarkdownLinks)
	// present only in the new / only in the old snapshot, sorted. All zero
	// on the baseline and on an unchanged check.
	// ponytail: net, not gross — gross added/removed words would need
	// DiffWords to return its op counts; the upgrade path is a
	// DiffWordsStats beside it.
	WordsDelta   int
	LinksAdded   []string
	LinksRemoved []string
}

// CheckForChange runs one zero-LLM check: scrape the URL markdown-only,
// hash it, compare against watch's own latest snapshot (a scrape or research
// read in between is not a baseline), diff on change, ALWAYS
// store the snapshot (first run = baseline), then fire the webhook once
// on change. A webhook failure is recorded in WatchResult.WebhookStatus,
// never returned — the snapshot is already stored and a dead sink must
// not fail the check (cron re-runs are the retry). The hash covers
// markdown only: an explicit Vertical would run record-discarding
// sub-fetches per check — pass none.
func CheckForChange(ctx context.Context, d Deps, rawURL string, o Options) (WatchResult, error) {
	// Zero-LLM is enforced, not hoped for: a caller-supplied schema must
	// never turn a price watch into token spend.
	o.Schema = nil
	res, err := Run(ctx, d, rawURL, o)
	if err != nil {
		return WatchResult{}, err
	}
	newHash := sha256Hex(res.Markdown)
	out := WatchResult{URL: rawURL, NewHash: newHash}
	prev, ok, err := d.DB.LatestWatchSnapshot(rawURL)
	if err != nil {
		return WatchResult{}, err
	}
	if ok {
		out.OldHash = prev.ContentHash
		if prev.ContentHash != newHash {
			out.Changed = true
			diff, derr := DiffWords(prev.Markdown, res.Markdown)
			if derr != nil {
				return WatchResult{}, derr
			}
			out.Diff = diff
			out.WordsDelta = len(strings.Fields(res.Markdown)) - len(strings.Fields(prev.Markdown))
			out.LinksAdded, out.LinksRemoved = linkDelta(clean.MarkdownLinks(prev.Markdown), clean.MarkdownLinks(res.Markdown))
		}
	}
	// CheckedAt is the stored key, so the webhook below carries it too.
	if out.CheckedAt, err = d.DB.PutSnapshot(rawURL, newHash, res.Markdown, out.Changed); err != nil {
		return WatchResult{}, err
	}
	if out.Changed && o.Webhook != "" {
		out.WebhookStatus = postWebhook(ctx, rawURL, o.Webhook, out)
	}
	return out, nil
}

// linkDelta returns targets only in cur (added) and only in prev (removed),
// sorted. Inputs are already deduped by MarkdownLinks.
func linkDelta(prev, cur []string) (added, removed []string) {
	p, c := make(map[string]bool, len(prev)), make(map[string]bool, len(cur))
	for _, u := range prev {
		p[u] = true
	}
	for _, u := range cur {
		c[u] = true
		if !p[u] {
			added = append(added, u)
		}
	}
	for _, u := range prev {
		if !c[u] {
			removed = append(removed, u)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type webhookPayload struct {
	URL     string `json:"url"`
	Changed bool   `json:"changed"`
	OldHash string `json:"old_hash"`
	NewHash string `json:"new_hash"`
	Diff    string `json:"diff"`
}

// postWebhook fires the change notification exactly once (cron re-runs
// are the retry), unsigned, with the v1 payload, through webhook.Send —
// the one delivery engine core and the desktop share. The message id
// derives from the new content hash, so a re-post of the same change is
// recognisably the same message.
func postWebhook(ctx context.Context, rawURL, webhookURL string, res WatchResult) string {
	body, err := json.Marshal(webhookPayload{
		URL: rawURL, Changed: res.Changed,
		OldHash: res.OldHash, NewHash: res.NewHash, Diff: res.Diff,
	})
	if err != nil {
		return "failed: " + err.Error()
	}
	r := webhook.Send(ctx, webhookURL, "", "msg_"+res.NewHash[:24], body, webhook.Options{Tries: 1}) // CLI contract: exactly once, unsigned; cron re-runs are the retry
	if r.Err != nil {
		return "failed: " + r.Err.Error()
	}
	return "sent"
}
