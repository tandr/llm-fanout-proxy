package bridge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type convertedRequest struct {
	chat   obj
	model  string
	stream bool
	tools  *toolSet
}

func convertRequest(r obj, prefix string) (*convertedRequest, error) {
	allowed := strings.Fields("model input instructions tools tool_choice parallel_tool_calls reasoning stream store include metadata client_metadata prompt_cache_key prompt_cache_options stream_options text max_output_tokens temperature top_p service_tier user safety_identifier previous_response_id truncation background access_programs")
	known := map[string]bool{}
	for _, key := range allowed {
		known[key] = true
	}
	for key, value := range r {
		if !known[key] && value != nil {
			return nil, fmt.Errorf("unsupported Responses field %q", key)
		}
	}
	for _, key := range []string{"previous_response_id", "access_programs"} {
		if r[key] != nil {
			return nil, fmt.Errorf("%s is unsupported by the stateless chat adapter", key)
		}
	}
	if v, ok := r["store"]; ok && v != nil && v != false {
		return nil, fmt.Errorf("store:true is unsupported by the stateless chat adapter")
	}
	if v := r["background"]; v != nil && v != false {
		return nil, fmt.Errorf("background mode is unsupported")
	}
	if v := r["truncation"]; v != nil && v != "disabled" {
		return nil, fmt.Errorf("automatic truncation is unsupported")
	}
	model, ok := r["model"].(string)
	if !ok || model == "" {
		return nil, fmt.Errorf("model is required")
	}
	upstreamModel := model
	if prefix != "" {
		p := prefix + "/"
		if !strings.HasPrefix(model, p) {
			return nil, fmt.Errorf("model does not match configured prefix")
		}
		upstreamModel = strings.TrimPrefix(model, p)
		if upstreamModel == "" {
			return nil, fmt.Errorf("upstream model is empty")
		}
	}
	stream := false
	if value, exists := r["stream"]; exists {
		var valid bool
		stream, valid = value.(bool)
		if !valid {
			return nil, fmt.Errorf("stream must be a boolean")
		}
	}
	input, ok := r["input"].([]any)
	if text, isString := r["input"].(string); isString {
		input = []any{obj{"role": "user", "content": text}}
		ok = true
	}
	if !ok {
		return nil, fmt.Errorf("input must be a string or array")
	}
	tools, err := buildTools(r["tools"], input)
	if err != nil {
		return nil, err
	}
	messages, err := convertInput(input, tools)
	if err != nil {
		return nil, err
	}
	if instructions := r["instructions"]; instructions != nil {
		s, ok := instructions.(string)
		if !ok {
			return nil, fmt.Errorf("instructions must be a string")
		}
		if s != "" {
			messages = append([]any{obj{"role": "system", "content": s}}, messages...)
		}
	}
	chat := obj{"model": upstreamModel, "messages": messages, "stream": stream}
	if len(tools.declarations) > 0 {
		chat["tools"] = tools.declarations
	}
	if choice, err := tools.choice(r["tool_choice"]); err != nil {
		return nil, err
	} else if choice != nil {
		chat["tool_choice"] = choice
	}
	for _, key := range []string{"parallel_tool_calls", "temperature", "top_p", "service_tier", "user", "safety_identifier"} {
		if r[key] != nil {
			chat[key] = r[key]
		}
	}
	if max := r["max_output_tokens"]; max != nil {
		n, ok := max.(float64)
		if !ok || n <= 0 || n != float64(int64(n)) {
			return nil, fmt.Errorf("max_output_tokens must be a positive integer")
		}
		chat["max_tokens"] = n
	}
	if raw := r["reasoning"]; raw != nil {
		reasoning := object(raw)
		if reasoning == nil {
			return nil, fmt.Errorf("reasoning must be an object")
		}
		for k, v := range reasoning {
			if k != "effort" && k != "summary" && v != nil {
				return nil, fmt.Errorf("unsupported reasoning field %q", k)
			}
		}
		if effort := reasoning["effort"]; effort != nil {
			s, ok := effort.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("reasoning.effort must be a string")
			}
			chat["reasoning_effort"] = s
		}
	}
	if raw := r["text"]; raw != nil {
		text := object(raw)
		if text == nil {
			return nil, fmt.Errorf("text must be an object")
		}
		for k, v := range text {
			if k != "format" && v != nil {
				return nil, fmt.Errorf("unsupported text field %q", k)
			}
		}
		if rawFormat := text["format"]; rawFormat != nil {
			f := object(rawFormat)
			switch stringValue(f["type"]) {
			case "text":
			case "json_object":
				chat["response_format"] = obj{"type": "json_object"}
			case "json_schema":
				if object(f["schema"]) == nil || stringValue(f["name"]) == "" {
					return nil, fmt.Errorf("JSON schema format requires name and schema")
				}
				schema := obj{"name": f["name"], "schema": f["schema"]}
				for _, k := range []string{"description", "strict"} {
					if f[k] != nil {
						schema[k] = f[k]
					}
				}
				chat["response_format"] = obj{"type": "json_schema", "json_schema": schema}
			default:
				return nil, fmt.Errorf("unsupported text.format")
			}
		}
	}
	if stream {
		chat["stream_options"] = obj{"include_usage": true}
	}
	return &convertedRequest{chat, model, stream, tools}, nil
}

