package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type wsObject = map[string]any

func openWS(t *testing.T, g *Gateway) *websocket.Conn {
	t.Helper()
	s := httptest.NewServer(g)
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headers := http.Header{"X-Litellm-Api-Key": {"gateway-secret"}, "Authorization": {"Bearer native-secret"}, "Chatgpt-Account-Id": {"account"}, "Openai-Beta": {"responses_websockets=2026-02-06"}, "Cookie": {"private-cookie"}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+codexPath+"/responses?test=1", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(requestLimit)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func sendWS(t *testing.T, c *websocket.Conn, event wsObject) {
	t.Helper()
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func nextWS(t *testing.T, c *websocket.Conn) wsObject {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var event wsObject
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func terminalWS(t *testing.T, c *websocket.Conn) wsObject {
	t.Helper()
	for {
		event := nextWS(t, c)
		switch event["type"] {
		case "response.completed", "response.incomplete", "response.failed", "error":
			return event
		}
	}
}

func chatAnswer(w http.ResponseWriter, request wsObject, text string) {
	if request["stream"] == true {
		chunk := wsObject{"choices": []any{wsObject{"index": 0, "delta": wsObject{"content": text}, "finish_reason": "stop"}}}
		b, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	} else {
		_ = json.NewEncoder(w).Encode(wsObject{"choices": []any{wsObject{"index": 0, "message": wsObject{"content": text}, "finish_reason": "stop"}}})
	}
}

func decodeObject(t *testing.T, r *http.Request) wsObject {
	t.Helper()
	var body wsObject
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Error(err)
	}
	return body
}

func TestWebSocketPrewarmAndHTTPContinuation(t *testing.T) {
	var calls atomic.Int32
	g, _, _ := setup(t, func(http.ResponseWriter, *http.Request) { t.Error("native route used") }, func(w http.ResponseWriter, r *http.Request) {
		body := decodeObject(t, r)
		n := calls.Add(1)
		if n == 2 {
			messages := fmt.Sprint(body["messages"])
			if !strings.Contains(messages, "original question") || !strings.Contains(messages, "HTTP answer") || !strings.Contains(messages, "prewarm input") || strings.Contains(messages, "prewarm instruction") || strings.Contains(messages, "original instruction") {
				t.Errorf("bad warm continuation: %v", body)
			}
		}
		if n == 3 && !strings.Contains(fmt.Sprint(body["messages"]), "WS answer") {
			t.Error("HTTP lost WS history")
		}
		text := "HTTP answer"
		if body["stream"] == true {
			text = "WS answer"
		}
		chatAnswer(w, body, text)
	}, "chat_completions")
	g.config.ExternalOnly = true
	w := request(t, g, "POST", codexPath+"/responses", `{"model":"vendor/test","input":"original question","instructions":"original instruction"}`)
	var first wsObject
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	c := openWS(t, g)
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "prewarm input", "instructions": "prewarm instruction", "previous_response_id": first["id"], "generate": false, "stream": true, "store": false})
	warm := terminalWS(t, c)
	if warm["type"] != "response.completed" || calls.Load() != 1 {
		t.Fatal(warm, calls.Load())
	}
	warmResponse := warm["response"].(map[string]any)
	if len(warmResponse["output"].([]any)) != 0 {
		t.Fatal("prewarm produced output")
	}
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": []any{}, "previous_response_id": warmResponse["id"]})
	response := terminalWS(t, c)
	if response["type"] != "response.completed" || calls.Load() != 2 {
		t.Fatal(response, calls.Load())
	}
	id := response["response"].(map[string]any)["id"]
	body, _ := json.Marshal(wsObject{"model": "vendor/test", "input": "final question", "previous_response_id": id})
	if w := request(t, g, "POST", codexPath+"/responses", string(body)); w.Code != 200 || calls.Load() != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "new", "previous_response_id": "expired-id"})
	missing := terminalWS(t, c)
	if missing["type"] != "error" || missing["status"] != float64(400) || missing["error"].(map[string]any)["code"] != "previous_response_not_found" || calls.Load() != 3 {
		t.Fatal(missing)
	}
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "full recovered context"})
	if recovered := terminalWS(t, c); recovered["type"] != "response.completed" || calls.Load() != 4 {
		t.Fatal(recovered)
	}
}

