package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type eventWriter struct {
	w        http.ResponseWriter
	sequence int
}

func (e *eventWriter) send(kind string, data obj) error {
	data["type"] = kind
	data["sequence_number"] = e.sequence
	e.sequence++
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
		return err
	}
	return http.NewResponseController(e.w).Flush()
}

type streamedItem struct {
	kind, id, text string
	index          int
	call           chatCall
}
type streamState struct {
	r               *convertedRequest
	events          *eventWriter
	id              string
	created         int64
	items           []*streamedItem
	text, reasoning *streamedItem
	calls           map[int]*streamedItem
	usage           obj
	finish          string
	bytes           int
}

func (s *streamState) newItem(kind string) *streamedItem {
	prefix := "msg_"
	if kind == "reasoning" {
		prefix = "rs_"
	} else if kind == "tool" {
		prefix = "fc_"
	}
	item := &streamedItem{kind: kind, id: newID(prefix), index: len(s.items)}
	s.items = append(s.items, item)
	return item
}
func (s *streamState) addText(kind, delta string) error {
	if delta == "" {
		return nil
	}
	var item *streamedItem
	if kind == "reasoning" {
		item = s.reasoning
	} else {
		item = s.text
	}
	if item == nil {
		item = s.newItem(kind)
		var initial obj
		if kind == "reasoning" {
			s.reasoning = item
			initial = obj{"id": item.id, "type": "reasoning", "summary": []any{}}
		} else {
			s.text = item
			initial = obj{"id": item.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
		}
		if err := s.events.send("response.output_item.added", obj{"output_index": item.index, "item": initial}); err != nil {
			return err
		}
		if kind == "reasoning" {
			if err := s.events.send("response.reasoning_summary_part.added", obj{"item_id": item.id, "output_index": item.index, "summary_index": 0, "part": obj{"type": "summary_text", "text": ""}}); err != nil {
				return err
			}
		} else {
			if err := s.events.send("response.content_part.added", obj{"item_id": item.id, "output_index": item.index, "content_index": 0, "part": obj{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
				return err
			}
		}
	}
	item.text += delta
	fields := obj{"item_id": item.id, "output_index": item.index, "delta": delta}
	event := "response.output_text.delta"
	if kind == "reasoning" {
		event = "response.reasoning_summary_text.delta"
		fields["summary_index"] = 0
	} else {
		fields["content_index"] = 0
	}
	return s.events.send(event, fields)
}
func (s *streamState) consume(raw string) error {
	s.bytes += len(raw)
	if s.bytes > maxBody {
		return fmt.Errorf("upstream stream exceeds response size limit")
	}
	var chunk chatResponse
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		return fmt.Errorf("upstream returned invalid stream JSON")
	}
	if chunk.Error != nil {
		return fmt.Errorf("upstream returned a stream error")
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 {
		return fmt.Errorf("upstream stream must contain one choice")
	}
	choice := chunk.Choices[0]
	if s.finish != "" {
		return fmt.Errorf("upstream emitted choice data after finish_reason")
	}
	if err := s.addText("reasoning", choice.Delta.ReasoningContent); err != nil {
		return err
	}
	text, err := textFromChat(choice.Delta.Content)
	if err != nil {
		return err
	}
	text += choice.Delta.Refusal
	if err = s.addText("message", text); err != nil {
		return err
	}
	for _, fragment := range choice.Delta.ToolCalls {
		if fragment.Index == nil || *fragment.Index < 0 {
			return fmt.Errorf("upstream tool delta has no valid index")
		}
		item := s.calls[*fragment.Index]
		if item == nil {
			item = &streamedItem{kind: "tool", id: newID("fc_")}
			s.calls[*fragment.Index] = item
		}
		if fragment.Type != "" && fragment.Type != "function" {
			return fmt.Errorf("unsupported upstream tool type")
		}
		if fragment.ID != "" {
			if item.call.ID != "" && item.call.ID != fragment.ID {
				return fmt.Errorf("upstream changed a tool call ID")
			}
			item.call.ID = fragment.ID
		}
		item.call.Function.Name += fragment.Function.Name
		item.call.Function.Arguments += fragment.Function.Arguments
	}
	if choice.FinishReason != nil {
		s.finish = *choice.FinishReason
	}
	return nil
}
func (s *streamState) complete() error {
	status, incomplete, err := finishStatus(s.finish)
	if err != nil {
		return err
	}
	if s.finish == "tool_calls" && len(s.calls) == 0 {
		return fmt.Errorf("upstream tool finish has no calls")
	}
	// Incomplete tool arguments must never become executable output items.
	if status == "completed" {
		indexes := make([]int, 0, len(s.calls))
		for index := range s.calls {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			item := s.calls[index]
			item.index = len(s.items)
			s.items = append(s.items, item)
		}
	}
	output := []any{}
	ids := map[string]bool{}
	// Validate every item before emitting final items or declaring completion.
	for _, item := range s.items {
		switch item.kind {
		case "message":
			output = append(output, messageItem(item.id, item.text))
		case "reasoning":
			output = append(output, reasoningItem(item.id, item.text))
		case "tool":
			if ids[item.call.ID] {
				return fmt.Errorf("duplicate upstream tool call ID")
			}
			ids[item.call.ID] = true
			result, err := s.r.tools.output(item.call.Function.Name, item.call.Function.Arguments, item.call.ID, item.id)
			if err != nil {
				return err
			}
			output = append(output, result)
		}
	}
	for i, item := range s.items {
		result := object(output[i])
		fields := obj{"item_id": item.id, "output_index": i}
		switch item.kind {
		case "message":
			fields["content_index"] = 0
			fields["text"] = item.text
			if err := s.events.send("response.output_text.done", fields); err != nil {
				return err
			}
			if err := s.events.send("response.content_part.done", obj{"item_id": item.id, "output_index": i, "content_index": 0, "part": array(result["content"])[0]}); err != nil {
				return err
			}
		case "reasoning":
			fields["summary_index"] = 0
			fields["text"] = item.text
			if err := s.events.send("response.reasoning_summary_text.done", fields); err != nil {
				return err
			}
			if err := s.events.send("response.reasoning_summary_part.done", obj{"item_id": item.id, "output_index": i, "summary_index": 0, "part": array(result["summary"])[0]}); err != nil {
				return err
			}
		case "tool":
			initial := obj{}
			for key, value := range result {
				initial[key] = value
			}
			initial["status"] = "in_progress"
			custom := result["type"] == "custom_tool_call"
			field, event := "arguments", "response.function_call_arguments"
			if custom {
				field, event = "input", "response.custom_tool_call_input"
			}
			initial[field] = ""
			if err := s.events.send("response.output_item.added", obj{"output_index": i, "item": initial}); err != nil {
				return err
			}
			fields["call_id"] = item.call.ID
			fields["delta"] = result[field]
			if err := s.events.send(event+".delta", fields); err != nil {
				return err
			}
			done := obj{"item_id": item.id, "call_id": item.call.ID, "output_index": i, field: result[field]}
			if err := s.events.send(event+".done", done); err != nil {
				return err
			}
		}
		if err := s.events.send("response.output_item.done", obj{"output_index": i, "item": result}); err != nil {
			return err
		}
	}
	return s.events.send("response."+status, obj{"response": responseObject(s.id, s.r.model, status, s.created, output, s.usage, incomplete)})
}
func serveStream(w http.ResponseWriter, body io.Reader, r *convertedRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	s := &streamState{r: r, events: &eventWriter{w: w}, id: newID("resp_"), created: time.Now().Unix(), calls: map[int]*streamedItem{}}
	for _, kind := range []string{"response.created", "response.in_progress"} {
		if err := s.events.send(kind, obj{"response": responseObject(s.id, r.model, "in_progress", s.created, []any{}, nil, nil)}); err != nil {
			return
		}
	}
	fail := func(err error) {
		_ = s.events.send("response.failed", obj{"response": obj{"id": s.id, "object": "response", "model": r.model, "status": "failed", "error": obj{"code": "upstream_error", "message": err.Error()}}})
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32<<10), maxRecord)
	data := ""
	done := false
	process := func() error {
		if data == "" {
			return nil
		}
		record := data
		data = ""
		if record == "[DONE]" {
			done = true
			return nil
		}
		return s.consume(record)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := process(); err != nil {
				fail(err)
				return
			}
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if len(data)+len(value)+1 > maxRecord {
				fail(fmt.Errorf("upstream SSE record exceeds size limit"))
				return
			}
			if data != "" {
				data += "\n"
			}
			data += value
		}
	}
	if err := scanner.Err(); err != nil {
		fail(fmt.Errorf("upstream stream read failed"))
		return
	}
	if !done && data != "" {
		if err := process(); err != nil {
			fail(err)
			return
		}
	}
	if !done {
		fail(fmt.Errorf("upstream stream ended before [DONE]"))
		return
	}
	if err := s.complete(); err != nil {
		fail(err)
	}
}
