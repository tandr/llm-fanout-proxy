package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadYAML(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	text := `api_key_env: GATEWAY_KEY
providers:
  - prefix: vendor
    base_url: https://example.test/v1
    api_key_env: PROVIDER_KEY
    protocol: chat_completions
    model_overrides:
      - pattern: deepseek-ai/DeepSeek-V4.1-Flash*
        context_window: 1048576
        reasoning_levels: [none, low, medium, high, xhigh, max]
`
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8765" || cfg.Native.BaseURL != "https://chatgpt.com/backend-api/codex" || cfg.Providers[0].ModelOverrides[0].ContextWindow != 1048576 {
		t.Fatalf("decoded %+v", cfg)
	}
	for _, text := range []string{
		"api_key_env: KEY\nunknown: 1\n",
		"api_key_env: FIRST\napi_key_env: SECOND\n",
		"api_key_env: KEY\n---\napi_key_env: OTHER\n",
		"api_key_env: KEY\n---\n",
		"api_key_env: BAD-ENV-NAME\n",
		"api_key_env: KEY\nproviders:\n  - prefix: vendor\n    base_url: https://example.test\n    api_key_env: BAD-NAME\n    protocol: responses\n",
		"api_key_env: KEY\n" + strings.Repeat(" ", 1<<20) + "invalid: trailing\n",
	} {
		if err := os.WriteFile(file, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(file); err == nil {
			t.Errorf("accepted invalid YAML/config prefix %.80q", text)
		}
	}
}