func TestWebSocketInterruptDisconnectAndQueueBound(t *testing.T) {
	canceled := make(chan string, 3)
	g, _, _ := setup(t, func(http.ResponseWriter, *http.Request) { t.Error("native route used") }, func(w http.ResponseWriter, r *http.Request) {
		body := decodeObject(t, r)
		text := fmt.Sprint(body["messages"])
		if strings.Contains(text, "hold") {
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			canceled <- text
			return
		}
		chatAnswer(w, body, "done")
	}, "chat_completions")
	c := openWS(t, g)
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "hold one"})
	created := nextWS(t, c)
	id := created["response"].(map[string]any)["id"]
	for event := nextWS(t, c); event["type"] != "response.output_text.delta"; event = nextWS(t, c) {
	}
	sendWS(t, c, wsObject{"type": "response.interrupt", "response_id": "wrong", "mode": "discard_partial_items"})
	if failed := nextWS(t, c); failed["type"] != "response.interrupt.failed" {
		t.Fatal(failed)
	}
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "queued"})
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "queue overflow"})
	if rejected := nextWS(t, c); rejected["type"] != "error" || rejected["status"] != float64(429) {
		t.Fatal(rejected)
	}
	sendWS(t, c, wsObject{"type": "response.interrupt", "response_id": id, "mode": "discard_partial_items"})
	if ack := nextWS(t, c); ack["type"] != "response.interrupt.accepted" || ack["response_id"] != id {
		t.Fatal(ack)
	}
	interrupted := nextWS(t, c)
	if interrupted["type"] != "response.incomplete" {
		t.Fatal(interrupted)
	}
	r := interrupted["response"].(map[string]any)
	if r["incomplete_details"].(map[string]any)["reason"] != "interrupted" || len(r["output"].([]any)) != 0 {
		t.Fatal(r)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt did not cancel upstream")
	}
	if queued := terminalWS(t, c); queued["type"] != "response.completed" {
		t.Fatal(queued)
	}
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "hold disconnect"})
	for event := nextWS(t, c); event["type"] != "response.output_text.delta"; event = nextWS(t, c) {
	}
	_ = c.CloseNow()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel upstream")
	}
}

func TestWebSocketResponsesRelayAndHeaderIsolation(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint("native=", native), func(t *testing.T) {
			upstream := func(w http.ResponseWriter, r *http.Request) {
				wantAuth := "Bearer provider-secret"
				wantPath := "/v1/responses"
				if native {
					wantAuth = "Bearer native-secret"
					wantPath = codexPath + "/responses"
				}
				if r.URL.Path != wantPath || r.URL.RawQuery != "test=1" || r.Header.Get("Authorization") != wantAuth || r.Header.Get("X-Litellm-Api-Key") != "" {
					t.Errorf("bad relay route/auth: %s", r.URL)
				}
				if !native && (r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("Cookie") != "") {
					t.Error("external header leak")
				}
				if native && r.Header.Get("ChatGPT-Account-ID") != "account" {
					t.Error("native account removed")
				}
				if r.Header.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" {
					t.Error("WS protocol header removed")
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for i := 0; i < 2; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_, data, err := conn.Read(ctx)
					cancel()
					if err != nil {
						t.Error(err)
						return
					}
					var message wsObject
					_ = json.Unmarshal(data, &message)
					if message["model"] != "test" || message["opaque"] != "preserved" || message["generate"] != false {
						t.Errorf("native request was translated: %v", message)
					}
					data = []byte(`{"type":"response.completed","response":{"id":"native-id","opaque":"unchanged"}}`)
					if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
						t.Error(err)
						return
					}
				}
			}
			var g *Gateway
			if native {
				g, _, _ = setup(t, upstream, nil, "")
			} else {
				g, _, _ = setup(t, func(http.ResponseWriter, *http.Request) { t.Error("native route used") }, upstream, "responses")
			}
			c := openWS(t, g)
			model := "vendor/test"
			other := "chatgpt/test"
			if native {
				model = "chatgpt/test"
				other = "unknown/test"
			}
			for i := 0; i < 2; i++ {
				sendWS(t, c, wsObject{"type": "response.create", "model": model, "input": []any{}, "opaque": "preserved", "generate": false})
				if event := terminalWS(t, c); event["type"] != "response.completed" || event["response"].(map[string]any)["opaque"] != "unchanged" {
					t.Fatal(event)
				}
				if i == 0 {
					sendWS(t, c, wsObject{"type": "response.create", "model": other, "input": []any{}})
					if event := nextWS(t, c); event["type"] != "error" {
						t.Fatal(event)
					}
				}
			}
		})
	}
}

func TestWebSocketAuthUnsupportedModesAndShutdown(t *testing.T) {
	g, _, _ := setup(t, func(http.ResponseWriter, *http.Request) { t.Error("native route used") }, func(w http.ResponseWriter, r *http.Request) { chatAnswer(w, decodeObject(t, r), "done") }, "chat_completions")
	g.config.ExternalOnly = true
	s := httptest.NewServer(g)
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+codexPath+"/responses", nil)
	if err == nil || response == nil || response.StatusCode != 401 {
		t.Fatal("unauthenticated upgrade accepted")
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	c := openWS(t, g)
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "hi", "stream_id": "lane"})
	if event := terminalWS(t, c); event["type"] != "error" {
		t.Fatal(event)
	}
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "hi", "generate": "false"})
	if event := terminalWS(t, c); event["type"] != "error" {
		t.Fatal(event)
	}
	idle := openWS(t, g)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []*websocket.Conn{c, idle} {
		if _, _, err := conn.Read(ctx); err == nil {
			t.Fatal("shutdown left hijacked socket open")
		}
	}
}
