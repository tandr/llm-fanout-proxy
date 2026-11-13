package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistoryBranchesDoNotShareItems(t *testing.T) {
	h := NewHistory()
	defer h.Close()
	item := func(s string) any { return obj{"content": s} }
	h.put("p", "m", "", []any{item("q")}, []any{item("a")})
	h.put("b1", "m", "p", []any{item("q"), item("a"), item("x1")}, []any{item("y1")})
	h.put("b2", "m", "p", []any{item("q"), item("a"), item("x2")}, []any{item("y2")})
	got, ok := h.get("b2", "m")
	if !ok {
		t.Fatal("missing branch snapshot")
	}
	var order []string
	for _, value := range got {
		order = append(order, stringValue(object(value)["content"]))
	}
	if strings.Join(order, ",") != "q,a,x2,y2" {
		t.Fatalf("sibling items leaked into branch: %v", order)
	}
}

func TestHistoryOversizedConversationStopsCaching(t *testing.T) {
	h := NewHistory()
	defer h.Close()
	big := strings.Repeat("x", 6<<20)
	h.put("r1", "m", "", []any{big}, nil)
	h.put("r2", "m", "r1", []any{big, big}, nil)
	h.put("r3", "m", "r2", []any{big, big, big}, nil)
	if _, ok := h.get("r2", "m"); !ok {
		t.Fatal("resident snapshot lost at the size limit")
	}
	if _, ok := h.get("r3", "m"); ok {
		t.Fatal("oversized conversation cached")
	}
	// Recovery sends the full transcript without a parent; it is still too large.
	h.put("r4", "m", "", []any{big, big, big}, nil)
	if s := h.Stats(); s["oversized"] != 2 || s["bytes"] > 12<<20+8 {
		t.Fatalf("unexpected state past the size limit: %v", s)
	}
}

