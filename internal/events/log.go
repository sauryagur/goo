// Package events is the durable, ordered mutation log at the heart of GOO.
//
// GOO's defining idea is that object mutations are first-class durable events,
// and the object namespace is a materialized view of this log. This package
// wraps a mature write-ahead log (github.com/tidwall/wal) with a small,
// domain-aware API:
//
//   - sequenced, monotonic appends (the WAL index IS the event sequence)
//   - replay from any sequence
//   - live tailing via subscriptions
//   - bucket-filtered scanning
//   - restart recovery (the WAL is reopened and First/Last are re-derived)
//
// Durability: writes are fsynced by the WAL (NoSync is left false), so an
// event returned by Append has been persisted to stable storage before the
// caller is told about it. The only thing the caller must never do is report a
// mutation as committed before its event has been appended here.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gur/goo/internal/goo"
	"github.com/tidwall/wal"
)

// Log is the durable event log. It is safe for concurrent use.
//
// The sequence number of every event is its physical position in the
// underlying WAL, which is what makes replay, resume, and SSE event ids all
// line up without an extra index.
type Log struct {
	path string
	wal  *wal.Log

	mu     sync.Mutex // guards subs + the live-tail broadcast + closed flag
	subs   map[*subscription]struct{}
	closed bool
}

// subscription is one live tailer of the log.
type subscription struct {
	ch chan goo.Event
}

// ErrNotFound is returned by Read when a sequence is outside the log.
var ErrNotFound = errors.New("events: sequence not found")

// Open opens (or creates) the durable log stored under dir.
func Open(dir string) (*Log, error) {
	// AllowEmpty keeps the index math simple: an empty/new log reports
	// FirstIndex()==1 and LastIndex()==0, so the next sequence is always
	// LastIndex()+1. We never truncate in v1, so a real log always has at
	// least one entry with First>0.
	w, err := wal.Open(dir, &wal.Options{
		AllowEmpty:  true,
		NoSync:      false, // fsync every write: durability over throughput
		SegmentSize: 16 * 1024 * 1024,
		DirPerms:    0o700,
		FilePerms:   0o600,
	})
	if err != nil {
		return nil, fmt.Errorf("open wal: %w", err)
	}
	return &Log{
		path: dir,
		wal:  w,
		subs: make(map[*subscription]struct{}),
	}, nil
}

// Close closes the log and stops all subscriptions. It is safe to call more
// than once; subsequent calls are no-ops.
func (l *Log) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	for s := range l.subs {
		close(s.ch)
		delete(l.subs, s)
	}
	l.mu.Unlock()
	return l.wal.Close()
}

// Dir returns the on-disk directory backing this log.
func (l *Log) Dir() string { return l.path }

// FirstSequence returns the lowest sequence in the log (1 if empty).
func (l *Log) FirstSequence() (uint64, error) {
	return l.wal.FirstIndex()
}

// LastSequence returns the highest sequence in the log (0 if empty).
func (l *Log) LastSequence() (uint64, error) {
	return l.wal.LastIndex()
}

// IsEmpty reports whether the log has no entries.
func (l *Log) IsEmpty() (bool, error) {
	return l.wal.IsEmpty()
}

// Append records a mutation durably and returns the committed event.
//
// Sequencing, the per-mutation unique ID, and the timestamp are assigned here.
// The write is fsynced before Append returns, so by the time the caller sees
// the Event it is already on stable storage. The event is then broadcast to
// live subscribers.
//
// Because sequences are assigned under a lock and the WAL is strictly ordered,
// concurrent callers can never share or skip a sequence.
func (l *Log) Append(action, bucket, key string, size int64, hash string, version uint64) (goo.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	last, err := l.wal.LastIndex()
	if err != nil {
		return goo.Event{}, fmt.Errorf("read last index: %w", err)
	}
	seq := last + 1
	ev := goo.Event{
		Sequence:  seq,
		ID:        fmt.Sprintf("%d-%s-%s-%d", seq, bucket, key, version),
		Action:    action,
		Bucket:    bucket,
		Key:       key,
		Size:      size,
		Hash:      hash,
		Version:   version,
		Timestamp: time.Now().UTC(),
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return goo.Event{}, fmt.Errorf("marshal event: %w", err)
	}
	if err := l.wal.Write(seq, data); err != nil {
		return goo.Event{}, fmt.Errorf("write event seq %d: %w", seq, err)
	}

	l.broadcast(ev)
	return ev, nil
}

// Read returns the event stored at seq.
func (l *Log) Read(seq uint64) (goo.Event, error) {
	data, err := l.wal.Read(seq)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, wal.ErrNotFound) {
			return goo.Event{}, ErrNotFound
		}
		return goo.Event{}, fmt.Errorf("read seq %d: %w", seq, err)
	}
	var ev goo.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return goo.Event{}, fmt.Errorf("unmarshal seq %d: %w", seq, err)
	}
	return ev, nil
}

// Replay returns every event with sequence >= from and <= last, in order.
// If from is past the end of the log, an empty (non-error) slice is returned;
// the caller should then subscribe for live events.
func (l *Log) Replay(from uint64) ([]goo.Event, error) {
	last, err := l.wal.LastIndex()
	if err != nil {
		return nil, fmt.Errorf("last index: %w", err)
	}
	if from == 0 {
		// 0 means "from the beginning"; FirstIndex is 1 for our empty policy.
		from = 1
	}
	if from > last {
		return nil, nil
	}
	out := make([]goo.Event, 0, last-from+1)
	for seq := from; seq <= last; seq++ {
		ev, err := l.Read(seq)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// Scan walks events from `from` (inclusive) to the end, calling fn for each.
// If bucket is non-empty, results are limited to that bucket. Returning false
// from fn stops the scan early. This is the streaming path used by SSE and the
// CLI so we never materialize the whole log in memory.
func (l *Log) Scan(from uint64, bucket string, fn func(goo.Event) error) error {
	last, err := l.wal.LastIndex()
	if err != nil {
		return fmt.Errorf("last index: %w", err)
	}
	if from == 0 {
		from = 1
	}
	if from > last {
		return nil
	}
	for seq := from; seq <= last; seq++ {
		ev, err := l.Read(seq)
		if err != nil {
			return err
		}
		if bucket != "" && ev.Bucket != bucket {
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe begins a live tail. The returned channel delivers every event
// appended *after* Subscribe returns. Replay historical events with Replay
// first if you need the full history.
//
// The channel is buffered; if a consumer is too slow to keep up, the broadcast
// drops the live delivery for that subscriber but the event remains durable in
// the log and can be recovered by reconnecting and replaying from the last
// seen sequence. This keeps a stuck consumer from stalling the whole log.
func (l *Log) Subscribe() <-chan goo.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &subscription{
		ch: make(chan goo.Event, 256),
	}
	l.subs[s] = struct{}{}
	return s.ch
}

// Unsubscribe removes a subscription and stops its channel.
func (l *Log) Unsubscribe(ch <-chan goo.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for s := range l.subs {
		if (<-chan goo.Event)(s.ch) == ch {
			close(s.ch)
			delete(l.subs, s)
			return
		}
	}
}

// broadcast delivers ev to every live subscriber without blocking the appender.
func (l *Log) broadcast(ev goo.Event) {
	for s := range l.subs {
		select {
		case s.ch <- ev:
		default:
			// slow consumer: drop live delivery, event stays durable in log.
		}
	}
}
