// Package bridge translates stateless Responses requests to Chat Completions.
package bridge

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxBody = 64 << 20
const maxRecord = 16 << 20

type Config struct{ BaseURL, APIKey, ModelPrefix string }
type handler struct {
	config   Config
	endpoint string
	client   *http.Client
}

func New(config Config, client *http.Client) (http.Handler, error) {
	u, err := url.Parse(config.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("bridge base URL must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("bridge API key is required")
	}
	config.ModelPrefix = strings.TrimSuffix(config.ModelPrefix, "/")
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	// Provider credentials never follow a redirect, including one to another origin.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &handler{config, strings.TrimRight(config.BaseURL, "/") + "/chat/completions", &copyClient}, nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return b, nil
}
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj{"error": obj{"type": "invalid_request_error", "message": message}})
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "bridge requires POST")
		return
	}
	data, err := readBounded(r.Body, maxBody)
	if err != nil {
		writeError(w, 413, "request body is too large or unreadable")
		return
	}
	var request obj
	if err = json.Unmarshal(data, &request); err != nil || request == nil {
		writeError(w, 400, "request must be a JSON object")
		return
	}
	converted, err := convertRequest(request, h.config.ModelPrefix)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	body, err := json.Marshal(converted.chat)
	if err != nil {
		writeError(w, 400, "invalid request")
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.endpoint, strings.NewReader(string(body)))
	if err != nil {
		writeError(w, 500, "cannot create upstream request")
		return
	}
	upstream.Header.Set("Authorization", "Bearer "+h.config.APIKey)
	upstream.Header.Set("Content-Type", "application/json")
	if converted.stream {
		upstream.Header.Set("Accept", "text/event-stream")
	} else {
		upstream.Header.Set("Accept", "application/json")
	}
	response, err := h.client.Do(upstream)
	if err != nil {
		if r.Context().Err() == nil {
			writeError(w, 502, "upstream request failed")
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		if retry := response.Header.Get("Retry-After"); retry != "" && len(retry) <= 128 && !strings.ContainsAny(retry, "\r\n") {
			w.Header().Set("Retry-After", retry)
		}
		writeError(w, status, fmt.Sprintf("upstream returned HTTP %d", response.StatusCode))
		return
	}
	if converted.stream {
		serveStream(w, response.Body, converted)
		return
	}
	data, err = readBounded(response.Body, maxBody)
	if err != nil {
		writeError(w, 502, "upstream response is too large or unreadable")
		return
	}
	result, err := convertResponse(data, converted)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
func newID(prefix string) string { return prefix + rand.Text() }

type obj = map[string]any

func stringValue(v any) string { s, _ := v.(string); return s }
func object(v any) obj         { m, _ := v.(map[string]any); return m }
func array(v any) []any        { a, _ := v.([]any); return a }
