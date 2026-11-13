package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func decode(t *testing.T, r io.Reader) obj {
	t.Helper()
	var v obj
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
func bridgeFor(t *testing.T, upstream http.HandlerFunc) http.Handler {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	h, err := New(Config{BaseURL: server.URL + "/v1", APIKey: "upstream-only-key", ModelPrefix: "lithos"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}
func call(t *testing.T, h http.Handler, request obj) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://gateway/backend-api/codex/responses", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer native-secret")
	r.Header.Set("ChatGPT-Account-ID", "account-secret")
	r.Header.Set("X-Litellm-Api-Key", "gateway-secret")
	r.Header.Set("Cookie", "session-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func request(input any) obj { return obj{"model": "lithos/example", "input": input} }
func function(name string) obj {
	return obj{"type": "function", "name": name, "description": "A tool", "parameters": obj{"type": "object", "properties": obj{"cmd": obj{"type": "string"}}, "required": []string{"cmd"}}}
}
func completion(content any, reasoning string, calls []any, finish string) obj {
	return obj{"choices": []any{obj{"index": 0, "message": obj{"role": "assistant", "content": content, "reasoning_content": reasoning, "tool_calls": calls}, "finish_reason": finish}}, "usage": obj{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
}
func chatTool(id, name, args string) obj {
	return obj{"id": id, "type": "function", "function": obj{"name": name, "arguments": args}}
}
func sendJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
func TestNonstreamTextAndParameters(t *testing.T) {
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer upstream-only-key" {
			t.Errorf("incorrect upstream route/auth")
		}
		for _, key := range []string{"ChatGPT-Account-ID", "Cookie", "X-Litellm-Api-Key"} {
			if r.Header.Get(key) != "" {
				t.Errorf("leaked %s", key)
			}
		}
		body := decode(t, r.Body)
		if body["model"] != "example" || body["reasoning_effort"] != "high" || body["max_tokens"] != float64(50) {
			t.Errorf("incorrect parameter mapping: %v", body)
		}
		for _, key := range []string{"client_metadata", "metadata", "prompt_cache_key", "include"} {
			if body[key] != nil {
				t.Errorf("leaked %s", key)
			}
		}
		if object(body["response_format"])["type"] != "json_schema" {
			t.Error("missing response schema")
		}
		sendJSON(w, completion("hello", "", nil, "stop"))
	})
	r := request("Hi")
	r["reasoning"] = obj{"effort": "high", "summary": "auto"}
	r["text"] = obj{"format": obj{"type": "json_schema", "name": "answer", "schema": obj{"type": "object"}, "strict": true}}
	r["max_output_tokens"] = 50
	r["client_metadata"] = obj{"secret": "private"}
	r["metadata"] = obj{"secret": "private"}
	r["prompt_cache_key"] = "private"
	r["include"] = []string{"reasoning.encrypted_content"}
	r["stream_options"] = obj{"include_usage": true, "include_obfuscation": true}
	w := call(t, h, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	result := decode(t, w.Body)
	if result["model"] != "lithos/example" || result["status"] != "completed" || object(result["usage"])["total_tokens"] != float64(15) {
		t.Fatal(result)
	}
	if object(array(object(array(result["output"])[0])["content"])[0])["text"] != "hello" {
		t.Fatal(result)
	}
}
func TestReasoningToolRoundTrip(t *testing.T) {
	var turns int
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		turns++
		body := decode(t, r.Body)
		if turns == 1 {
			name := object(object(array(body["tools"])[0])["function"])["name"].(string)
			sendJSON(w, completion(nil, "plan carefully", []any{chatTool("call-1", name, `{"cmd":"pwd"}`)}, "tool_calls"))
			return
		}
		messages := array(body["messages"])
		assistant := object(messages[1])
		if assistant["reasoning_content"] != "plan carefully" || len(array(assistant["tool_calls"])) != 1 || object(messages[2])["tool_call_id"] != "call-1" {
			t.Errorf("lost tool reasoning/history: %v", messages)
		}
		sendJSON(w, completion("done", "checked result", nil, "stop"))
	})
	r := request("Use shell")
	r["tools"] = []any{function("shell")}
	first := call(t, h, r)
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	result := decode(t, first.Body)
	output := array(result["output"])
	input := []any{obj{"role": "user", "content": "Use shell"}}
	input = append(input, output...)
	input = append(input, obj{"type": "function_call_output", "call_id": "call-1", "output": "/tmp"})
	r["input"] = input
	second := call(t, h, r)
	if second.Code != 200 {
		t.Fatalf("second turn: %s", second.Body.String())
	}
	if turns != 2 {
		t.Fatal(turns)
	}
}
func TestNamespaceCollisionAndCustomRoundTrip(t *testing.T) {
	custom := obj{"type": "custom", "name": "exec", "description": "Run code", "format": obj{"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}}
	r := request("hi")
	r["tools"] = []any{function("ns__exec"), obj{"type": "namespace", "name": "ns", "tools": []any{function("exec"), custom}}}
	c, err := convertRequest(r, "lithos")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.tools.byWire) != 3 {
		t.Fatal(c.tools)
	}
	for _, raw := range c.tools.declarations {
		f := object(object(raw)["function"])
		if strings.Contains(stringValue(f["description"]), "```lark") {
			if object(object(f["parameters"])["properties"])["content"] == nil {
				t.Fatal(f)
			}
		}
	}
	target := c.tools.byKey[toolKey("ns", "exec", "custom")]
	responseBytes, _ := json.Marshal(completion(nil, "", []any{chatTool("custom-1", target.wire, `{"content":"print(1)"}`)}, "tool_calls"))
	response, err := convertResponse(responseBytes, c)
	if err != nil {
		t.Fatal(err)
	}
	item := object(array(response["output"])[0])
	if item["type"] != "custom_tool_call" || item["namespace"] != "ns" || item["name"] != "exec" || item["input"] != "print(1)" {
		t.Fatal(item)
	}
	r["input"] = []any{obj{"role": "user", "content": "hi"}, item, obj{"type": "custom_tool_call_output", "call_id": "custom-1", "output": "1"}}
	next, err := convertRequest(r, "lithos")
	if err != nil {
		t.Fatal(err)
	}
	call := object(array(object(array(next.chat["messages"])[1])["tool_calls"])[0])
	if stringValue(object(call["function"])["arguments"]) != `{"content":"print(1)"}` {
		t.Fatal(call)
	}
	// Tool history remains translatable when its declaration has been removed.
	delete(r, "tools")
	if _, err := convertRequest(r, "lithos"); err != nil {
		t.Fatal(err)
	}
}
func TestImagesAndParallelToolOutputs(t *testing.T) {
	r := request([]any{
		obj{"role": "user", "content": []any{obj{"type": "input_image", "image_url": "data:image/png;base64,YQ==", "detail": "high"}}},
		obj{"type": "function_call", "name": "a", "call_id": "a1", "arguments": "{}"}, obj{"type": "function_call", "name": "b", "call_id": "b1", "arguments": "{}"},
		obj{"type": "function_call_output", "call_id": "a1", "output": []any{obj{"type": "input_text", "text": "screenshot"}, obj{"type": "input_image", "image_url": "https://example.test/a.png"}}},
		obj{"type": "function_call_output", "call_id": "b1", "output": "done"},
	})
	c, err := convertRequest(r, "lithos")
	if err != nil {
		t.Fatal(err)
	}
	messages := array(c.chat["messages"])
	if len(messages) != 5 || object(messages[2])["role"] != "tool" || object(messages[3])["role"] != "tool" || object(messages[4])["role"] != "user" {
		t.Fatal(messages)
	}
	if len(array(object(messages[1])["tool_calls"])) != 2 {
		t.Fatal(messages)
	}
	image := object(array(object(messages[0])["content"])[0])
	if object(image["image_url"])["url"] != "data:image/png;base64,YQ==" {
		t.Fatal(image)
	}
	if !strings.Contains(stringValue(object(array(object(messages[4])["content"])[0])["text"]), "a1") {
		t.Fatal(messages)
	}
}
func eventData(t *testing.T, text, kind string) []obj {
	t.Helper()
	result := []obj{}
	for _, frame := range strings.Split(text, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var v obj
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
					t.Fatal(err)
				}
				if v["type"] == kind {
					result = append(result, v)
				}
			}
		}
	}
	return result
}
func chunk(w io.Writer, v any) { b, _ := json.Marshal(v); _, _ = fmt.Fprintf(w, "data: %s\n\n", b) }
func delta(d obj, finish any) obj {
	return obj{"choices": []any{obj{"index": 0, "delta": d, "finish_reason": finish}}}
}
func fragment(index int, id, name, args string) obj {
	return obj{"index": index, "id": id, "type": "function", "function": obj{"name": name, "arguments": args}}
}
func TestStreamingInterleavedParallelCalls(t *testing.T) {
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		_ = decode(t, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk(w, delta(obj{"reasoning_content": "plan ", "content": "working "}, nil))
		chunk(w, delta(obj{"tool_calls": []any{fragment(0, "a", "tool_", `{"cmd":`), fragment(1, "b", "tool_1", `{"cmd":"b`)}}, nil))
		chunk(w, delta(obj{"reasoning_content": "done", "content": "now", "tool_calls": []any{fragment(1, "", "", `"}`), fragment(0, "", "0", `"a"}`)}}, nil))
		chunk(w, delta(obj{}, "tool_calls"))
		chunk(w, obj{"choices": []any{}, "usage": obj{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	r := request("run")
	r["stream"] = true
	r["tools"] = []any{function("a"), function("b")}
	w := call(t, h, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	text := w.Body.String()
	if len(eventData(t, text, "response.completed")) != 1 || len(eventData(t, text, "response.failed")) != 0 {
		t.Fatal(text)
	}
	items := eventData(t, text, "response.output_item.done")
	if len(items) != 4 {
		t.Fatal(text)
	}
	first := object(items[2]["item"])
	second := object(items[3]["item"])
	if first["name"] != "a" || first["arguments"] != `{"cmd":"a"}` || second["name"] != "b" || second["arguments"] != `{"cmd":"b"}` {
		t.Fatal(items)
	}
	if len(eventData(t, text, "response.output_text.delta")) != 2 || len(eventData(t, text, "response.reasoning_summary_text.delta")) != 2 {
		t.Fatal(text)
	}
}
func TestStreamFailureAndLength(t *testing.T) {
	cases := []struct {
		name, finish string
		done         bool
		event        string
	}{
		{"premature_eof", "stop", false, "response.failed"}, {"length", "length", true, "response.incomplete"}, {"normal", "stop", true, "response.completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
				chunk(w, delta(obj{"content": "partial"}, tc.finish))
				if tc.done {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
			})
			r := request("hi")
			r["stream"] = true
			w := call(t, h, r)
			if len(eventData(t, w.Body.String(), tc.event)) != 1 {
				t.Fatal(w.Body.String())
			}
			if tc.event != "response.completed" && len(eventData(t, w.Body.String(), "response.completed")) != 0 {
				t.Fatal(w.Body.String())
			}
		})
	}
}
func TestStreamAboveScannerDefaultLimit(t *testing.T) {
	text := strings.Repeat("x", 100000)
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		chunk(w, delta(obj{"content": text}, "stop"))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	r := request("hi")
	r["stream"] = true
	w := call(t, h, r)
	events := eventData(t, w.Body.String(), "response.output_text.delta")
	if len(events) != 1 || events[0]["delta"] != text {
		t.Fatal("large SSE record lost")
	}
}
func TestUnsupportedRejectedBeforeUpstream(t *testing.T) {
	var requests atomic.Int32
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	cases := []obj{
		{"previous_response_id": "resp_old"}, {"store": true}, {"background": true}, {"truncation": "auto"}, {"text": obj{"verbosity": "high"}}, {"context_management": []any{}},
		{"input": []any{obj{"type": "compaction", "encrypted_content": "opaque"}}}, {"input": []any{obj{"type": "reasoning", "summary": []any{}, "encrypted_content": "opaque"}}},
		{"input": []any{obj{"role": "user", "content": []any{obj{"type": "input_audio", "audio_url": "data:audio/wav;base64,a"}}}}},
		{"tools": []any{obj{"type": "web_search"}}}, {"tools": []any{obj{"type": "tool_search"}}},
	}
	for i, patch := range cases {
		r := request("hi")
		for k, v := range patch {
			r[k] = v
		}
		w := call(t, h, r)
		if w.Code != 400 {
			t.Errorf("case %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if requests.Load() != 0 {
		t.Fatal("unsupported request reached upstream")
	}
}
func TestUpstreamFailuresAndRedirectIsolation(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"http_error", "bad_json", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "http_error":
					w.WriteHeader(429)
					_, _ = io.WriteString(w, "provider secret")
				case "bad_json":
					_, _ = io.WriteString(w, "{")
				case "redirect":
					http.Redirect(w, r, target.URL, 307)
				}
			})
			w := call(t, h, request("hi"))
			expected := 502
			if mode == "http_error" {
				expected = 429
			}
			if w.Code != expected || strings.Contains(w.Body.String(), "provider secret") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("followed credential-bearing redirect")
	}
}
func TestCancellationReachesUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "http://gateway/responses", strings.NewReader(`{"model":"lithos/example","input":"hi"}`)).WithContext(ctx)
	finished := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(finished) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never started")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not cancelled")
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not return")
	}
}
func TestConfigAndBounds(t *testing.T) {
	for _, base := range []string{"relative", "ftp://example.test", "http://user:password@example.test", "http://example.test?a=b"} {
		if _, err := New(Config{BaseURL: base, APIKey: "key"}, nil); err == nil {
			t.Errorf("accepted %s", base)
		}
	}
	if _, err := readBounded(strings.NewReader("123456"), 5); err == nil {
		t.Fatal("missing body bound")
	}
}

