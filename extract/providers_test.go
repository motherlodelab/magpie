package extract

import (
	"errors"
	"strings"
	"testing"
)

func TestProviders_CanonicalSet(t *testing.T) {
	want := []struct {
		name     string
		needsKey bool
	}{
		{"anthropic", true}, {"openai", true}, {"ollama", false},
		{"openrouter", true}, {"codex", false},
		{"opencode-go", true}, {"opencode-zen", true},
	}
	got := Providers()
	if len(got) != len(want) {
		t.Fatalf("Providers() len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].NeedsKey != w.needsKey {
			t.Errorf("Providers()[%d] = {%s %v}, want {%s %v}", i, got[i].Name, got[i].NeedsKey, w.name, w.needsKey)
		}
	}
	// The seam Providers feeds (help text) must spell the same set.
	if help := strings.Join(ProviderNames(), "|"); help != "anthropic|openai|ollama|openrouter|codex|opencode-go|opencode-zen" {
		t.Errorf("ProviderNames() = %q", help)
	}
}

func TestBuildExtractor_ConstructsPerProvider(t *testing.T) {
	// MAGPIE_BASE_URL (test-only override) may be set in the shell from
	// unrelated debugging — neutralize it so constructor args assert true.
	t.Setenv("MAGPIE_BASE_URL", "")
	log := func(string, TokenUsage) {}
	const session = "run-123"

	t.Run("openai-family", func(t *testing.T) {
		for _, name := range []string{"openai", "ollama", "openrouter", "OpenAI"} {
			a, err := BuildExtractor(name, "k", "m", session, nil, log)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			oa, ok := a.(*OpenAIAdapter)
			if !ok {
				t.Fatalf("%s: type %T, want *OpenAIAdapter", name, a)
			}
			if oa.Log == nil {
				t.Errorf("%s: Log not attached — llm_calls rows would be lost", name)
			}
			if name == "openrouter" {
				if oa.BaseURL != "https://openrouter.ai/api/v1" {
					t.Errorf("openrouter baseURL = %q", oa.BaseURL)
				}
				if oa.Provider != "openrouter" {
					t.Errorf("openrouter Provider = %q", oa.Provider)
				}
				if oa.ExtraHeaders["X-Title"] != "magpie" {
					t.Error("openrouter: X-Title header not set")
				}
				prov, _ := oa.BodyExtra["provider"].(map[string]any)
				if prov["require_parameters"] != true {
					t.Error("openrouter: require_parameters routing missing — schema would be a hint")
				}
			}
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		a, err := BuildExtractor("anthropic", "k", "m", session, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.(*AnthropicAdapter); !ok {
			t.Fatalf("type %T, want *AnthropicAdapter", a)
		}
	})
	t.Run("zen-routing-and-session", func(t *testing.T) {
		// opencode-zen (credits) + a /messages model → Anthropic-shaped.
		a, err := BuildExtractor("opencode-zen", "k", "claude-sonnet-4", session, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		an, ok := a.(*AnthropicAdapter)
		if !ok {
			t.Fatalf("zen+anthropic model: type %T, want Anthropic leg", a)
		}
		if an.BaseURL != "https://opencode.ai/zen/v1" {
			t.Errorf("zen base = %q", an.BaseURL)
		}
		if an.SessionID != session {
			t.Errorf("zen SessionID = %q, want %q (x-opencode-session billing header)", an.SessionID, session)
		}
		if an.ExtraHeaders["User-Agent"] != MagpieUA {
			t.Error("zen: session UA header not set")
		}
		// opencode-go (flat plan) + a /chat/completions model → OpenAI-shaped.
		a, err = BuildExtractor("opencode-go", "k", "gpt-x", session, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		oa, ok := a.(*OpenAIAdapter)
		if !ok {
			t.Fatalf("go+openai model: type %T, want OpenAI leg", a)
		}
		if oa.BaseURL != "https://opencode.ai/zen/go/v1" {
			t.Errorf("go base = %q, want the Go (flat-plan) URL", oa.BaseURL)
		}
		if oa.SessionID != session {
			t.Errorf("go SessionID = %q, want %q", oa.SessionID, session)
		}
	})
	// Codex Preflight shells out to the codex binary. PATH is emptied so
	// the binary is guaranteed absent on any machine — an assertion that
	// loosens when the binary IS installed would be an environment flake,
	// and flakes are bugs. The contract: loud wrapped error, never silent,
	// never a panic.
	t.Run("codex-never-silent", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		a, err := BuildExtractor("codex", "", "m", session, nil, log)
		if err == nil {
			t.Fatalf("codex with no binary on PATH built %T — preflight skipped?", a)
		}
		if !strings.Contains(err.Error(), "codex") {
			t.Errorf("codex error should name the CLI: %v", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		_, err := BuildExtractor("gpt5-turbo", "k", "m", session, nil, log)
		if !errors.Is(err, ErrUnknownProvider) {
			t.Fatalf("err = %v, want ErrUnknownProvider", err)
		}
		if !strings.Contains(err.Error(), "anthropic") {
			t.Errorf("error should name a known provider: %v", err)
		}
	})
}
