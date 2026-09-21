package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ExtractInput is one LLM extraction job.
type ExtractInput struct {
	Markdown       string
	StructuredData json.RawMessage
	Schema         *Schema
	PromptExtra    string // repair context appended on retries
	Purpose        string // first-attempt log purpose; "" defaults to "extract"
}

// ExtractResult is validated, coerced output.
type ExtractResult struct {
	Record   map[string]any
	Raw      json.RawMessage
	Usage    TokenUsage
	Provider string
	Model    string
	Attempts int
}

// Extractor runs the 3-attempt repair loop.
type Extractor interface {
	Extract(ctx context.Context, in ExtractInput) (ExtractResult, error)
	Name() string
}

// providerCall is one raw LLM round-trip returning output text + usage.
// USDEstimate is set only when the provider reports real cost (usage.cost);
// otherwise runRepairLoop falls back to the price table.
type providerCall func(ctx context.Context, system, user string) (string, TokenUsage, error)

// runRepairLoop implements spec §3.3: max 3 attempts; repair prompt embeds
// validator err.Error() verbatim.
func runRepairLoop(ctx context.Context, call providerCall, log func(purpose string, usage TokenUsage), in ExtractInput, provider, model string) (ExtractResult, error) {
	system := "You emit ONLY JSON matching the provided JSON schema. No prose, no code fences."
	user := buildPrompt(in)
	var lastErr error
	var total TokenUsage
	for attempt := 0; attempt < 3; attempt++ {
		text, u, err := call(ctx, system, user)
		if err != nil {
			// Transport errors fail loudly (no validator text to repair with).
			if isTruncation(err) {
				return ExtractResult{}, fmt.Errorf("extract: %w", err)
			}
			return ExtractResult{}, fmt.Errorf("extract: provider: %w", err)
		}
		// Cost policy lives in costFor (cost.go): provider-reported cost wins,
		// flat-rate stays 0 quietly, otherwise the price table.
		u.USDEstimate = costFor(provider, model, u)
		total.PromptTokens += u.PromptTokens
		total.CompletionTokens += u.CompletionTokens
		total.USDEstimate += u.USDEstimate
		purpose := in.Purpose
		if purpose == "" {
			purpose = "extract"
		}
		if attempt > 0 {
			purpose = "repair"
		}
		if log != nil {
			log(purpose, u)
		}
		if verr := in.Schema.Validate([]byte(text)); verr != nil {
			lastErr = verr
			user = buildPrompt(in) + "\n\nYour previous output was invalid:\n" + text + "\n\nValidation errors:\n" + verr.Error() + "\nFix the specific fields above and emit ONLY the corrected JSON."
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return ExtractResult{}, fmt.Errorf("extract: parse valid output: %w", err)
		}
		// jsonld_path pre-fill: pull missing fields from the sidecar first.
		for field, path := range in.Schema.Hints.JSONLDPath {
			if _, ok := rec[field]; !ok || rec[field] == nil {
				if v, found := JSONLDWalk(in.StructuredData, path); found {
					rec[field] = v
				}
			}
		}
		rec, err = ApplyCoercions(rec, in.Schema.Hints)
		if err != nil {
			return ExtractResult{}, err
		}
		// Re-validate after coercion? Coercion fixes types, so marshal and check.
		if coerced, merr := json.Marshal(rec); merr == nil {
			if verr := in.Schema.Validate(coerced); verr != nil {
				lastErr = verr
				user = buildPrompt(in) + "\n\nCoerced output still invalid:\n" + string(coerced) + "\n\nValidation errors:\n" + verr.Error()
				continue
			}
			text = string(coerced)
		}
		return ExtractResult{Record: rec, Raw: json.RawMessage(text), Usage: total, Provider: provider, Model: model, Attempts: attempt + 1}, nil
	}
	return ExtractResult{}, fmt.Errorf("extract: still invalid after 3 attempts: %w", lastErr)
}

func buildPrompt(in ExtractInput) string {
	var sb bytes.Buffer
	sb.WriteString("Extract structured data from the following page content.\n\n")
	if len(in.StructuredData) > 0 {
		sb.WriteString("Structured data (always complete, prefer these facts):\n")
		sb.Write(in.StructuredData)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Page markdown:\n")
	sb.WriteString(in.Markdown)
	if in.PromptExtra != "" {
		sb.WriteString("\n\n" + in.PromptExtra)
	}
	return sb.String()
}

type truncError struct{ msg string }

func (e *truncError) Error() string { return e.msg }

func isTruncation(err error) bool {
	_, ok := err.(*truncError)
	return ok
}

// SessionHeader identifies the run to OpenCode Zen/Go (required server-side).
const SessionHeader = "x-opencode-session"

// MagpieUA identifies magpie on Zen/Go calls (required alongside SessionHeader).
const MagpieUA = "magpie (+https://github.com/you/magpie)"

// schemaDoc round-trips a schema through JSON so providers get a plain
// map without yaml-node types.
func schemaDoc(raw any) (any, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("extract: marshal schema: %w", err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("extract: decode schema: %w", err)
	}
	return doc, nil
}

// httpClient is shared: one LLM call at a time per run, no per-call setup.
var httpClient = &http.Client{Timeout: 120 * time.Second}

// endpointURL joins the Anthropic base URL and API path ("/v1/messages").
// Bases in the wild disagree about carrying the /v1 segment —
// api.anthropic.com does not, the zen bases and MAGPIE_BASE_URL usually do —
// so the join collapses a doubled /v1 instead of posting to /v1/v1/… (which
// 404'd every messages-routed zen model). The OpenAI adapter is NOT routed
// through this: it appends "/chat/completions" verbatim, so root-mounted
// gateways keep working untouched.
func endpointURL(base, path string) string {
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		return base + path[len("/v1"):]
	}
	return base + path
}

func postJSON(ctx context.Context, url string, headers map[string]string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body fully read above; close error unactionable
	out, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(out, 500))
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
