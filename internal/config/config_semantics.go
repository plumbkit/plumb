package config

import (
	"os"
	"time"
)

// semanticsPreset holds a provider's default endpoint, model, and key env var.
type semanticsPreset struct {
	baseURL string
	model   string
	keyEnv  string
}

// SemanticsPresets are the built-in providers. "custom" carries no defaults —
// the user supplies base_url (and model) for a self-run OpenAI-compatible
// endpoint (Ollama / llama.cpp / LM Studio / TEI / vLLM). Cohere uses a distinct
// wire format handled by an adapter in internal/semantics.
var semanticsPresets = map[string]semanticsPreset{
	"openai":  {"https://api.openai.com/v1", "text-embedding-3-large", "OPENAI_API_KEY"},
	"voyage":  {"https://api.voyageai.com/v1", "voyage-code-3", "VOYAGE_API_KEY"},
	"jina":    {"https://api.jina.ai/v1", "jina-embeddings-v3", "JINA_API_KEY"},
	"mistral": {"https://api.mistral.ai/v1", "mistral-embed", "MISTRAL_API_KEY"},
	"cohere":  {"https://api.cohere.com/v2", "embed-v4.0", "COHERE_API_KEY"},
	"custom":  {"", "", ""},
}

// SemanticsProviders lists the recognised provider names (for the TUI cycle and
// docs), in display order.
var SemanticsProviders = []string{"openai", "voyage", "jina", "mistral", "cohere", "custom"}

// ResolvedSemantics is SemanticsConfig with provider presets applied and the
// API key resolved — the form the embedder client consumes. It carries no
// config types, so internal/semantics need not import this package.
type ResolvedSemantics struct {
	Enabled          bool
	Provider         string
	BaseURL          string
	Model            string
	APIKey           string
	RerankCandidates int
	Timeout          time.Duration
}

// Resolve applies the provider preset defaults and resolves the API key.
//
// Key precedence: a literal api_key in config wins; otherwise the key is read
// from the environment variable named by api_key_env (or the preset's default
// env var). The provider name is normalised to "openai" when empty.
func (s SemanticsConfig) Resolve() ResolvedSemantics {
	provider := s.Provider
	if provider == "" {
		provider = "openai"
	}
	p := semanticsPresets[provider]

	baseURL := s.BaseURL
	if baseURL == "" {
		baseURL = p.baseURL
	}
	model := s.Model
	if model == "" {
		model = p.model
	}

	key := s.APIKey // config api_key wins over the env var
	if key == "" {
		env := s.APIKeyEnv
		if env == "" {
			env = p.keyEnv
		}
		if env != "" {
			key = os.Getenv(env)
		}
	}

	rc := s.RerankCandidates
	if rc <= 0 {
		rc = 50
	}
	timeout := s.Timeout.Duration
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return ResolvedSemantics{
		Enabled:          s.Enabled,
		Provider:         provider,
		BaseURL:          baseURL,
		Model:            model,
		APIKey:           key,
		RerankCandidates: rc,
		Timeout:          timeout,
	}
}

// KeySourceEnv returns the env var name the key would be read from when no
// literal api_key is set — for the TUI's "key detected" status line.
func (s SemanticsConfig) KeySourceEnv() string {
	if s.APIKeyEnv != "" {
		return s.APIKeyEnv
	}
	provider := s.Provider
	if provider == "" {
		provider = "openai"
	}
	return semanticsPresets[provider].keyEnv
}
