package bridge

import (
	"encoding/json"
	"fmt"
	"time"
)

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type chatCall struct {
	Index    *int         `json:"index"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}
type chatMessage struct {
	Content          any        `json:"content"`
	ReasoningContent string     `json:"reasoning_content"`
	ToolCalls        []chatCall `json:"tool_calls"`
	Refusal          string     `json:"refusal"`
}
type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	Delta        chatMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}
type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   obj          `json:"usage"`
	Error   any          `json:"error"`
}

func textFromChat(content any) (string, error) {
	if content == nil {
		return "", nil
	}
	if s, ok := content.(string); ok {
		return s, nil
	}
	parts, ok := content.([]any)
	if !ok {
		return "", fmt.Errorf("unsupported upstream content")
	}
	text := ""
	for _, raw := range parts {
		p := object(raw)
		if p["type"] != "text" || p["text"] == nil {
			return "", fmt.Errorf("unsupported upstream content part")
		}
		s, ok := p["text"].(string)
		if !ok {
			return "", fmt.Errorf("upstream text is invalid")
		}
		text += s
	}
	return text, nil
}
func messageItem(id, text string) obj {
	return obj{"id": id, "type": "message", "role": "assistant", "status": "completed", "content": []any{obj{"type": "output_text", "text": text, "annotations": []any{}}}}
}
func reasoningItem(id, text string) obj {
	return obj{"id": id, "type": "reasoning", "summary": []any{obj{"type": "summary_text", "text": text}}, "content": []any{obj{"type": "reasoning_text", "text": text}}}
}
func finishStatus(reason string) (string, any, error) {
	switch reason {
	case "stop", "tool_calls":
		return "completed", nil, nil
	case "length":
		return "incomplete", obj{"reason": "max_output_tokens"}, nil
	case "content_filter":
		return "incomplete", obj{"reason": "content_filter"}, nil
	default:
		return "", nil, fmt.Errorf("upstream returned unsupported or missing finish_reason")
	}
}
func usageForResponse(usage obj) any {
	if usage == nil {
		return nil
	}
	in := usage["prompt_tokens"]
	if in == nil {
		in = float64(0)
	}
	out := usage["completion_tokens"]
	if out == nil {
		out = float64(0)
	}
	total := usage["total_tokens"]
	if total == nil {
		a, _ := in.(float64)
		b, _ := out.(float64)
		total = a + b
	}
	cached := object(usage["prompt_tokens_details"])["cached_tokens"]
	if cached == nil {
		cached = 0
	}
	reasoning := object(usage["completion_tokens_details"])["reasoning_tokens"]
	if reasoning == nil {
		reasoning = 0
	}
	return obj{"input_tokens": in, "output_tokens": out, "total_tokens": total, "input_tokens_details": obj{"cached_tokens": cached}, "output_tokens_details": obj{"reasoning_tokens": reasoning}}
}
func responseObject(id, model, status string, created int64, output []any, usage obj, incomplete any) obj {
	return obj{"id": id, "object": "response", "created_at": created, "model": model, "status": status, "output": output, "usage": usageForResponse(usage), "error": nil, "incomplete_details": incomplete, "parallel_tool_calls": true, "store": false}
}
func convertResponse(data []byte, r *convertedRequest) (obj, error) {
	var chat chatResponse
	if err := json.Unmarshal(data, &chat); err != nil {
		return nil, fmt.Errorf("upstream returned invalid JSON")
	}
	if chat.Error != nil {
		return nil, fmt.Errorf("upstream returned an error")
	}
	if len(chat.Choices) != 1 || chat.Choices[0].Index != 0 {
		return nil, fmt.Errorf("upstream must return exactly one choice")
	}
	choice := chat.Choices[0]
	if choice.FinishReason == nil {
		return nil, fmt.Errorf("upstream response has no finish_reason")
	}
	status, incomplete, err := finishStatus(*choice.FinishReason)
	if err != nil {
		return nil, err
	}
	output := []any{}
	m := choice.Message
	if m.ReasoningContent != "" {
		output = append(output, reasoningItem(newID("rs_"), m.ReasoningContent))
	}
	text, err := textFromChat(m.Content)
	if err != nil {
		return nil, err
	}
	if m.Refusal != "" {
		text += m.Refusal
	}
	if m.Content != nil || text != "" {
		output = append(output, messageItem(newID("msg_"), text))
	}
	ids := map[string]bool{}
	// A token-limited partial tool call must not be returned as executable output.
	calls := m.ToolCalls
	if status != "completed" {
		calls = nil
	}
	for _, call := range calls {
		if call.Type != "" && call.Type != "function" {
			return nil, fmt.Errorf("unsupported upstream tool call type")
		}
		if ids[call.ID] {
			return nil, fmt.Errorf("duplicate upstream tool call ID")
		}
		ids[call.ID] = true
		item, err := r.tools.output(call.Function.Name, call.Function.Arguments, call.ID, newID("fc_"))
		if err != nil {
			return nil, err
		}
		output = append(output, item)
	}
	if *choice.FinishReason == "tool_calls" && len(m.ToolCalls) == 0 {
		return nil, fmt.Errorf("upstream tool finish has no calls")
	}
	return responseObject(newID("resp_"), r.model, status, time.Now().Unix(), output, chat.Usage, incomplete), nil
}