func TestHistoryConcurrentAccess(t *testing.T) {
	h := NewHistory()
	defer h.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id := fmt.Sprintf("g%d-%d", g, i)
				h.put(id, "m", "", []any{obj{"content": id}}, nil)
				if _, ok := h.get(id, "m"); !ok {
					t.Errorf("lost concurrent snapshot %s", id)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if s := h.Stats(); s["entries"] != 400 || s["hits"] != 400 {
		t.Fatalf("unexpected counters: %v", s)
	}
}

func TestHistorySharesConversationPrefix(t *testing.T) {
	h := NewHistory()
	defer h.Close()
	item := func(s string) any { return obj{"content": s} }
	h.put("r1", "m", "", []any{item("u1")}, []any{item("a1")})
	h.put("r2", "m", "r1", []any{item("u1"), item("a1"), item("u2")}, []any{item("a2")})
	h.put("r3", "m", "r2", []any{item("u1"), item("a1"), item("u2"), item("a2"), item("u3")}, []any{item("a3")})
	got, ok := h.get("r3", "m")
	if !ok {
		t.Fatal("missing snapshot")
	}
	var order []string
	for _, value := range got {
		order = append(order, stringValue(object(value)["content"]))
	}
	if strings.Join(order, ",") != "u1,a1,u2,a2,u3,a3" {
		t.Fatalf("snapshot order: %v", order)
	}
	// Six unique items are retained once with five commas, not twelve items across three snapshots.
	unique := 6 * len(`{"content":"ux"}`)
	if s := h.Stats(); s["entries"] != 3 || s["bytes"] != unique+5 {
		t.Fatalf("conversation was not shared: %v", s)
	}
	// A parent that is no longer resident must fall back to the merged input, not a truncated snapshot.
	h.put("r9", "m", "gone", []any{item("x1"), item("x2")}, nil)
	if got, ok := h.get("r9", "m"); !ok || len(got) != 2 {
		t.Fatalf("fallback snapshot: %v %v", got, ok)
	}
}

func TestHistoryExpiryEvictionAndIsolation(t *testing.T) {
	h := NewHistory()
	defer h.Close()
	now := time.Unix(100, 0)
	h.now = func() time.Time { return now }
	h.put("a", "one/model", "", []any{obj{"role": "user", "content": "a"}}, nil)
	now = now.Add(30 * time.Minute)
	h.put("b", "one/model", "", nil, []any{obj{"role": "assistant", "content": "b"}})
	if _, ok := h.get("a", "two/model"); ok {
		t.Fatal("cross-provider history exposed")
	}
	now = now.Add(time.Minute)
	input, ok := h.get("a", "one/model")
	if !ok {
		t.Fatal("missing fresh history")
	}
	object(input[0])["content"] = "mutated"
	if again, _ := h.get("a", "one/model"); object(again[0])["content"] != "a" {
		t.Fatal("snapshot was mutable")
	}
	now = now.Add(31 * time.Minute)
	if _, ok := h.get("a", "one/model"); !ok {
		t.Fatal("access did not refresh idle lifetime")
	}
	now = now.Add(time.Hour)
	if _, ok := h.get("a", "one/model"); ok {
		t.Fatal("expired history survived")
	}
	h.maxBytes = 60
	h.put("d", "one/model", "", []any{obj{"content": strings.Repeat("d", 40)}}, nil)
	now = now.Add(time.Second)
	h.put("e", "one/model", "", []any{obj{"content": strings.Repeat("e", 40)}}, nil)
	if _, ok := h.get("d", "one/model"); ok {
		t.Fatal("byte limit did not evict")
	}
	h.put("huge", "one/model", "", []any{strings.Repeat("x", 100)}, nil)
	if _, ok := h.get("huge", "one/model"); ok {
		t.Fatal("oversized snapshot cached")
	}
	h.Close()
	h.put("closed", "one/model", "", nil, nil)
	if _, ok := h.get("closed", "one/model"); ok {
		t.Fatal("closed cache accepted data")
	}
	if s := h.Stats(); s["hits"] != 3 || s["misses"] != 5 || s["evictions"] != 1 || s["oversized"] != 1 {
		t.Fatalf("unexpected counters: %v", s)
	}
}

func TestHistoryContinuationAfterParentExpires(t *testing.T) {
	h := NewHistory()
	defer h.Close()
	now := time.Unix(100, 0)
	h.now = func() time.Time { return now }
	h.put("parent", "model", "", []any{"question"}, []any{"answer"})
	h.timer.Stop()
	// Model work outlasts the parent TTL before the expiry callback runs.
	now = now.Add(historyTTL)
	want := []any{"question", "answer", "followup", "next answer"}
	h.put("child", "model", "parent", want[:3], want[3:])
	got, ok := h.get("child", "model")
	encoded, _ := json.Marshal(want)
	actual, _ := json.Marshal(got)
	if !ok || string(actual) != string(encoded) {
		t.Fatalf("continuation lost history: found=%v got=%s want=%s", ok, actual, encoded)
	}
	if s := h.Stats(); s["entries"] != 1 || s["bytes"] != len(encoded)-2 {
		t.Fatalf("incorrect accounting after parent expiry: %v", s)
	}
	now = now.Add(historyTTL)
	if _, ok := h.get("child", "model"); ok {
		t.Fatal("expired continuation survived")
	}
	if s := h.Stats(); s["entries"] != 0 || s["bytes"] != 0 {
		t.Fatalf("incorrect accounting after continuation expiry: %v", s)
	}
}

func TestHistoryRemovesIdleDataWithoutTraffic(t *testing.T) {
	h := NewHistory()
	defer h.Close()
	h.ttl = 10 * time.Millisecond
	h.put("idle", "model", "", []any{"private"}, nil)
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.entries) != 0 || h.bytes != 0 {
		t.Fatal("idle plaintext retained after expiry")
	}
}

