package bridge

import (
	"encoding/json"
	"sync"
	"time"
)

const historyTTL = time.Hour
const historyEntries = 256
const historyBytes = 64 << 20
const snapshotBytes = 16 << 20

type historyEntry struct {
	model string
	input []byte
	used  time.Time
}

// History retains bounded, immutable conversation snapshots; it never writes to disk.
type History struct {
	mu                   sync.Mutex
	entries              map[string]historyEntry
	bytes                int
	timer                *time.Timer
	closed               bool
	now                  func() time.Time
	ttl                  time.Duration
	maxEntries, maxBytes int
}

func NewHistory() *History {
	return &History{entries: make(map[string]historyEntry), now: time.Now, ttl: historyTTL, maxEntries: historyEntries, maxBytes: historyBytes}
}

func (h *History) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
	}
	clear(h.entries)
	h.bytes = 0
}

func (h *History) prune(now time.Time) {
	for id, entry := range h.entries {
		if !now.Before(entry.used.Add(h.ttl)) {
			h.bytes -= len(entry.input)
			delete(h.entries, id)
		}
	}
}

func (h *History) schedule() {
	if h.timer != nil {
		h.timer.Stop()
	}
	if h.closed || len(h.entries) == 0 {
		return
	}
	var earliest time.Time
	for _, entry := range h.entries {
		expires := entry.used.Add(h.ttl)
		if earliest.IsZero() || expires.Before(earliest) {
			earliest = expires
		}
	}
	delay := earliest.Sub(h.now())
	if delay < 0 {
		delay = 0
	}
	h.timer = time.AfterFunc(delay, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.prune(h.now())
		h.schedule()
	})
}

func (h *History) get(id, model string) ([]any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.prune(now)
	entry, ok := h.entries[id]
	if h.closed || !ok || entry.model != model {
		return nil, false
	}
	entry.used = now
	h.entries[id] = entry
	h.schedule()
	var input []any
	if json.Unmarshal(entry.input, &input) != nil {
		return nil, false
	}
	return input, true
}

func (h *History) put(id, model string, input, output []any) {
	items := make([]any, 0, len(input)+len(output))
	items = append(items, input...)
	items = append(items, output...)
	data, err := json.Marshal(items)
	if err != nil || len(data) > snapshotBytes || len(data) > h.maxBytes {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	now := h.now()
	h.prune(now)
	if old, ok := h.entries[id]; ok {
		h.bytes -= len(old.input)
		delete(h.entries, id)
	}
	for len(h.entries) >= h.maxEntries || h.bytes+len(data) > h.maxBytes {
		var oldest string
		var used time.Time
		for candidate, entry := range h.entries {
			if oldest == "" || entry.used.Before(used) {
				oldest, used = candidate, entry.used
			}
		}
		if oldest == "" {
			return
		}
		h.bytes -= len(h.entries[oldest].input)
		delete(h.entries, oldest)
	}
	h.entries[id] = historyEntry{model, data, now}
	h.bytes += len(data)
	h.schedule()
}

type requestError struct {
	status               int
	code, param, message string
}

func (e *requestError) Error() string      { return e.message }
func invalid(message string) *requestError { return &requestError{status: 400, message: message} }
func missingPrevious() *requestError {
	return &requestError{400, "previous_response_not_found", "previous_response_id", "Previous response was not found. Retry with the full input."}
}
func errorObject(err error) (int, obj) {
	e, ok := err.(*requestError)
	if !ok {
		e = &requestError{status: 502, message: "upstream request failed"}
	}
	return e.status, obj{"type": "invalid_request_error", "code": e.code, "param": e.param, "message": e.message}
}

func (h *handler) prepare(request obj) (*convertedRequest, error) {
	model := stringValue(request["model"])
	input, ok := request["input"].([]any)
	if text, isString := request["input"].(string); isString {
		input, ok = []any{obj{"role": "user", "content": text}}, true
	}
	if request["input"] == nil && request["previous_response_id"] != nil {
		input, ok = []any{}, true
	}
	if !ok {
		return nil, invalid("input must be a string or array")
	}
	if raw := request["previous_response_id"]; raw != nil {
		id, ok := raw.(string)
		if !ok || id == "" {
			return nil, invalid("previous_response_id must be a nonempty string or null")
		}
		parent, found := h.config.History.get(id, model)
		if !found {
			return nil, missingPrevious()
		}
		input = append(parent, input...)
	}
	resolved := make(obj, len(request))
	for key, value := range request {
		resolved[key] = value
	}
	delete(resolved, "previous_response_id")
	resolved["input"] = input
	converted, err := convertRequest(resolved, h.config.ModelPrefix)
	if err != nil {
		return nil, invalid(err.Error())
	}
	converted.id = newID("resp_")
	converted.created = time.Now().Unix()
	converted.save = func(response obj) {
		h.config.History.put(stringValue(response["id"]), model, input, array(response["output"]))
	}
	return converted, nil
}

func (h *handler) Close() { h.config.History.Close() }

func upstreamError(message string) error { return &requestError{status: 502, message: message} }
