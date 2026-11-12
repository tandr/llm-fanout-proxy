package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type wsRoute struct {
	up     *upstream
	native bool
}

func (g *Gateway) websocketRoute(data []byte) (wsRoute, []byte, error) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil || payload == nil {
		return wsRoute{}, nil, errors.New("invalid WebSocket JSON object")
	}
	var kind, model string
	_ = json.Unmarshal(payload["type"], &kind)
	if kind != "response.create" {
		return wsRoute{}, nil, errors.New("first WebSocket event must be response.create")
	}
	if json.Unmarshal(payload["model"], &model) != nil || model == "" {
		return wsRoute{}, nil, errors.New("model is required")
	}
	prefix, modelID, hasPrefix := strings.Cut(model, "/")
	route := wsRoute{native: true}
	if hasPrefix && prefix != "chatgpt" {
		route = wsRoute{up: g.providers[prefix]}
		if route.up == nil || modelID == "" {
			return wsRoute{}, nil, errors.New("unknown provider model prefix")
		}
	} else if g.config.ExternalOnly {
		return wsRoute{}, nil, errors.New("use the native Codex route for OpenAI models")
	}
	if hasPrefix && modelID == "" {
		return wsRoute{}, nil, errors.New("native model is required")
	}
	if hasPrefix && (route.native || route.up.config.Protocol == "responses") {
		payload["model"], _ = json.Marshal(modelID)
		data, _ = json.Marshal(payload)
	}
	return route, data, nil
}

func wsWrite(ctx context.Context, conn *websocket.Conn, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func wsError(ctx context.Context, conn *websocket.Conn, status int, code, message string) {
	data, _ := json.Marshal(map[string]any{"type": "error", "status": status, "error": map[string]string{"type": "invalid_request_error", "code": code, "message": message}})
	_ = wsWrite(ctx, conn, data)
}

func wsRead(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	kind, data, err := conn.Read(ctx)
	if err == nil && kind != websocket.MessageText {
		err = errors.New("WebSocket events must be text")
	}
	return data, err
}

func (g *Gateway) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(requestLimit)
	g.wsMu.Lock()
	if g.closed || len(g.sockets) >= 128 {
		g.wsMu.Unlock()
		_ = conn.CloseNow()
		return
	}
	g.sockets[conn] = struct{}{}
	g.wsMu.Unlock()
	defer func() { _ = conn.CloseNow(); g.wsMu.Lock(); delete(g.sockets, conn); g.wsMu.Unlock() }()
	first, err := wsRead(r.Context(), conn)
	if err != nil {
		return
	}
	route, first, err := g.websocketRoute(first)
	if err != nil {
		wsError(r.Context(), conn, 400, "invalid_request", err.Error())
		return
	}
	if route.up != nil && route.up.websocket != nil {
		route.up.websocket(r.Context(), conn, first)
		return
	}
	g.relayWebSocket(r, conn, route, first)
}

func (g *Gateway) relayWebSocket(r *http.Request, downstream *websocket.Conn, route wsRoute, first []byte) {
	base, _ := parseBase(g.config.Native.BaseURL)
	headers := r.Header.Clone()
	headers.Del("X-Litellm-Api-Key")
	if !route.native {
		base = route.up.base
		headers = providerHeaders(r.Header, route.up.key)
		if beta := r.Header.Get("OpenAI-Beta"); beta != "" {
			headers.Set("OpenAI-Beta", beta)
		}
	}
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(name))
		}
	}
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
			headers.Del(name)
		}
	}
	for _, name := range []string{"Connection", "Upgrade", "Proxy-Authorization", "Proxy-Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Host"} {
		headers.Del(name)
	}
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + "/responses"
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	if base.Scheme == "https" {
		target.Scheme = "wss"
	} else {
		target.Scheme = "ws"
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	dialCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	up, response, err := websocket.Dial(dialCtx, target.String(), &websocket.DialOptions{HTTPClient: g.client, HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled})
	stop()
	if err != nil {
		status := http.StatusBadGateway
		if response != nil {
			status = response.StatusCode
			if status < 400 || status > 599 {
				status = http.StatusBadGateway
			}
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		wsError(ctx, downstream, status, "upstream_error", "upstream WebSocket connection failed")
		return
	}
	defer up.CloseNow()
	up.SetReadLimit(requestLimit)
	if wsWrite(ctx, up, first) != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for {
			data, err := wsRead(ctx, up)
			if err != nil || wsWrite(ctx, downstream, data) != nil {
				return
			}
		}
	}()
	defer func() { cancel(); _ = up.CloseNow(); _ = downstream.CloseNow(); <-done }()
	for {
		data, err := wsRead(ctx, downstream)
		if err != nil {
			return
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &event) != nil {
			wsError(ctx, downstream, 400, "invalid_request", "invalid WebSocket JSON object")
			continue
		}
		if event.Type == "response.create" {
			selected, rewritten, err := g.websocketRoute(data)
			if err != nil || selected != route {
				wsError(ctx, downstream, 400, "invalid_request", "WebSocket connection cannot change upstream; open another connection")
				continue
			}
			data = rewritten
		}
		if wsWrite(ctx, up, data) != nil {
			return
		}
	}
}

// Close releases conversation history and closes hijacked WebSocket connections.
func (g *Gateway) Close() error {
	g.wsMu.Lock()
	g.closed = true
	connections := make([]*websocket.Conn, 0, len(g.sockets))
	for conn := range g.sockets {
		connections = append(connections, conn)
	}
	g.wsMu.Unlock()
	for _, conn := range connections {
		_ = conn.CloseNow()
	}
	g.history.Close()
	return nil
}