func TestHTTPContinuationToolsInstructionsAndBranches(t *testing.T) {
	var requests []obj
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		body := decode(t, r.Body)
		requests = append(requests, body)
		if len(requests) == 1 {
			name := stringValue(object(object(array(body["tools"])[0])["function"])["name"])
			sendJSON(w, completion(nil, "plan", []any{chatTool("call-1", name, `{"cmd":"pwd"}`)}, "tool_calls"))
		} else {
			sendJSON(w, completion(fmt.Sprintf("answer-%d", len(requests)), "", nil, "stop"))
		}
	})
	first := request("question")
	first["instructions"] = "old instructions"
	first["tools"] = []any{obj{"type": "namespace", "name": "shell", "tools": []any{function("run")}}}
	parent := decode(t, call(t, h, first).Body)
	second := request([]any{obj{"type": "function_call_output", "call_id": "call-1", "output": "/home"}})
	second["previous_response_id"] = parent["id"]
	second["instructions"] = "new instructions"
	w := call(t, h, second)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	messages := array(requests[1]["messages"])
	if len(messages) != 4 || object(messages[0])["content"] != "new instructions" || object(messages[2])["reasoning_content"] != "plan" || object(messages[3])["tool_call_id"] != "call-1" || requests[1]["tools"] != nil {
		t.Fatalf("bad reconstructed context: %v", requests[1])
	}
	branch := request("other branch")
	branch["previous_response_id"] = parent["id"]
	branch["input"] = []any{obj{"type": "function_call_output", "call_id": "call-1", "output": "branch result"}, obj{"role": "user", "content": "other branch"}}
	w = call(t, h, branch)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	serialized := fmt.Sprint(requests[2])
	if strings.Contains(serialized, "instructions") || strings.Contains(serialized, "answer-2") || strings.Contains(serialized, "/home") || !strings.Contains(serialized, "branch result") {
		t.Fatal("branch or instructions leaked:", serialized)
	}
	branch["model"] = "lithos/other-model"
	w = call(t, h, branch)
	if w.Code != 400 || object(decode(t, w.Body)["error"])["code"] != "previous_response_not_found" || len(requests) != 3 {
		t.Fatal("cross-model continuation accepted")
	}
}

func TestHTTPIncompleteContinuationAndFailedStream(t *testing.T) {
	var calls int
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := decode(t, r.Body)
		if calls == 1 {
			sendJSON(w, completion("partial", "", nil, "length"))
			return
		}
		if calls == 2 {
			if !strings.Contains(fmt.Sprint(body["messages"]), "partial") {
				t.Error("incomplete output absent")
			}
			sendJSON(w, completion("done", "", nil, "stop"))
			return
		}
		_, _ = fmt.Fprint(w, "data: {invalid}\n\n")
	})
	parent := decode(t, call(t, h, request("question")).Body)
	next := obj{"model": "lithos/example", "previous_response_id": parent["id"]}
	if w := call(t, h, next); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	failed := request("fail")
	failed["stream"] = true
	w := call(t, h, failed)
	var failedID string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			var event obj
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err == nil && event["type"] == "response.created" {
				failedID = stringValue(object(event["response"])["id"])
			}
		}
	}
	if failedID == "" || !strings.Contains(w.Body.String(), "response.failed") {
		t.Fatal(w.Body.String())
	}
	next["previous_response_id"] = failedID
	if w := call(t, h, next); w.Code != 400 || calls != 3 {
		t.Fatal("failed response was cached")
	}
}

