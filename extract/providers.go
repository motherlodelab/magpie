package extract

import (
	"errors"
	"fmt"
	"strings"
)

// Provider is one LLM backend name plus whether it bills per-call API
// keys (ollama rides the local daemon, codex shells out to the user's
// logged-in CLI — both keyless).
type Provider struct {
	Name     string
	NeedsKey bool
}

// ErrUnknownProvider is wrapped by every unknown-provider error from
// BuildExtractor; callers match with errors.Is (cli maps it to a usage
// exit code, the bridge passes the message through verbatim).
var ErrUnknownProvider = errors.New("extract: unknown provider")

// providers is the canonical set, in ProviderHelp order: the same names
// every surface (cli flags, MCP, desktop Settings) must offer.
var providers = []Provider{
	{Name: "anthropic", NeedsKey: true},
	{Name: "openai", NeedsKey: true},
	{Name: "ollama", NeedsKey: false},
	{Name: "openrouter", NeedsKey: true},
	{Name: "codex", NeedsKey: false},
	{Name: "opencode-go", NeedsKey: true},
	{Name: "opencode-zen", NeedsKey: true},
}

// Providers returns every supported LLM provider in canonical order.
// The one home for the provider list — derive help text, key checks and
// settings UIs from this, never from a hand-copied literal.
func Providers() []Provider {
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// ProviderNames returns just the names, canonical order.
func ProviderNames() []string {
	out := make([]string, len(providers))
	for i, p := range providers {
		out[i] = p.Name
	}
	return out
}

// BuildExtractor constructs the provider adapter for provider (case-
// insensitive). Adapters are lazy — construction dials nothing — except
// codex, whose Preflight shells out at build time so a broken CLI fails
// before any spend.
//
// sessionID is set as x-opencode-session on the Zen/Go adapters (their
// billing header). It is a parameter because SessionID lives on the
// concrete adapters, not on the Extractor interface; callers pass their
// run id.
func BuildExtractor(provider, apiKey, model, sessionID string, sch *Schema,
	log func(string, TokenUsage)) (Extractor, error) {
	// Normalize once: adapters log this id into llm_calls, so raw input
	// casing must never fork the accounting.
	provider = strings.ToLower(provider)
	switch provider {
	case "openai", "ollama":
		a := NewOpenAI("", apiKey, model, sch)
		a.Log = log
		return a, nil
	case "anthropic":
		a := NewAnthropic("", apiKey, model, sch)
		a.Log = log
		return a, nil
	case "openrouter":
		a := NewOpenAI("https://openrouter.ai/api/v1", apiKey, model, sch)
		a.Provider = provider
		a.Log = log
		a.ExtraHeaders = map[string]string{
			"HTTP-Referer": "https://github.com/motherlodelab/magpie",
			"X-Title":      "magpie",
		}
		// Hard schema routing: without require_parameters OpenRouter may
		// route to an upstream that treats the schema as a hint.
		a.BodyExtra = map[string]any{"provider": map[string]any{"require_parameters": true}}
		return a, nil
	case "opencode-go", "opencode-zen":
		return buildZenExtractor(provider, apiKey, model, sessionID, sch, log)
	case "codex":
		a := NewCodexExec(model, sch, log)
		if err := a.Preflight(); err != nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, fmt.Errorf("%w %q (want %s)", ErrUnknownProvider, provider, strings.Join(ProviderNames(), "|"))
	}
}

// buildZenExtractor builds the OpenCode Go/Zen adapter: base URL per
// billing (flat plan vs pay-as-you-go credits), adapter per model prefix,
// session + UA headers on every call.
func buildZenExtractor(provider, apiKey, model, sessionID string, sch *Schema,
	log func(string, TokenUsage)) (Extractor, error) {
	base := "https://opencode.ai/zen/v1"
	if provider == "opencode-go" {
		base = "https://opencode.ai/zen/go/v1"
	}
	headers := map[string]string{"User-Agent": MagpieUA}
	if ZenUsesMessages(model) {
		a := NewAnthropic(base, apiKey, model, sch)
		a.Provider, a.SessionID, a.ExtraHeaders, a.Log = provider, sessionID, headers, log
		return a, nil
	}
	a := NewOpenAI(base, apiKey, model, sch)
	a.Provider, a.SessionID, a.ExtraHeaders, a.Log = provider, sessionID, headers, log
	return a, nil
}