func TestLengthWithTruncatedToolNeverExecutes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					chunk(w, delta(obj{"tool_calls": []any{fragment(0, "partial", "tool_0", `{"cmd":`)}}, "length"))
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				} else {
					sendJSON(w, completion(nil, "", []any{chatTool("partial", "tool_0", `{"cmd":`)}, "length"))
				}
			})
			r := request("hi")
			r["tools"] = []any{function("shell")}
			r["stream"] = stream
			w := call(t, h, r)
			if stream {
				if len(eventData(t, w.Body.String(), "response.incomplete")) != 1 || len(eventData(t, w.Body.String(), "response.output_item.done")) != 0 {
					t.Fatal(w.Body.String())
				}
			} else {
				result := decode(t, w.Body)
				if result["status"] != "incomplete" || len(array(result["output"])) != 0 {
					t.Fatal(result)
				}
			}
		})
	}
}

func TestPlaintextCollaborationOutput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ namespace, name string }{{"collaboration", "spawn_agent"}, {"collaboration", "send_message"}, {"collaboration", "followup_task"}, {"collaboration", "other"}, {"other", "spawn_agent"}} {
			name := tc.name
			t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.namespace, name, stream), func(t *testing.T) {
				h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
					if stream {
						chunk(w, delta(obj{"tool_calls": []any{fragment(0, "c", "tool_0", `{"message":"hello"}`)}}, nil))
						chunk(w, delta(obj{}, "tool_calls"))
						_, _ = io.WriteString(w, "data: [DONE]\n\n")
					} else {
						sendJSON(w, completion(nil, "", []any{chatTool("c", "tool_0", `{"message":"hello"}`)}, "tool_calls"))
					}
				})
				req := request("hi")
				req["stream"] = stream
				req["tools"] = []any{obj{"type": "namespace", "name": tc.namespace, "tools": []any{function(name)}}}
				w := call(t, h, req)
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				var item obj
				if stream {
					events := eventData(t, w.Body.String(), "response.output_item.done")
					if len(events) != 1 {
						t.Fatal(w.Body.String())
					}
					item = object(events[0]["item"])
				} else {
					item = object(array(decode(t, w.Body)["output"])[0])
				}
				args, present := item["encrypted_function_args"]
				if name == "other" || tc.namespace != "collaboration" {
					if present {
						t.Fatal(item)
					}
				} else if list, ok := args.([]any); !present || !ok || len(list) != 0 {
					t.Fatal(item)
				}
			})
		}
	}
}
