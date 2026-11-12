package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type websocketRequest struct {
	r                     *convertedRequest
	cancel                context.CancelFunc
	interrupted, terminal bool
	sequence              int
	completed             []any
}

type websocketSession struct {
	mu     sync.Mutex
	conn   *websocket.Conn
	ctx    context.Context
	active *websocketRequest
}

func (s *websocketSession) send(kind string, data obj) error {
	data["type"] = kind
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, b)
}

func (s *websocketSession) fail(err error) {
	status, detail := errorObject(err)
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.send("error", obj{"status": status, "error": detail})
}

func (s *websocketSession) interrupt(message obj) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.active
	id := stringValue(message["response_id"])
	if a == nil || id == "" || a.r.id != id || a.terminal || message["mode"] != "discard_partial_items" {
		_ = s.send("response.interrupt.failed", obj{"response_id": id, "error": obj{"code": "response_not_active", "message": "No matching active response to interrupt."}})
		return
	}
	a.interrupted, a.terminal = true, true
	a.cancel()
	_ = s.send("response.interrupt.accepted", obj{"response_id": id, "sequence_number": a.sequence})
	a.sequence++
	response := responseObject(id, a.r.model, "incomplete", a.r.created, a.completed, nil, obj{"reason": "interrupted"})
	a.r.save(response)
	_ = s.send("response.incomplete", obj{"response": response, "sequence_number": a.sequence})
	a.sequence++
}

func (s *websocketSession) run(h *handler, request obj) {
	warmup := false
	if generate, exists := request["generate"]; exists {
		var ok bool
		warmup, ok = generate.(bool)
		if !ok {
			s.fail(invalid("generate must be a boolean"))
			return
		}
		warmup = !warmup
	}
	delete(request, "generate")
	delete(request, "type")
	request["stream"] = true
	r, err := h.prepare(request)
	if err != nil {
		s.fail(err)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	a := &websocketRequest{r: r, cancel: cancel, completed: []any{}}
	s.mu.Lock()
	s.active = a
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.active == a {
			s.active = nil
		}
		s.mu.Unlock()
	}()
	events := &eventWriter{started: true}
	events.emit = func(kind string, data obj) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if a.interrupted || ctx.Err() != nil {
			return context.Canceled
		}
		if kind == "response.completed" || kind == "response.incomplete" {
			a.terminal = true
			r.save(object(data["response"]))
		} else if kind == "response.failed" {
			a.terminal = true
		}
		data["sequence_number"] = a.sequence
		a.sequence++
		err := s.send(kind, data)
		if err == nil && kind == "response.output_item.done" {
			a.completed = append(a.completed, data["item"])
		}
		return err
	}
	for _, kind := range []string{"response.created", "response.in_progress"} {
		if events.send(kind, obj{"response": responseObject(r.id, r.model, "in_progress", r.created, []any{}, nil, nil)}) != nil {
			return
		}
	}
	if warmup {
		_ = events.send("response.completed", obj{"response": responseObject(r.id, r.model, "completed", r.created, []any{}, nil, nil)})
		return
	}
	if _, err := h.execute(ctx, r, events); err != nil && ctx.Err() == nil {
		s.fail(err)
	}
}

// ServeWebSocket translates one connection using the same history and conversion as HTTP.
func (h *handler) ServeWebSocket(ctx context.Context, conn *websocket.Conn, first []byte) {
	ctx, cancel := context.WithCancel(ctx)
	s := &websocketSession{conn: conn, ctx: ctx}
	requests := make(chan obj, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-requests:
				if ctx.Err() != nil {
					return
				}
				s.run(h, request)
			}
		}
	}()
	defer func() { cancel(); _ = conn.CloseNow(); <-done }()
	for data := first; ; {
		var message obj
		if json.Unmarshal(data, &message) != nil || message == nil {
			s.fail(invalid("WebSocket event must be a JSON object"))
		} else if message["stream_id"] != nil {
			s.fail(invalid("named WebSocket streams are unsupported"))
		} else {
			switch message["type"] {
			case "response.create":
				select {
				case requests <- message:
				default:
					s.fail(&requestError{status: 429, message: "Only one pending response.create is allowed."})
				}
			case "response.interrupt":
				s.interrupt(message)
			default:
				s.fail(invalid("unsupported WebSocket event type"))
			}
		}
		readCtx, stop := context.WithTimeout(ctx, time.Hour)
		kind, next, err := conn.Read(readCtx)
		stop()
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			s.fail(invalid("WebSocket events must be text"))
			return
		}
		data = next
	}
}
