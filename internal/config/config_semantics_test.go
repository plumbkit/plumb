package config

import "testing"

func TestSemanticsResolve_PresetDefaults(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	r := SemanticsConfig{Provider: "openai", Enabled: true}.Resolve()
	if r.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("base_url = %q", r.BaseURL)
	}
	if r.Model != "text-embedding-3-large" {
		t.Errorf("model = %q", r.Model)
	}
	if r.APIKey != "env-key" {
		t.Errorf("key = %q, want env-key", r.APIKey)
	}
	if r.RerankCandidates != 50 || r.Timeout.Seconds() != 10 {
		t.Errorf("defaults not applied: rc=%d timeout=%s", r.RerankCandidates, r.Timeout)
	}
}

func TestSemanticsResolve_ConfigKeyWinsOverEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	r := SemanticsConfig{Provider: "openai", APIKey: "config-key"}.Resolve()
	if r.APIKey != "config-key" {
		t.Errorf("config api_key must win over env; got %q", r.APIKey)
	}
}

func TestSemanticsResolve_NamedEnvVar(t *testing.T) {
	t.Setenv("MY_VOYAGE_KEY", "vk")
	r := SemanticsConfig{Provider: "voyage", APIKeyEnv: "MY_VOYAGE_KEY"}.Resolve()
	if r.APIKey != "vk" {
		t.Errorf("key from named env var = %q, want vk", r.APIKey)
	}
	if r.Model != "voyage-code-3" {
		t.Errorf("voyage default model = %q", r.Model)
	}
}

func TestSemanticsResolve_Custom(t *testing.T) {
	r := SemanticsConfig{Provider: "custom", BaseURL: "http://localhost:11434/v1", Model: "nomic-embed-text"}.Resolve()
	if r.BaseURL != "http://localhost:11434/v1" || r.Model != "nomic-embed-text" {
		t.Errorf("custom not honoured: %+v", r)
	}
	if r.APIKey != "" {
		t.Errorf("custom with no key env should resolve empty key; got %q", r.APIKey)
	}
}

func TestSemanticsResolve_EmptyProviderDefaultsOpenAI(t *testing.T) {
	r := SemanticsConfig{}.Resolve()
	if r.Provider != "openai" || r.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("empty provider should default to openai; got %+v", r)
	}
}

func TestKeySourceEnv(t *testing.T) {
	if got := (SemanticsConfig{Provider: "voyage"}).KeySourceEnv(); got != "VOYAGE_API_KEY" {
		t.Errorf("KeySourceEnv = %q, want VOYAGE_API_KEY", got)
	}
	if got := (SemanticsConfig{Provider: "openai", APIKeyEnv: "X"}).KeySourceEnv(); got != "X" {
		t.Errorf("explicit api_key_env should win; got %q", got)
	}
}

func TestValidateSemantics(t *testing.T) {
	if err := validateSemantics(SemanticsConfig{Provider: "bogus"}); err == nil {
		t.Error("bogus provider should fail validation")
	}
	if err := validateSemantics(SemanticsConfig{Provider: "custom", Enabled: true}); err == nil {
		t.Error("custom + enabled without base_url should fail")
	}
	if err := validateSemantics(SemanticsConfig{Provider: "openai", RerankCandidates: 50}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
