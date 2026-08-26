package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gur/goo/internal/events"
	"github.com/gur/goo/internal/goo"
)

// SSEHub streams the durable event log to HTTP clients over Server-Sent
// Events. It is the "live consumer" spoke of GOO's central diagram: a client
// receives the requested history (replay) and then a seamless transition into
// live tailing of new mutations.
//
// Reconnect/resume is first-class: the client either passes ?from=N or sends
// Last-Event-ID: N, and the hub resumes from the next available event. Because
// the event sequence is the SSE event id, a client that tracks the last id it
// saw can always recover missing events after a disconnect by reconnecting
// with that id.
type SSEHub struct {
	log *events.Log
}

// NewSSEHub builds the SSE hub around the engine's event log.
func NewSSEHub(log *events.Log) *SSEHub {
	return &SSEHub{log: log}
}

// Handle serves one SSE connection for a bucket's event stream.
//
// Query/header semantics:
//   - ?from=N        replay from sequence N (inclusive), then live
//   - Last-Event-ID  same as from=, used by browsers on auto-reconnect
//   - bucket         path param; when set, only that bucket's events are sent
//
// The handler returns only after the client disconnects or the server shuts
// down; it runs in its own goroutine per connection (multiple consumers are
// fully supported).
func (h *SSEHub) Handle(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if bucket != "" && !goo.ValidBucket(bucket) {
		writeError(w, http.StatusBadRequest, "invalid bucket name")
		return
	}

	from := parseFrom(r)
	// Last-Event-ID (sent by EventSource on reconnect) means "the last
	// sequence the client already has"; resume from the next one so a
	// reconnect never replays a duplicate or misses an event.
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if n, err := strconv.ParseUint(id, 10, 64); err == nil {
			from = n + 1
		}
	}
	// ?from=N is explicit and inclusive: replay from sequence N onward.

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	flusher.Flush()

	// Subscribe BEFORE replaying so we don't miss an event appended in the
	// gap between reading "last" and starting the live loop. We remember the
	// cutoff and only forward live events strictly after it.
	sub := h.log.Subscribe()
	defer h.log.Unsubscribe(sub)

	// 1) replay the requested history first.
	last := h.replay(w, flusher, from, bucket, r)
	if r.Context().Err() != nil {
		return // client gone during replay
	}

	// 2) then transition to live tailing, skipping anything <= last.
	for {
		select {
		case <-r.Context().Done():
			return // client disconnected
		case ev := <-sub:
			if bucket != "" && ev.Bucket != bucket {
				continue
			}
			if ev.Sequence <= last {
				continue // already covered by replay
			}
			if sendEvent(w, flusher, ev) {
				return
			}
		}
	}
}

// replay sends all events with sequence >= from up to the current end, and
// returns the highest sequence it sent (0 if nothing was sent and the caller
// should treat the log as empty). It returns early (last=0) if the client
// disconnected mid-replay.
func (h *SSEHub) replay(w http.ResponseWriter, f http.Flusher, from uint64, bucket string, r *http.Request) uint64 {
	last, err := h.log.LastSequence()
	if err != nil {
		return 0
	}
	if from == 0 {
		from = 1
	}
	var sent uint64
	for seq := from; seq <= last; seq++ {
		select {
		case <-r.Context().Done():
			return 0
		default:
		}
		evs, err := h.log.Replay(seq)
		if err != nil || len(evs) == 0 {
			break
		}
		ev := evs[0]
		if bucket != "" && ev.Bucket != bucket {
			continue
		}
		if sendEvent(w, f, ev) {
			return 0
		}
		sent = ev.Sequence
	}
	// if we sent nothing (e.g. empty log or all filtered), the live loop can
	// start from 0 safely.
	if sent == 0 {
		return 0
	}
	return sent
}

// sendEvent writes one SSE frame. Returns true if the client disconnected.
func sendEvent(w http.ResponseWriter, f http.Flusher, ev goo.Event) bool {
	name := goo.EventNamePut
	if ev.Action == goo.ActionDelete {
		name = goo.EventNameDelete
	}
	data, _ := json.Marshal(ev)
	// id + event + data, separated by blank line. The id is the sequence so
	// clients can resume from the last one they received.
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Sequence, name, data)
	f.Flush()
	// a zero-byte write after flush surfaces a broken client connection.
	if _, err := w.Write([]byte("")); err != nil {
		return true
	}
	return false
}

func parseFrom(r *http.Request) uint64 {
	if v := r.URL.Query().Get("from"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}
