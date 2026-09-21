package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// OpenAIAdapter serves OpenAI, Ollama, OpenRouter, and OpenCode Zen/Go chat
// models (base-URL switch) via the chat-completions response_format
// json_schema path.
type OpenAIAdapter struct {
	BaseURL string
	APIKey  string
	Model   string
	Log     func(purpose string, usage TokenUsage)
	// Provider overrides Name() so reused adapters log their own id
	// (usage.provider rows); empty defaults to "openai".
	Provider string
	// ExtraHeaders merge into every request (Referer, session UA, ...).
	ExtraHeaders map[string]string
	// BodyExtra merges into the top-level request body (e.g. OpenRouter
	// provider routing, which headers alone cannot express).
	BodyExtra map[string]any
	// SessionID sets x-opencode-session when non-empty (Zen/Go require it).
	SessionID string
	schema    *Schema
}

func NewOpenAI(baseURL, apiKey, model string, sch *Schema) *OpenAIAdapter {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	// Test-only env override for base URL — no test hooks beyond env config.
	if v := os.Getenv("MAGPIE_BASE_URL"); v != "" {
		baseURL = v
	}
	return &OpenAIAdapter{BaseURL: baseURL, APIKey: apiKey, Model: model, schema: sch}
}

func (o *OpenAIAdapter) Name() string {
	if o.Provider != "" {
		return o.Provider
	}
	return "openai"
}

func (o *OpenAIAdapter) Extract(ctx context.Context, in ExtractInput) (ExtractResult, error) {
	if in.Schema == nil {
		in.Schema = o.schema
	}
	if in.Schema == nil {
		return ExtractResult{}, fmt.Errorf("extract: nil schema")
	}
	doc, err := schemaDoc(in.Schema.Raw)
	if err != nil {
		return ExtractResult{}, err
	}
	call := func(ctx context.Context, system, user string) (string, TokenUsage, error) {
		return o.promptOnce(ctx, system, user, doc)
	}
	return runRepairLoop(ctx, call, o.Log, in, o.Name(), o.Model)
}

// PromptText is the schema-less text path: the same chat-completions call
// with no response_format, so the model returns plain text. See Prompter
// for the caller-logs attribution contract.
func (o *OpenAIAdapter) PromptText(ctx context.Context, system, user string) (string, TokenUsage, error) {
	return o.promptOnce(ctx, system, user, nil)
}

func (o *OpenAIAdapter) promptOnce(ctx context.Context, system, user string, doc any) (string, TokenUsage, error) {
	body := map[string]any{
		"model": o.Model,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": user},
		},
	}
	if doc != nil {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "magpie",
				"strict": true,
				"schema": doc,
			},
		}
	}
	for k, v := range o.BodyExtra {
		body[k] = v
	}
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	if o.SessionID != "" {
		headers[SessionHeader] = o.SessionID
	}
	for k, v := range o.ExtraHeaders {
		headers[k] = v
	}
	out, err := postJSON(ctx, o.BaseURL+"/chat/completions", headers, body)
	if err != nil {
		return "", TokenUsage{}, err
	}
	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int      `json:"prompt_tokens"`
			CompletionTokens int      `json:"completion_tokens"`
			Cost             *float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return "", TokenUsage{}, fmt.Errorf("decode chat-completions: %w", err)
	}
	if len(env.Choices) == 0 {
		return "", TokenUsage{}, fmt.Errorf("empty choices")
	}
	usage := TokenUsage{
		PromptTokens:     env.Usage.PromptTokens,
		CompletionTokens: env.Usage.CompletionTokens,
	}
	if env.Usage.Cost != nil {
		// OpenRouter reports provider-computed cost; canonical, no table.
		usage.USDEstimate = *env.Usage.Cost
	}
	if env.Choices[0].FinishReason == "length" {
		return "", usage, &truncError{"response truncated (finish_reason=length)"}
	}
	return env.Choices[0].Message.Content, usage, nil
}