func TestReasoningAcrossContinuation(t *testing.T) {
	turns := 0
	h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
		turns++
		body := decode(t, r.Body)
		if turns == 3 {
			messages := array(body["messages"])
			first, second := object(messages[1]), object(messages[2])
			if first["reasoning_content"] != "plan1" || second["reasoning_content"] != "plan2" {
				t.Errorf("reasoning reassigned between messages: first=%v second=%v", first, second)
			}
		}
		finish := "stop"
		if turns == 1 {
			finish = "length"
		}
		sendJSON(w, completion(fmt.Sprintf("answer%d", turns), fmt.Sprintf("plan%d", turns), nil, finish))
	})
	parent := decode(t, call(t, h, request("question")).Body)
	child := decode(t, call(t, h, obj{"model": "lithos/example", "previous_response_id": parent["id"]}).Body)
	third := request("continue")
	third["previous_response_id"] = child["id"]
	if w := call(t, h, third); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestAgentMessageDuringParallelTools(t *testing.T) {
	for _, withImage := range []bool{false, true} {
		t.Run(fmt.Sprintf("image=%v", withImage), func(t *testing.T) {
			var output any = "first result"
			if withImage {
				output = []any{obj{"type": "input_text", "text": "first result"}, obj{"type": "input_image", "image_url": "https://example.com/image.png"}}
			}
			input := []any{
				obj{"type": "function_call", "call_id": "a", "name": "run", "arguments": "{}"},
				obj{"type": "custom_tool_call", "call_id": "b", "name": "exec", "input": "second command"},
				obj{"type": "function_call_output", "call_id": "a", "output": output},
				obj{"type": "agent_message", "author": "worker", "recipient": "root", "content": "worker finished"},
				obj{"role": "user", "content": "user steering"},
				obj{"type": "custom_tool_call_output", "call_id": "b", "output": "second result"},
			}
			converted, err := convertRequest(request(input), "lithos")
			if err != nil {
				t.Fatal(err)
			}
			messages := array(converted.chat["messages"])
			wantCount := 5
			if withImage {
				wantCount++
			}
			if len(messages) != wantCount {
				t.Fatalf("lost messages: %v", messages)
			}
			if len(array(object(messages[0])["tool_calls"])) != 2 || object(messages[1])["tool_call_id"] != "a" || object(messages[2])["tool_call_id"] != "b" {
				t.Fatalf("tool results not grouped with calls: %v", messages)
			}
			if !strings.Contains(stringValue(object(messages[1])["content"]), "first result") || object(messages[2])["content"] != "second result" {
				t.Fatalf("lost results: %v", messages)
			}
			next := 3
			if withImage {
				content := array(object(messages[next])["content"])
				if len(content) != 2 || object(object(content[1])["image_url"])["url"] != "https://example.com/image.png" {
					t.Fatalf("lost image: %v", content)
				}
				next++
			}
			agent := array(object(messages[next])["content"])
			if len(agent) != 2 || object(agent[0])["text"] != "Agent message from worker to root:" || object(agent[1])["text"] != "worker finished" {
				t.Fatalf("lost agent message: %v", agent)
			}
			if object(array(object(messages[next+1])["content"])[0])["text"] != "user steering" {
				t.Fatalf("lost steering: %v", messages)
			}
		})
	}
}

func TestReasoningOnlyIncompleteContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, followup := range []string{"none", "user", "agent"} {
			t.Run(fmt.Sprintf("stream=%v/followup=%s", stream, followup), func(t *testing.T) {
				calls := 0
				h := bridgeFor(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					body := decode(t, r.Body)
					if calls == 1 {
						if stream {
							fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"unfinished plan\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
						} else {
							sendJSON(w, completion(nil, "unfinished plan", nil, "length"))
						}
						return
					}
					messages := array(body["messages"])
					want := 2
					if followup != "none" {
						want++
					}
					if len(messages) != want || object(messages[1])["role"] != "assistant" || object(messages[1])["reasoning_content"] != "unfinished plan" {
						t.Errorf("reasoning lost or assigned to another turn: %v", messages)
					}
					sendJSON(w, completion("answer", "", nil, "stop"))
				})
				first := request("question")
				first["stream"] = stream
				w := call(t, h, first)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				var parent obj
				if stream {
					for _, line := range strings.Split(w.Body.String(), "\n") {
						if strings.HasPrefix(line, "data: ") {
							var event obj
							if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.incomplete" {
								parent = object(event["response"])
							}
						}
					}
				} else {
					parent = decode(t, w.Body)
				}
				if parent["status"] != "incomplete" {
					t.Fatal("missing incomplete response", parent)
				}
				next := obj{"model": "lithos/example", "previous_response_id": parent["id"]}
				switch followup {
				case "user":
					next["input"] = "continue"
				case "agent":
					next["input"] = []any{obj{"type": "agent_message", "author": "worker", "recipient": "root", "content": "continue"}}
				}
				if got := call(t, h, next); got.Code != 200 {
					t.Fatal(got.Code, got.Body.String())
				}
				if calls != 2 {
					t.Fatalf("continuation did not reach upstream: %d calls", calls)
				}
			})
		}
	}
}
