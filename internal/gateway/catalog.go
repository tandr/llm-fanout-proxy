package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

func (g *Gateway) catalog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	catalog := map[string]json.RawMessage{}
	rows := []json.RawMessage{}
	var headers http.Header
	if !g.config.ExternalOnly {
		nativeURL, _ := parseBase(g.config.Native.BaseURL)
		nativeURL.Path = strings.TrimRight(nativeURL.Path, "/") + "/models"
		nativeURL.RawQuery = r.URL.RawQuery
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nativeURL.String(), nil)
		if err != nil {
			writeError(w, 502, "invalid native catalog URL")
			return
		}
		req.Header = r.Header.Clone()
		req.Header.Del("X-Litellm-Api-Key")
		req.Header.Del("If-None-Match")
		req.Header.Del("If-Modified-Since")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := g.client.Do(req)
		if err != nil {
			writeError(w, 502, "native catalog request failed")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}
		headers = resp.Header
		raw, err := readCatalog(resp.Body)
		if err != nil {
			writeError(w, 502, "native "+err.Error())
			return
		}
		if json.Unmarshal(raw, &catalog) != nil || catalog == nil {
			writeError(w, 502, "native model catalog is invalid")
			return
		}
		if json.Unmarshal(catalog["models"], &rows) != nil || rows == nil {
			writeError(w, 502, "native model catalog has no models array")
			return
		}
	}
	seen := map[string]bool{}
	var instructions json.RawMessage
	var messages json.RawMessage
	for _, row := range rows {
		var m map[string]json.RawMessage
		if json.Unmarshal(row, &m) != nil {
			continue
		}
		var slug string
		_ = json.Unmarshal(m["slug"], &slug)
		if slug != "" {
			seen[slug] = true
		}
		var text string
		if len(instructions) == 0 && json.Unmarshal(m["base_instructions"], &text) == nil && text != "" {
			instructions = m["base_instructions"]
		}
		if len(messages) == 0 {
			var value map[string]json.RawMessage
			if json.Unmarshal(m["model_messages"], &value) == nil && json.Unmarshal(value["instructions_template"], &text) == nil && text != "" {
				messages = m["model_messages"]
			}
		}
	}
	for _, cfg := range g.config.Providers {
		up := g.providers[cfg.Prefix]
		u := *up.base
		u.Path = strings.TrimRight(u.Path, "/") + "/models"
		query := r.URL.Query()
		query.Del("client_version")
		u.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			writeError(w, 502, "invalid provider catalog URL")
			return
		}
		req.Header = providerHeaders(r.Header, up.key)
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := g.client.Do(req)
		if err != nil {
			writeError(w, 502, fmt.Sprintf("provider %s catalog request failed", cfg.Prefix))
			return
		}
		raw, readErr := readCatalog(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			writeError(w, 502, fmt.Sprintf("provider %s catalog returned HTTP %d", cfg.Prefix, resp.StatusCode))
			return
		}
		if readErr != nil {
			writeError(w, 502, fmt.Sprintf("provider %s %s", cfg.Prefix, readErr))
			return
		}
		var discovered struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &discovered) != nil || discovered.Data == nil {
			writeError(w, 502, fmt.Sprintf("provider %s model catalog is invalid", cfg.Prefix))
			return
		}
		for _, model := range discovered.Data {
			id := model.ID
			if id == "" {
				writeError(w, 502, fmt.Sprintf("provider %s catalog contains empty model ID", cfg.Prefix))
				return
			}
			id = cfg.Prefix + "/" + id
			if seen[id] {
				continue
			}
			seen[id] = true
			row := catalogRow(id, cfg)
			if len(instructions) > 0 {
				row["base_instructions"] = instructions
			}
			if len(messages) > 0 {
				row["model_messages"] = messages
			}
			encoded, _ := json.Marshal(row)
			rows = append(rows, encoded)
		}
	}
	catalog["models"], _ = json.Marshal(rows)
	body, _ := json.Marshal(catalog)
	copyHeaders(w.Header(), headers)
	for _, name := range []string{"Content-Length", "Content-Encoding", "Etag", "Last-Modified", "Content-Md5", "Digest", "Content-Range"} {
		w.Header().Del(name)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}
func readCatalog(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, catalogLimit+1))
	if err != nil {
		return nil, fmt.Errorf("model catalog could not be read")
	}
	if len(b) > catalogLimit {
		return nil, fmt.Errorf("model catalog exceeds 16 MiB")
	}
	return b, nil
}
func copyHeaders(dst, src http.Header) {
	for _, connection := range src.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			src.Del(strings.TrimSpace(name))
		}
	}
	for name, values := range src {
		switch http.CanonicalHeaderKey(name) {
		case "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}
func catalogRow(id string, cfg Provider) map[string]any {
	row := map[string]any{"slug": id, "display_name": id, "description": nil, "supported_reasoning_levels": []any{}, "shell_type": "unified_exec", "visibility": "list", "supported_in_api": true, "priority": 100, "availability_nux": nil, "upgrade": nil, "support_verbosity": false, "default_verbosity": nil, "apply_patch_tool_type": "freeform", "truncation_policy": map[string]any{"mode": "tokens", "limit": 10000}, "experimental_supported_tools": []any{}, "input_modalities": []string{"text"}, "supports_reasoning_summary_parameter": false, "supports_search_tool": false, "use_responses_lite": false, "multi_agent_version": "v2"}
	rawID := strings.TrimPrefix(id, cfg.Prefix+"/")
	// First matching override wins, allowing specific models before family patterns.
	for _, override := range cfg.ModelOverrides {
		match, _ := path.Match(override.Pattern, rawID)
		if !match {
			continue
		}
		if override.ContextWindow > 0 {
			row["context_window"] = override.ContextWindow
		}
		levels := []any{}
		for _, level := range override.ReasoningLevels {
			levels = append(levels, map[string]string{"effort": level, "description": level})
		}
		row["supported_reasoning_levels"] = levels
		if len(override.ReasoningLevels) > 0 {
			row["default_reasoning_level"] = override.ReasoningLevels[0]
		}
		break
	}
	return row
}
