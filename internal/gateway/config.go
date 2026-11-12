package gateway

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen       string       `yaml:"listen"`
	APIKeyEnv    string       `yaml:"api_key_env"`
	Native       NativeConfig `yaml:"native"`
	Providers    []Provider   `yaml:"providers"`
	ExternalOnly bool         `yaml:"-"`
}
type NativeConfig struct {
	BaseURL string `yaml:"base_url"`
}
type Provider struct {
	Prefix         string          `yaml:"prefix"`
	BaseURL        string          `yaml:"base_url"`
	APIKeyEnv      string          `yaml:"api_key_env"`
	Protocol       string          `yaml:"protocol"`
	ModelOverrides []ModelOverride `yaml:"model_overrides,omitempty"`
}
type ModelOverride struct {
	Pattern         string   `yaml:"pattern"`
	ContextWindow   int      `yaml:"context_window,omitempty"`
	ReasoningLevels []string `yaml:"reasoning_levels,omitempty"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Load(path string) (Config, error) {
	var cfg Config
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return cfg, err
	}
	if len(data) > 1<<20 {
		return cfg, fmt.Errorf("config exceeds 1 MiB")
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("config must contain exactly one YAML document")
	}
	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8765"
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return fmt.Errorf("listen must be a loopback host:port")
	}
	if !envName.MatchString(c.APIKeyEnv) {
		return fmt.Errorf("api_key_env must be an environment variable name")
	}
	if c.Native.BaseURL == "" {
		c.Native.BaseURL = "https://chatgpt.com/backend-api/codex"
	}
	if _, err := parseBase(c.Native.BaseURL); err != nil {
		return fmt.Errorf("native base_url: %w", err)
	}
	seen := map[string]bool{"chatgpt": true}
	for _, p := range c.Providers {
		if p.Prefix == "" || strings.ContainsAny(p.Prefix, "/ \\?#") || seen[p.Prefix] {
			return fmt.Errorf("invalid or duplicate provider prefix %q", p.Prefix)
		}
		seen[p.Prefix] = true
		if _, err := parseBase(p.BaseURL); err != nil {
			return fmt.Errorf("provider %s base_url: %w", p.Prefix, err)
		}
		if !envName.MatchString(p.APIKeyEnv) {
			return fmt.Errorf("provider %s api_key_env must be an environment variable name", p.Prefix)
		}
		switch p.Protocol {
		case "responses", "chat_completions":
		default:
			return fmt.Errorf("provider %s has unsupported protocol %q", p.Prefix, p.Protocol)
		}
		for _, m := range p.ModelOverrides {
			if _, err := path.Match(m.Pattern, ""); err != nil || m.Pattern == "" || m.ContextWindow < 0 {
				return fmt.Errorf("provider %s has invalid model override", p.Prefix)
			}
			for _, level := range m.ReasoningLevels {
				switch level {
				case "none", "minimal", "low", "medium", "high", "xhigh", "max":
				default:
					return fmt.Errorf("provider %s has invalid reasoning level %q", p.Prefix, level)
				}
			}
		}
	}
	return nil
}
func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("expected absolute HTTP(S) URL without credentials, query or fragment")
	}
	return u, nil
}
