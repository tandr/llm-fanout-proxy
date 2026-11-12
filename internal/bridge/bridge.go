// Package bridge translates Responses requests to Chat Completions.
package bridge

import (
	"context"
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

type Config struct {
	BaseURL, APIKey, ModelPrefix string
	History                      *History
}
type handler struct {
	config   Config
	endpoint string
	client   *http.Client
}

func New(config Config, client *http.Client) (*handler, error) {
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
	if config.History == nil {
		config.History = NewHistory()
	}
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
	converted, err := h.prepare(request)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	events := &eventWriter{w: w, complete: converted.save}
	result, err := h.execute(r.Context(), converted, events)
	if err != nil {
		if r.Context().Err() == nil {
			writeRequestError(w, err)
		}
		return
	}
	if !converted.stream {
		converted.save(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

func writeRequestError(w http.ResponseWriter, err error) {
	status, detail := errorObject(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj{"error": detail})
}

func (h *handler) execute(ctx context.Context, converted *convertedRequest, events *eventWriter) (obj, error) {
	body, err := json.Marshal(converted.chat)
	if err != nil {
		return nil, invalid("invalid request")
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, upstreamError("cannot create upstream request")
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
		return nil, upstreamError("upstream request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		if events != nil && events.w != nil {
			if retry := response.Header.Get("Retry-After"); retry != "" && len(retry) <= 128 && !strings.ContainsAny(retry, "\r\n") {
				events.w.Header().Set("Retry-After", retry)
			}
		}
		return nil, &requestError{status: status, message: fmt.Sprintf("upstream returned HTTP %d", response.StatusCode)}
	}
	if converted.stream {
		serveStream(response.Body, converted, events)
		return nil, nil
	}
	data, err := readBounded(response.Body, maxBody)
	if err != nil {
		return nil, upstreamError("upstream response is too large or unreadable")
	}
	result, err := convertResponse(data, converted)
	if err != nil {
		return nil, upstreamError(err.Error())
	}
	return result, nil
}

func newID(prefix string) string { return prefix + rand.Text() }

type obj = map[string]any

func stringValue(v any) string { s, _ := v.(string); return s }
func object(v any) obj         { m, _ := v.(map[string]any); return m }
func array(v any) []any        { a, _ := v.([]any); return a }
