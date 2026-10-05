package scrape

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"
)

// MaxDraftSampleWords caps the page sample a draft reads (cost bound; the
// model needs field names and shape, not the whole page).
const MaxDraftSampleWords = 1500

// maxDraftDescription caps the description in runes.
const maxDraftDescription = 2000

// DraftOptions configures one schema draft. Description is required;
// Sample is optional page markdown the model reads for field names and
// shape, never values. Provider "auto" tries AutoProviderOrder.
type DraftOptions struct {
	Description string
	Sample      string
	Provider    string
	Model       string
	MaxCost     float64
}

// DraftOut is one usable draft: the schema as indented JSON in the model's
// own key order, its root "title" ("" when absent), the winning provider,
// and usage summed over every attempt.
type DraftOut struct {
	Schema   string             `json:"schema"`
	Title    string             `json:"title"`
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	Usage    extract.TokenUsage `json:"usage"`
	Attempts int                `json:"attempts"`
}

// draftSystem asks for flat records: crawl's CSV writer rejects nested
// values and selector docs are per top-level field, so flat drafts are the
// ones that cache.
const draftSystem = `You write JSON Schema (draft 2020-12) documents that magpie uses to extract ONE record from ONE web page.
Reply with ONLY the schema as a JSON object: no prose, no code fences.
The root is {"type":"object"} with a short "title" (1-3 words naming the record) and "properties".
Property names are snake_case. Every property has a "type" and a one-line "description".
Prefer flat scalar properties (string, number, integer, boolean); use an array of strings for repeated values.
Only if the description asks for many items per page, use one array property whose items are flat objects.
List in "required" only the fields every such page will have. No $ref, $defs, $id or $schema.`

// DraftSchema turns a plain-language description into a JSON Schema that
// compiles and has at least one top-level property. It owns its run row
// (command "schema"); each attempt is one Prompt call logged as "schema",
// retries as "repair" (the extract convention). An unusable reply is
// repaired up to 3 attempts with the validator error verbatim; provider,
// key and cost-ceiling errors return at once, never repaired.
// ponytail: no `magpie schema draft` command yet; the upgrade is a
// ~30-line cli/ wrapper printing DraftOut.Schema once someone asks.
func DraftSchema(ctx context.Context, d Deps, o DraftOptions) (DraftOut, error) {
	desc := strings.TrimSpace(o.Description)
	switch {
	case desc == "":
		return DraftOut{}, fmt.Errorf("scrape: draft schema: describe what to extract")
	case utf8.RuneCountInString(desc) > maxDraftDescription:
		return DraftOut{}, fmt.Errorf("scrape: draft schema: description is over %d characters", maxDraftDescription)
	case d.DB == nil:
		return DraftOut{}, fmt.Errorf("scrape: draft schema: nil DB")
	}
	base := "Description: " + desc
	if s := strings.TrimSpace(o.Sample); s != "" {
		base += "\n\nPage sample (field names and shape only; don't copy its values):\n" + CapWords(s, MaxDraftSampleWords)
	}
	runID := store.NewRunID()
	if err := d.DB.BeginRun(runID, "schema"); err != nil {
		return DraftOut{}, err
	}
	finish := func(ok int, status string) {
		if ferr := d.DB.FinishRun(runID, ok, 0, status); ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
		}
	}

	user, purpose := base, "schema"
	var total extract.TokenUsage
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		pr, err := Prompt(ctx, d, runID, PromptOptions{
			Provider: o.Provider, Model: o.Model, MaxCost: o.MaxCost,
			System: draftSystem, User: user, Purpose: purpose,
		})
		if err != nil {
			finish(0, "error")
			return DraftOut{}, err
		}
		o.Provider = pr.Provider // auto: repairs stay on the provider that answered
		total.PromptTokens += pr.Usage.PromptTokens
		total.CompletionTokens += pr.Usage.CompletionTokens
		total.USDEstimate += pr.Usage.USDEstimate
		schema, title, verr := parseDraft(pr.Text)
		if verr == nil {
			finish(1, "finished")
			return DraftOut{Schema: schema, Title: title, Provider: pr.Provider, Model: o.Model, Usage: total, Attempts: attempt}, nil
		}
		lastErr = verr
		user = base + "\n\nYour previous reply was not a usable schema:\n" + pr.Text +
			"\n\nError: " + verr.Error() + "\nReply with ONLY the corrected JSON Schema."
		purpose = "repair"
	}
	finish(0, "error")
	return DraftOut{}, fmt.Errorf("scrape: draft schema: still unusable after 3 attempts: %w", lastErr)
}

// parseDraft accepts a reply only if it is a JSON object with no "$" keys
// that compiles as a schema whose root is an object with at least one
// property. It tolerates one pair of code fences (models add them despite
// the prompt) and returns the JSON re-indented in the model's key order —
// json.Indent, not a map round-trip, which would sort "type" after
// "properties".
func parseDraft(text string) (schema, title string, err error) {
	raw := strings.TrimSpace(text)
	if strings.HasPrefix(raw, "```") {
		// Cut, not an index slice: a one-line fence ("```{…}```") has no
		// newline and leaves raw empty → "not JSON" → repaired, never a panic.
		_, raw, _ = strings.Cut(raw, "\n")
		raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "```"))
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", "", errors.New("the reply is not JSON")
	}
	// The schema compiler's default loader follows $ref to local files, so
	// a page sample could inject a ref that reads one: reject every "$" key
	// before ParseSchema sees the bytes.
	if k := dollarKey(v); k != "" {
		return "", "", fmt.Errorf("%q is not allowed: no $ref, $defs, $id or $schema", k)
	}
	sch, err := extract.ParseSchema([]byte(raw))
	if err != nil {
		return "", "", err
	}
	root, _ := sch.Raw.(map[string]any)
	if root["type"] != "object" || len(selector.SchemaFields(sch)) == 0 {
		return "", "", errors.New(`the root must be {"type":"object"} with at least one property`)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return "", "", err
	}
	title, _ = root["title"].(string)
	return buf.String(), strings.TrimSpace(title), nil
}

// dollarKey returns the first object key starting with "$" anywhere in v
// (JSON escapes like "$ref" are already decoded), or "".
func dollarKey(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if strings.HasPrefix(k, "$") {
				return k
			}
			if k := dollarKey(c); k != "" {
				return k
			}
		}
	case []any:
		for _, c := range t {
			if k := dollarKey(c); k != "" {
				return k
			}
		}
	}
	return ""
}
