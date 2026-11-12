package gateway

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"llm-fanout-proxy/internal/bridge"
)

const codexPath = "/backend-api/codex"
const requestLimit = 64 << 20
const catalogLimit = 16 << 20

type Gateway struct {
	config    Config
	key       string
	native    *httputil.ReverseProxy
	providers map[string]*upstream
	client    *http.Client
}
type upstream struct {
	config  Provider
	key     string
	base    *url.URL
	handler http.Handler
}

func New(cfg Config) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	key := os.Getenv(cfg.APIKeyEnv)
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("environment variable %s is empty", cfg.APIKeyEnv)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 2 * time.Minute
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	g := &Gateway{config: cfg, key: key, providers: map[string]*upstream{}, client: client}
	base, _ := parseBase(cfg.Native.BaseURL)
	g.native = proxy(base, codexPath, "", false, transport)
	for _, p := range cfg.Providers {
		pk := os.Getenv(p.APIKeyEnv)
		if strings.TrimSpace(pk) == "" {
			return nil, fmt.Errorf("environment variable %s is empty", p.APIKeyEnv)
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(pk)) == 1 {
			return nil, fmt.Errorf("provider %s key must differ from gateway key", p.Prefix)
		}
		u, _ := parseBase(p.BaseURL)
		up := &upstream{config: p, key: pk, base: u}
		if p.Protocol == "chat_completions" {
			h, err := bridge.New(bridge.Config{BaseURL: p.BaseURL, APIKey: pk, ModelPrefix: p.Prefix}, client)
			if err != nil {
				return nil, fmt.Errorf("provider %s: %w", p.Prefix, err)
			}
			up.handler = h
		} else {
			up.handler = proxy(u, codexPath, pk, true, transport)
		}
		g.providers[p.Prefix] = up
	}
	return g, nil
}

func proxy(base *url.URL, strip, key string, external bool, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(p *httputil.ProxyRequest) {
		p.Out.URL.Scheme = base.Scheme
		p.Out.URL.Host = base.Host
		p.Out.URL.RawQuery = p.In.URL.RawQuery
		p.Out.Host = base.Host
		p.Out.URL.Path = strings.TrimRight(base.Path, "/") + strings.TrimPrefix(p.In.URL.Path, strip)
		p.Out.URL.RawPath = strings.TrimRight(base.EscapedPath(), "/") + strings.TrimPrefix(p.In.URL.EscapedPath(), strip)
		p.Out.Header.Del("X-Litellm-Api-Key")
		if external {
			p.Out.Header = providerHeaders(p.In.Header, key)
		}
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, http.StatusBadGateway, "upstream request failed")
	}}
}
func providerHeaders(in http.Header, key string) http.Header {
	h := make(http.Header)
	for _, name := range []string{"Content-Type", "Accept"} {
		if values := in.Values(name); len(values) > 0 {
			h[name] = append([]string(nil), values...)
		}
	}
	h.Set("Authorization", "Bearer "+key)
	return h
}
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}
func token(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 1 {
		return parts[0]
	}
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if (r.URL.Path == "/health" || r.URL.Path == "/health/readiness") && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
		return
	}
	supplied := token(r.Header.Get("X-Litellm-Api-Key"))
	if supplied == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(g.key)) != 1 {
		writeError(w, 401, "invalid gateway credential")
		return
	}
	if r.URL.Path != codexPath && !strings.HasPrefix(r.URL.Path, codexPath+"/") {
		writeError(w, 404, "unknown endpoint")
		return
	}
	// A gateway credential must never become a native upstream credential.
	if native := token(r.Header.Get("Authorization")); native != "" && subtle.ConstantTimeCompare([]byte(native), []byte(g.key)) == 1 {
		writeError(w, 401, "native and gateway credentials must differ")
		return
	}
	if r.URL.Path == codexPath+"/models" && r.Method == http.MethodGet {
		g.catalog(w, r)
		return
	}
	if g.config.ExternalOnly && (r.URL.Path != codexPath+"/responses" || r.Method != http.MethodPost) {
		writeError(w, 400, "external gateway supports only configured provider models")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestLimit))
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			writeError(w, 413, "request body exceeds 64 MiB")
		} else {
			writeError(w, 400, "request body could not be read")
		}
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if r.URL.Path == codexPath+"/responses" && r.Method == http.MethodPost {
		var payload map[string]json.RawMessage
		if json.Unmarshal(body, &payload) != nil {
			writeError(w, 400, "invalid Responses JSON object")
			return
		}
		var model string
		if json.Unmarshal(payload["model"], &model) != nil || model == "" {
			writeError(w, 400, "model is required")
			return
		}
		prefix, modelID, hasPrefix := strings.Cut(model, "/")
		if hasPrefix && prefix != "chatgpt" {
			up := g.providers[prefix]
			if up == nil || modelID == "" {
				writeError(w, 400, "unknown provider model prefix")
				return
			}
			if up.config.Protocol == "responses" {
				payload["model"], _ = json.Marshal(modelID)
				body, _ = json.Marshal(payload)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header = providerHeaders(r.Header, up.key)
			up.handler.ServeHTTP(w, r)
			return
		}
		if g.config.ExternalOnly {
			writeError(w, 400, "use the native Codex route for OpenAI models")
			return
		}
		if hasPrefix {
			if modelID == "" {
				writeError(w, 400, "native model is required")
				return
			}
			payload["model"], _ = json.Marshal(modelID)
			body, _ = json.Marshal(payload)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
	}
	g.native.ServeHTTP(w, r)
}
