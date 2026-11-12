package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T, native, provider http.HandlerFunc, protocol string) (*Gateway, *httptest.Server, *httptest.Server) {
	t.Helper()
	t.Setenv("TEST_GATEWAY_KEY", "gateway-secret")
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	n := httptest.NewServer(native)
	t.Cleanup(n.Close)
	var p *httptest.Server
	cfg := Config{APIKeyEnv: "TEST_GATEWAY_KEY", Native: NativeConfig{BaseURL: n.URL + codexPath}}
	if provider != nil {
		p = httptest.NewServer(provider)
		t.Cleanup(p.Close)
		cfg.Providers = []Provider{{Prefix: "vendor", BaseURL: p.URL + "/v1", APIKeyEnv: "TEST_PROVIDER_KEY", Protocol: protocol}}
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g, n, p
}
func request(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Litellm-Api-Key", "Bearer gateway-secret")
	r.Header.Set("Authorization", "Bearer native-secret")
	r.Header.Set("ChatGPT-Account-ID", "account")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestNativePassthrough(t *testing.T) {
	body := " { \"model\" : \"gpt-test\", \"unknown\": [1, 2] } "
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != body {
			t.Errorf("changed body: %s", b)
		}
		if r.URL.Path != codexPath+"/responses" || r.URL.RawQuery != "x=1" {
			t.Errorf("URL %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer native-secret" || r.Header.Get("ChatGPT-Account-ID") != "account" || r.Header.Get("X-Litellm-Api-Key") != "" {
			t.Errorf("native headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: second\n\n")
	}, nil, "")
	w := request(t, g, "POST", codexPath+"/responses?x=1", body)
	if w.Code != 201 || w.Body.String() != "data: first\n\ndata: second\n\n" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
func TestNativeOtherPathAndAlias(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/compact") {
			if string(b) != "opaque bytes" {
				t.Error(string(b))
			}
			w.WriteHeader(418)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["model"] != "gpt-test" {
			t.Errorf("model %v", m)
		}
	}, nil, "")
	if w := request(t, g, "POST", codexPath+"/responses/compact", "opaque bytes"); w.Code != 418 {
		t.Fatal(w.Code)
	}
	if w := request(t, g, "POST", codexPath+"/responses", `{"model":"chatgpt/gpt-test"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestExternalIsolationAndProtocol(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected native call") }, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.URL.RawQuery != "x=1" {
			t.Errorf("URL %s", r.URL)
		}
		for _, name := range []string{"ChatGPT-Account-ID", "X-Litellm-Api-Key", "X-Forwarded-For", "User-Agent", "Cookie", "Origin"} {
			if r.Header.Get(name) != "" && !(name == "User-Agent" && r.Header.Get(name) == "Go-http-client/1.1") {
				t.Errorf("leaked %s", name)
			}
		}
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Error("wrong provider auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "test" {
			t.Errorf("model %v", body["model"])
		}
		w.WriteHeader(429)
		_, _ = io.WriteString(w, "rate limited")
	}, "responses")
	r := httptest.NewRequest("POST", codexPath+"/responses?x=1", strings.NewReader(`{"model":"vendor/test"}`))
	r.Header.Set("X-Litellm-Api-Key", "gateway-secret")
	r.Header.Set("Authorization", "Bearer native-secret")
	r.Header.Set("ChatGPT-Account-ID", "private")
	r.Header.Set("Cookie", "private")
	r.Header.Set("X-Forwarded-For", "private")
	r.Header.Set("Origin", "private")
	r.Header.Set("User-Agent", "private")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 429 || w.Body.String() != "rate limited" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
func TestAuthenticationAndInvalidRoutes(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream") }, nil, "")
	for _, tc := range []struct {
		path, body string
		status     int
	}{{"/other", "", 404}, {codexPath + "/responses", `{"model":"missing/test"}`, 400}, {codexPath + "/responses", "null", 400}, {codexPath + "/responses", "garbage", 400}} {
		if w := request(t, g, "POST", tc.path, tc.body); w.Code != tc.status {
			t.Errorf("%s: %d", tc.path, w.Code)
		}
	}
	for _, key := range []string{"", "wrong", "Bearer gateway-secret extra"} {
		r := httptest.NewRequest("GET", codexPath+"/models", nil)
		r.Header.Set("X-Litellm-Api-Key", key)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Error(w.Code)
		}
	}
	r := httptest.NewRequest("GET", codexPath+"/models", nil)
	r.Header.Set("X-Litellm-Api-Key", "gateway-secret")
	r.Header.Set("Authorization", "Bearer gateway-secret")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, health := range []string{"/health", "/health/readiness"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", health, nil))
		if w.Code != 200 {
			t.Error(w.Code)
		}
	}
}
func TestExternalOnly(t *testing.T) {
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("external route contacted native upstream")
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("provider credentials were not isolated")
		}
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"test"}]}`)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	}, "responses")
	g.config.ExternalOnly = true
	w := request(t, g, "GET", codexPath+"/models", "")
	var catalog struct {
		Models []struct {
			Slug             string
			BaseInstructions string `json:"base_instructions"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil || w.Code != 200 || len(catalog.Models) != 1 || catalog.Models[0].Slug != "vendor/test" {
		t.Fatalf("external catalog: %d %s", w.Code, w.Body.String())
	}
	if catalog.Models[0].BaseInstructions == "" {
		t.Fatal("external catalog lacks the instructions required by Codex")
	}
	if w := request(t, g, "POST", codexPath+"/responses", `{"model":"vendor/test"}`); w.Code != http.StatusAccepted {
		t.Fatalf("external response: %d %s", w.Code, w.Body.String())
	}
	for _, model := range []string{"gpt-test", "chatgpt/gpt-test", "unknown/test"} {
		if w := request(t, g, "POST", codexPath+"/responses", `{"model":"`+model+`"}`); w.Code != 400 {
			t.Fatalf("accepted model %s: %d", model, w.Code)
		}
	}
	if w := request(t, g, "POST", codexPath+"/responses/compact", `{}`); w.Code != 400 {
		t.Fatalf("accepted native endpoint: %d", w.Code)
	}
}
func TestCatalogMergeAndDiscovery(t *testing.T) {
	nativeExtra := strings.Repeat("n", (1<<20)+100)
	calls := 0
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("X-Litellm-Api-Key") != "" {
			t.Errorf("native catalog headers %v", r.Header)
		}
		if r.URL.Query().Get("client_version") != "1" {
			t.Error("lost native query")
		}
		w.Header().Set("ETag", "old")
		w.Header().Set("Last-Modified", "old")
		w.Header().Set("Digest", "old")
		w.Header().Set("X-Native", "keep")
		_ = json.NewEncoder(w).Encode(map[string]any{"native_unknown": nativeExtra, "models": []any{map[string]any{"slug": "gpt-test", "base_instructions": "native instructions", "unknown": 123}, map[string]any{"slug": "vendor/duplicate"}, map[string]any{"unrecognized": "preserve"}}})
	}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" || r.URL.Query().Has("client_version") || r.URL.Query().Get("other") != "yes" {
			t.Errorf("provider discovery URL %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("X-Litellm-Api-Key") != "" {
			t.Errorf("provider discovery headers %v", r.Header)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"duplicate"},{"id":"model"},{"id":"model"}]}`)
	}, "responses")
	g.config.Providers[0].ModelOverrides = []ModelOverride{{Pattern: "model", ContextWindow: 1048576, ReasoningLevels: []string{"low", "high"}}}
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("GET", codexPath+"/models?client_version=1&other=yes", nil)
		r.Header.Set("X-Litellm-Api-Key", "gateway-secret")
		r.Header.Set("Authorization", "Bearer native-secret")
		r.Header.Set("ChatGPT-Account-ID", "private")
		r.Header.Set("If-None-Match", "old")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Etag") != "" || w.Header().Get("Digest") != "" || w.Header().Get("X-Native") != "keep" {
			t.Errorf("merged headers %v", w.Header())
		}
		var result struct {
			NativeUnknown string           `json:"native_unknown"`
			Models        []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.NativeUnknown != nativeExtra || len(result.Models) != 4 || result.Models[0]["unknown"] != float64(123) || result.Models[2]["unrecognized"] != "preserve" {
			t.Fatal("native data lost or duplicate rows")
		}
		m := result.Models[3]
		if m["context_window"] != float64(1048576) || m["base_instructions"] != "native instructions" || m["supports_search_tool"] != false {
			t.Errorf("external row %v", m)
		}
	}
	if calls != 2 {
		t.Fatalf("discovery calls %d", calls)
	}
}
func TestCatalogFailuresAndRedirects(t *testing.T) {
	for _, body := range []string{"bad", `{"data":null}`, `{"data":[{"id":""}]}`} {
		t.Run(body, func(t *testing.T) {
			g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"models":[]}`) }, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }, "responses")
			if w := request(t, g, "GET", codexPath+"/models", ""); w.Code != 502 {
				t.Fatal(w.Code)
			}
		})
	}
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }, nil, "")
	for _, p := range []string{"/models", "/responses"} {
		method, body := "GET", ""
		if p == "/responses" {
			method = "POST"
			body = `{"model":"gpt-test"}`
		}
		if w := request(t, g, method, codexPath+p, body); w.Code != 307 {
			t.Error(w.Code)
		}
	}
	if leaked {
		t.Fatal("followed credential-bearing redirect")
	}
}
func TestStreamCancellation(t *testing.T) {
	canceled := make(chan struct{})
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}, nil, "")
	s := httptest.NewServer(g)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+codexPath+"/responses", strings.NewReader(`{"model":"gpt-test","stream":true}`))
	req.Header.Set("X-Litellm-Api-Key", "gateway-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len("data: first\n\n"))
	if _, err = io.ReadFull(resp.Body, data); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
}
func TestLimitsAndConfig(t *testing.T) {
	if _, err := readCatalog(io.LimitReader(zeroReader{}, catalogLimit+1)); err == nil {
		t.Fatal("oversize catalog accepted")
	}
	good := Config{APIKeyEnv: "GATEWAY", Native: NativeConfig{BaseURL: "https://example.test"}}
	if err := good.Validate(); err != nil || good.Listen != "127.0.0.1:8765" {
		t.Fatal(err)
	}
	for _, p := range []Provider{{Prefix: "chatgpt", BaseURL: "https://example.test", APIKeyEnv: "KEY", Protocol: "responses"}, {Prefix: "vendor", BaseURL: "https://user:secret@example.test", APIKeyEnv: "KEY", Protocol: "responses"}, {Prefix: "vendor", BaseURL: "https://example.test", APIKeyEnv: "KEY", Protocol: "invalid"}} {
		cfg := good
		cfg.Providers = []Provider{p}
		if cfg.Validate() == nil {
			t.Error("invalid provider accepted")
		}
	}
	bad := good
	bad.Listen = "0.0.0.0:8765"
	if bad.Validate() == nil {
		t.Fatal("public listen accepted")
	}
	f, err := os.CreateTemp(t.TempDir(), "config")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("api_key_env: KEY\nunknown: 1\n")
	_ = f.Close()
	if _, err := Load(f.Name()); err == nil {
		t.Fatal("unknown field accepted")
	}
	g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { t.Error("oversized request reached upstream") }, nil, "")
	r := httptest.NewRequest("POST", codexPath+"/responses", io.MultiReader(bytes.NewReader([]byte(`{"model":"gpt-test","padding":"`)), io.LimitReader(zeroReader{}, requestLimit)))
	r.Header.Set("X-Litellm-Api-Key", "gateway-secret")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