func convertInput(input []any, tools *toolSet) ([]any, error) {
	messages := []any{}
	reasoning := ""
	pendingImages := []any{}
	calls := map[string]bool{}
	answered := map[string]bool{}
	flushImages := func() {
		if len(pendingImages) > 0 {
			messages = append(messages, obj{"role": "user", "content": pendingImages})
			pendingImages = nil
		}
	}
	attachReasoning := func(message obj) {
		if reasoning != "" {
			message["reasoning_content"] = stringValue(message["reasoning_content"]) + reasoning
			reasoning = ""
		}
	}
	for _, raw := range input {
		item := object(raw)
		if item == nil {
			return nil, fmt.Errorf("input items must be objects")
		}
		kind := stringValue(item["type"])
		if kind != "function_call_output" && kind != "custom_tool_call_output" {
			flushImages()
		}
		switch kind {
		case "", "message":
			role := stringValue(item["role"])
			if role != "system" && role != "developer" && role != "user" && role != "assistant" {
				return nil, fmt.Errorf("unsupported message role %q", role)
			}
			content, err := convertContent(item["content"])
			if err != nil {
				return nil, err
			}
			if item["encrypted_content"] != nil && len(content) == 0 {
				return nil, fmt.Errorf("encrypted-only messages cannot be translated")
			}
			message := obj{"role": role, "content": content}
			if role == "assistant" {
				attachReasoning(message)
			}
			messages = append(messages, message)
		case "agent_message":
			content, err := convertContent(item["content"])
			if err != nil {
				return nil, err
			}
			prefix := obj{"type": "text", "text": "Agent message from " + stringValue(item["author"]) + " to " + stringValue(item["recipient"]) + ":"}
			messages = append(messages, obj{"role": "user", "content": append([]any{prefix}, content...)})
		case "reasoning":
			text, err := reasoningText(item)
			if err != nil {
				return nil, err
			}
			reasoning += text
			if len(messages) > 0 && object(messages[len(messages)-1])["role"] == "assistant" {
				attachReasoning(object(messages[len(messages)-1]))
			}
		case "function_call", "custom_tool_call":
			id := stringValue(item["call_id"])
			if id == "" || calls[id] {
				return nil, fmt.Errorf("tool call ID is missing or duplicated")
			}
			calls[id] = true
			toolKind := "function"
			if kind == "custom_tool_call" {
				toolKind = "custom"
			}
			t := tools.byKey[toolKey(stringValue(item["namespace"]), stringValue(item["name"]), toolKind)]
			args := stringValue(item["arguments"])
			if toolKind == "custom" {
				text, ok := item["input"].(string)
				if !ok {
					return nil, fmt.Errorf("custom tool input must be a string")
				}
				b, _ := json.Marshal(obj{"content": text})
				args = string(b)
			} else {
				var parsed obj
				if err := json.Unmarshal([]byte(args), &parsed); err != nil || parsed == nil {
					return nil, fmt.Errorf("function arguments must be a JSON object string")
				}
			}
			call := obj{"id": id, "type": "function", "function": obj{"name": t.wire, "arguments": args}}
			var message obj
			if len(messages) > 0 && object(messages[len(messages)-1])["role"] == "assistant" {
				message = object(messages[len(messages)-1])
			} else {
				message = obj{"role": "assistant", "content": nil}
				messages = append(messages, message)
			}
			attachReasoning(message)
			message["tool_calls"] = append(array(message["tool_calls"]), call)
		case "function_call_output", "custom_tool_call_output":
			id := stringValue(item["call_id"])
			if !calls[id] || answered[id] {
				return nil, fmt.Errorf("tool output references a missing or already answered call")
			}
			answered[id] = true
			content, err := convertContent(item["output"])
			if err != nil {
				return nil, err
			}
			texts := []string{}
			images := []any{}
			for _, part := range content {
				p := object(part)
				if p["type"] == "text" {
					texts = append(texts, stringValue(p["text"]))
				} else {
					images = append(images, p)
				}
			}
			if len(images) > 0 {
				// Chat tool messages cannot carry images; keep the source call explicit in a following user message.
				pendingImages = append(pendingImages, obj{"type": "text", "text": "Images returned by tool call " + id + ":"})
				pendingImages = append(pendingImages, images...)
				texts = append(texts, "[Images follow in a user message, labeled with this tool call ID.]")
			}
			messages = append(messages, obj{"role": "tool", "tool_call_id": id, "content": strings.Join(texts, "\n")})
		case "compaction", "compaction_summary", "context_compaction":
			return nil, fmt.Errorf("opaque compaction cannot be translated to Chat Completions")
		default:
			return nil, fmt.Errorf("unsupported input item type %q", kind)
		}
	}
	flushImages()
	if reasoning != "" {
		if len(messages) > 0 && object(messages[len(messages)-1])["role"] == "assistant" {
			attachReasoning(object(messages[len(messages)-1]))
		} else {
			return nil, fmt.Errorf("reasoning has no associated assistant turn")
		}
	}
	for id := range calls {
		if !answered[id] {
			return nil, fmt.Errorf("tool call %q has no output in input history", id)
		}
	}
	return messages, nil
}
func convertContent(raw any) ([]any, error) {
	if text, ok := raw.(string); ok {
		return []any{obj{"type": "text", "text": text}}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be a string or array")
	}
	result := []any{}
	for _, rawPart := range list {
		p := object(rawPart)
		switch stringValue(p["type"]) {
		case "input_text", "output_text", "text":
			text, ok := p["text"].(string)
			if !ok {
				return nil, fmt.Errorf("text content must be a string")
			}
			result = append(result, obj{"type": "text", "text": text})
		case "input_image", "image_url":
			location := stringValue(p["image_url"])
			detail := stringValue(p["detail"])
			if image := object(p["image_url"]); image != nil {
				location = stringValue(image["url"])
				detail = stringValue(image["detail"])
			}
			u, err := url.Parse(location)
			if err != nil || location == "" || (u.Scheme != "http" && u.Scheme != "https" && !(u.Scheme == "data" && strings.HasPrefix(location, "data:image/"))) {
				return nil, fmt.Errorf("images require an HTTP(S) URL or image data URL")
			}
			image := obj{"url": location}
			if detail != "" {
				if detail != "auto" && detail != "low" && detail != "high" {
					return nil, fmt.Errorf("unsupported image detail %q", detail)
				}
				image["detail"] = detail
			}
			result = append(result, obj{"type": "image_url", "image_url": image})
		default:
			return nil, fmt.Errorf("unsupported content part %q", stringValue(p["type"]))
		}
	}
	return result, nil
}
func reasoningText(item obj) (string, error) {
	for _, key := range []string{"content", "summary"} {
		if item[key] == nil {
			continue
		}
		list, ok := item[key].([]any)
		if !ok {
			return "", fmt.Errorf("reasoning %s must be an array", key)
		}
		parts := []string{}
		for _, raw := range list {
			p := object(raw)
			kind := stringValue(p["type"])
			if kind != "reasoning_text" && kind != "summary_text" {
				return "", fmt.Errorf("unsupported reasoning content %q", kind)
			}
			s, ok := p["text"].(string)
			if !ok {
				return "", fmt.Errorf("reasoning text must be a string")
			}
			parts = append(parts, s)
		}
		text := strings.Join(parts, "\n")
		if text != "" {
			return text, nil
		}
	}
	if stringValue(item["encrypted_content"]) != "" {
		return "", fmt.Errorf("encrypted-only reasoning cannot be translated")
	}
	return "", nil
}
