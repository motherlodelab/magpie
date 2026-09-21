package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// AnthropicAdapter uses POST /v1/messages with GA output_config.format
// json_schema — no beta header, no tool wrapping. Also serves OpenCode
// Zen/Go /messages models (base-URL switch).
type AnthropicAdapter struct {
	BaseURL string
	APIKey  string
	Model   string
	Log     func(purpose string, usage TokenUsage)
	// Provider overrides Name() so reused adapters log their own id
	// (usage.provider rows); empty defaults to "anthropic".
	Provider string
	// ExtraHeaders merge into every request (session UA, ...).
	ExtraHeaders map[string]string
	// SessionID sets x-opencode-session when non-empty (Zen/Go require it).
	SessionID string
	schema    *Schema
}

func NewAnthropic(baseURL, apiKey, model string, sch *Schema) *AnthropicAdapter {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if v := os.Getenv("MAGPIE_BASE_URL"); v != "" {
		baseURL = v
	}
	return &AnthropicAdapter{BaseURL: baseURL, APIKey: apiKey, Model: model, schema: sch}
}

func (a *AnthropicAdapter) Name() string {
	if a.Provider != "" {
		return a.Provider
	}
	return "anthropic"
}

func (a *AnthropicAdapter) Extract(ctx context.Context, in ExtractInput) (ExtractResult, error) {
	if in.Schema == nil {
		in.Schema = a.schema
	}
	if in.Schema == nil {
		return ExtractResult{}, fmt.Errorf("extract: nil schema")
	}
	doc, err := schemaDoc(in.Schema.Raw)
	if err != nil {
		return ExtractResult{}, err
	}
	call := func(ctx context.Context, system, user string) (string, TokenUsage, error) {
		return a.promptOnce(ctx, system, user, doc)
	}
	return runRepairLoop(ctx, call, a.Log, in, a.Name(), a.Model)
}

// PromptText is the schema-less text path: the same /v1/messages call
// with no output_config, so the model returns plain text. See Prompter
// for the caller-logs attribution contract.
func (a *AnthropicAdapter) PromptText(ctx context.Context, system, user string) (string, TokenUsage, error) {
	return a.promptOnce(ctx, system, user, nil)
}

func (a *AnthropicAdapter) promptOnce(ctx context.Context, system, user string, doc any) (string, TokenUsage, error) {
	body := map[string]any{
		"model":      a.Model,
		"max_tokens": 16000,
		"system":     system,
		"messages":   []any{map[string]any{"role": "user", "content": user}},
	}
	if doc != nil {
		body["output_config"] = map[string]any{
			"format": map[string]any{"type": "json_schema", "schema": doc},
		}
	}
	headers := map[string]string{
		"x-api-key":         a.APIKey,
		"anthropic-version": "2023-06-01",
	}
	if a.SessionID != "" {
		headers[SessionHeader] = a.SessionID
	}
	for k, v := range a.ExtraHeaders {
		headers[k] = v
	}
	out, err := postJSON(ctx, endpointURL(a.BaseURL, "/v1/messages"), headers, body)
	if err != nil {
		return "", TokenUsage{}, err
	}
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return "", TokenUsage{}, fmt.Errorf("decode messages: %w", err)
	}
	usage := TokenUsage{PromptTokens: env.Usage.InputTokens, CompletionTokens: env.Usage.OutputTokens}
	if env.StopReason == "max_tokens" {
		return "", usage, &truncError{"response truncated (stop_reason=max_tokens)"}
	}
	if env.StopReason == "refusal" {
		return "", usage, fmt.Errorf("model refused")
	}
	for _, c := range env.Content {
		if c.Type == "text" {
			return c.Text, usage, nil
		}
	}
	return "", usage, fmt.Errorf("no text content block")
}
