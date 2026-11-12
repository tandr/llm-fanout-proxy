package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWebSocketInterruptRetainsDeliveredToolCalls(t *testing.T) {
	g, _, _ := setup(t, func(http.ResponseWriter, *http.Request) { t.Error("native route used") }, func(w http.ResponseWriter, r *http.Request) {
		body := decodeObject(t, r)
		if body["stream"] != true {
			messages := fmt.Sprint(body["messages"])
			if !strings.Contains(messages, "first") || strings.Contains(messages, "second") {
				t.Error("interrupted continuation lost the completed call or retained unfinished output")
			}
			chatAnswer(w, body, "continued")
			return
		}
		tool := func(index int, id, args string) wsObject {
			return wsObject{"index": index, "id": id, "type": "function", "function": wsObject{"name": "tool_0", "arguments": args}}
		}
		args, _ := json.Marshal(wsObject{"payload": strings.Repeat("x", 8<<20)})
		chunk := wsObject{"choices": []any{wsObject{"index": 0, "delta": wsObject{"tool_calls": []any{
			tool(0, "first", `{"payload":"first"}`), tool(1, "second", string(args)),
		}}, "finish_reason": "tool_calls"}}}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}, "chat_completions")
	c := openWS(t, g)
	sendWS(t, c, wsObject{"type": "response.create", "model": "vendor/test", "input": "question", "tools": []any{
		wsObject{"type": "function", "name": "run", "parameters": wsObject{"type": "object"}},
	}})
	var id any
	for {
		event := nextWS(t, c)
		if event["type"] == "response.created" {
			id = event["response"].(map[string]any)["id"]
		}
		if event["type"] == "response.output_item.done" {
			if event["item"].(map[string]any)["call_id"] != "first" {
				t.Fatal("first tool call was not delivered first")
			}
			break
		}
	}
	sendWS(t, c, wsObject{"type": "response.interrupt", "response_id": id, "mode": "discard_partial_items"})
	// Let the interrupt wait behind the unfinished second call's socket backpressure.
	time.Sleep(50 * time.Millisecond)
	interrupted := terminalWS(t, c)
	if interrupted["type"] != "response.incomplete" {
		t.Fatal("response completed before interruption")
	}
	output := interrupted["response"].(map[string]any)["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["call_id"] != "first" {
		t.Fatalf("interruption did not retain exactly the delivered tool call: %d items", len(output))
	}
	body, _ := json.Marshal(wsObject{"model": "vendor/test", "previous_response_id": id, "input": []any{
		wsObject{"type": "function_call_output", "call_id": "first", "output": "aborted before dispatch"},
	}})
	if w := request(t, g, "POST", codexPath+"/responses", string(body)); w.Code != http.StatusOK {
		t.Fatalf("completed call missing from interrupted history: %d %s", w.Code, w.Body.String())
	}
}
