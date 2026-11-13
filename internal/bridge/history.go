package bridge

import (
	"encoding/json"
	"sync"
	"time"
)

const historyTTL = time.Hour
const historyBytes = 1 << 30
const snapshotBytes = 16 << 20

// conversation is an append-only run of comma-separated JSON items; every snapshot
// is a prefix of it, so a linear conversation stores each item once.
type conversation struct {
	model string
	data  []byte
	refs  int
}

type historyEntry struct {
	conv  *conversation
	end   int // snapshot is conv.data[:end]
	count int // items in the snapshot
	used  time.Time
}

// History retains bounded, immutable conversation snapshots; it never writes to disk.
type History struct {
	mu       sync.Mutex
	entries  map[string]historyEntry
	bytes    int
	timer    *time.Timer
	closed   bool
	now      func() time.Time
	ttl      time.Duration
	maxBytes int
	hits     int
	misses   int
	evicted  int
	oversize int
}

func NewHistory() *History {
	return &History{entries: make(map[string]historyEntry), now: time.Now, ttl: historyTTL, maxBytes: historyBytes}
}

// Stats reports aggregate cache counters; it never includes identifiers or content.
func (h *History) Stats() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]int{"entries": len(h.entries), "bytes": h.bytes, "hits": h.hits, "misses": h.misses, "evictions": h.evicted, "oversized": h.oversize}
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

// drop removes one snapshot; the shared buffer is released with its last reference.
func (h *History) drop(id string) {
	entry, ok := h.entries[id]
	if !ok {
		return
	}
	delete(h.entries, id)
	entry.conv.refs--
	if entry.conv.refs == 0 {
		h.bytes -= len(entry.conv.data)
		entry.conv.data = nil
	}
}

func (h *History) prune(now time.Time) {
	for id, entry := range h.entries {
		if !now.Before(entry.used.Add(h.ttl)) {
			h.drop(id)
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

func (h *History) get(id, model string) (input []any, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer func() {
		if ok {
			h.hits++
		} else {
			h.misses++
		}
	}()
	now := h.now()
	h.prune(now)
	entry, found := h.entries[id]
	if h.closed || !found || entry.conv.model != model {
		return nil, false
	}
	entry.used = now
	h.entries[id] = entry
	h.schedule()
	data := make([]byte, 0, entry.end+2)
	data = append(append(append(data, '['), entry.conv.data[:entry.end]...), ']')
	if json.Unmarshal(data, &input) != nil {
		return nil, false
	}
	return input, true
}

func appendItems(dst []byte, items []any) ([]byte, bool) {
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			return dst, false
		}
		if len(dst) > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, data...)
	}
	return dst, true
}

// put stores a snapshot; input is the merged conversation, so only the items past
// the parent are appended to a shared buffer.
func (h *History) put(id, model, parent string, input, output []any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	conv := &conversation{model: model}
	delta := input
	if parent != "" {
		if entry, ok := h.entries[parent]; ok && entry.conv.model == model && entry.count <= len(input) {
			conv = entry.conv
			delta = input[entry.count:]
		}
	}
	count := len(input) + len(output)
	data, ok := appendItems(nil, delta)
	if ok {
		data, ok = appendItems(data, output)
	}
	if !ok {
		return
	}
	prev := len(conv.data)
	added := len(data)
	if prev > 0 && added > 0 {
		added++
	}
	if prev+added > snapshotBytes || prev+added > h.maxBytes {
		h.oversize++
		return
	}
	now := h.now()
	h.prune(now)
	h.drop(id)
	if prev > 0 && len(data) > 0 {
		conv.data = append(conv.data, ',')
	}
	conv.data = append(conv.data, data...)
	conv.refs++
	h.bytes += added
	h.entries[id] = historyEntry{conv: conv, end: len(conv.data), count: count, used: now}
	for h.bytes > h.maxBytes {
		var oldest string
		var used time.Time
		for candidate, entry := range h.entries {
			if oldest == "" || entry.used.Before(used) {
				oldest, used = candidate, entry.used
			}
		}
		if oldest == "" {
			break
		}
		h.drop(oldest)
		h.evicted++
	}
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
	var parentID string
	if raw := request["previous_response_id"]; raw != nil {
		id, ok := raw.(string)
		if !ok || id == "" {
			return nil, invalid("previous_response_id must be a nonempty string or null")
		}
		parent, found := h.config.History.get(id, model)
		if !found {
			return nil, missingPrevious()
		}
		parentID = id
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
		h.config.History.put(stringValue(response["id"]), model, parentID, input, array(response["output"]))
	}
	return converted, nil
}

func (h *handler) Close() { h.config.History.Close() }

func upstreamError(message string) error { return &requestError{status: 502, message: message} }
