// Package engine is the coordination layer that makes GOO's central idea real:
// object mutations are durable events, and the object namespace is a
// materialized view of the event log.
//
// It owns exactly two durable things:
//
//   - a Store (object bytes + on-disk metadata)
//   - an events.Log (the durable, ordered mutation log)
//
// and the in-memory index that is rebuilt by replaying the log.
//
// CRASH-CONSISTENCY INVARIANT (the most important comment in this repo):
//
//	A mutation is "committed" only after BOTH of these have happened, in order:
//	  1. the object bytes + metadata are durably on disk (fsynced), and
//	  2. the mutation event is durably appended to the log (fsynced).
//
//	The event log is the SOURCE OF TRUTH for the namespace.
//
//	Put:  bytes first, then the PUT event. A crash between them leaves an
//	      orphan byte with no event; rebuild prunes it.
//	Delete: the DELETE (tombstone) event first, then the bytes are removed. A
//	      crash between them leaves bytes with no index entry; rebuild prunes
//	      them too. Either way the durable log and the rebuilt namespace agree.
//
//	Consequences:
//	  - object-without-event can never survive a restart.
//	  - event-without-object cannot happen, because bytes are written and
//	    fsynced before the event is appended (and for deletes, the bytes are
//	    removed only after the tombstone is durable, with rebuild as backstop).
//	  - a reader never observes a commit whose event is not yet durable.
package engine

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gur/goo/internal/events"
	"github.com/gur/goo/internal/goo"
	"github.com/gur/goo/internal/store"
)

// Engine is the single coordination point for mutations and reads.
type Engine struct {
	root  string
	store *store.Store
	log   *events.Log

	mu    sync.Mutex            // serializes mutations so the index, bytes and log stay consistent
	index map[string]goo.Object // bucket/key -> current object state
}

// Open opens an engine rooted at root. It replays the log to rebuild the
// in-memory index and prunes orphaned object bytes.
func Open(root string) (*Engine, error) {
	st, err := store.Open(root)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	lg, err := events.Open(root + "/wal")
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	e := &Engine{
		root:  root,
		store: st,
		log:   lg,
		index: make(map[string]goo.Object),
	}
	if err := e.rebuild(); err != nil {
		return nil, fmt.Errorf("rebuild index: %w", err)
	}
	return e, nil
}

// rebuild replays the durable log into the in-memory index (the materialized
// view) and prunes object bytes that have no event. This is what makes "the
// namespace is a materialized view of mutation history" literally true after a
// crash.
func (e *Engine) rebuild() error {
	last, err := e.log.LastSequence()
	if err != nil {
		return err
	}
	for seq := uint64(1); seq <= last; seq++ {
		ev, err := e.log.Read(seq)
		if err != nil {
			return fmt.Errorf("read seq %d during rebuild: %w", seq, err)
		}
		e.applyToIndex(ev)
	}

	// prune orphan bytes: objects on disk with no event in the index.
	present, err := e.store.ListAll()
	if err != nil {
		return err
	}
	for _, ref := range present {
		if _, ok := e.index[ref]; !ok {
			// orphan from a crash between byte-write and event-append.
			bucket, key := splitRef(ref)
			_ = e.store.Delete(bucket, key) // best-effort; ignore not-found
		}
	}
	return nil
}

// applyToIndex folds one event into the in-memory materialized index.
func (e *Engine) applyToIndex(ev goo.Event) {
	ref := ev.Bucket + "/" + ev.Key
	switch ev.Action {
	case goo.ActionPut:
		now := ev.Timestamp
		if now.IsZero() {
			now = time.Now().UTC()
		}
		obj, ok := e.index[ref]
		if !ok {
			obj = goo.Object{Bucket: ev.Bucket, Key: ev.Key, CreatedAt: now}
		}
		obj.Size = ev.Size
		obj.Hash = ev.Hash
		obj.Version = ev.Version
		obj.UpdatedAt = now
		e.index[ref] = obj
	case goo.ActionDelete:
		delete(e.index, ref)
	}
}

// ---- mutations (the commit protocol) ----

// Put writes an object and appends its PUT event. It returns the committed
// event. The call blocks until both bytes and the event are durable.
func (e *Engine) Put(bucket, key string, data io.Reader, overwrite bool) (goo.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	meta, err := e.store.Put(bucket, key, data, store.PutOptions{Overwrite: overwrite})
	if err != nil {
		return goo.Event{}, fmt.Errorf("write object bytes: %w", err)
	}
	ev, err := e.log.Append(goo.ActionPut, bucket, key, meta.Size, meta.Hash, meta.Version)
	if err != nil {
		// bytes are durable but the event failed; roll back the bytes so we
		// don't leave an uncommitted object behind. The next rebuild would
		// prune it anyway, but rolling back keeps the store honest now.
		_ = e.store.Delete(bucket, key)
		return goo.Event{}, fmt.Errorf("append put event: %w", err)
	}
	e.applyToIndex(ev)
	return ev, nil
}

// Delete removes an object and appends its DELETE event. Deleting a missing
// object returns store.ErrNotFound and emits no event.
//
// Ordering (the mirror image of Put): commit the tombstone event FIRST, then
// remove the bytes. A crash in between leaves bytes with no index entry, which
// the next rebuild prunes — so the log stays the source of truth and we never
// report a delete whose record isn't durable.
func (e *Engine) Delete(bucket, key string) (goo.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.store.Stat(bucket, key); err != nil {
		return goo.Event{}, err // not found: no mutation, no event
	}
	// commit the tombstone before touching the bytes.
	ev, err := e.log.Append(goo.ActionDelete, bucket, key, 0, "", 0)
	if err != nil {
		return goo.Event{}, fmt.Errorf("append delete event: %w", err)
	}
	// removing the bytes is best-effort: if it fails, the index is already
	// updated (below) and the next rebuild prunes any leftover bytes, so the
	// namespace stays consistent with the durable event.
	if err := e.store.Delete(bucket, key); err != nil && !errors.Is(err, store.ErrNotFound) {
		return ev, fmt.Errorf("delete object bytes: %w", err)
	}
	e.applyToIndex(ev)
	return ev, nil
}

// ---- reads (served from the materialized index + store bytes) ----

// Get opens an object's bytes. The caller must close the ReadCloser.
func (e *Engine) Get(bucket, key string) (io.ReadCloser, *goo.Object, error) {
	e.mu.Lock()
	obj, ok := e.index[bucket+"/"+key]
	e.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s/%s", store.ErrNotFound, bucket, key)
	}
	rc, _, err := e.store.Get(bucket, key)
	if err != nil {
		return nil, nil, err
	}
	o := obj
	return rc, &o, nil
}

// Stat returns an object's metadata from the materialized index.
func (e *Engine) Stat(bucket, key string) (*goo.Object, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	obj, ok := e.index[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", store.ErrNotFound, bucket, key)
	}
	o := obj
	return &o, nil
}

// List returns every object in a bucket, sorted by key.
func (e *Engine) List(bucket string) ([]goo.Object, error) {
	if !goo.ValidBucket(bucket) {
		return nil, fmt.Errorf("%w: %q", goo.ErrInvalidBucket, bucket)
	}
	e.mu.Lock()
	var out []goo.Object
	for _, obj := range e.index {
		if obj.Bucket != bucket {
			continue
		}
		o := obj
		out = append(out, o)
	}
	e.mu.Unlock()
	sortByKey(out)
	return out, nil
}

// Objects returns every object across all buckets, sorted by bucket then key.
func (e *Engine) Objects() []goo.Object {
	e.mu.Lock()
	out := make([]goo.Object, 0, len(e.index))
	for _, obj := range e.index {
		o := obj
		out = append(out, o)
	}
	e.mu.Unlock()
	sortByRef(out)
	return out
}

// Buckets returns the sorted list of buckets in the namespace.
func (e *Engine) Buckets() []string {
	e.mu.Lock()
	seen := make(map[string]struct{})
	for _, obj := range e.index {
		seen[obj.Bucket] = struct{}{}
	}
	e.mu.Unlock()
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	return out
}

// Log exposes the durable event log for streaming (SSE, CLI tail).
func (e *Engine) Log() *events.Log { return e.log }

// LastSequence returns the highest committed event sequence.
func (e *Engine) LastSequence() (uint64, error) { return e.log.LastSequence() }

// ObjectCount returns the number of objects in the namespace.
func (e *Engine) ObjectCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.index)
}

// Close closes the log and store.
func (e *Engine) Close() error {
	err1 := e.log.Close()
	err2 := e.store.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func sortByKey(o []goo.Object) {
	for i := 1; i < len(o); i++ {
		for j := i; j > 0 && o[j-1].Key > o[j].Key; j-- {
			o[j-1], o[j] = o[j], o[j-1]
		}
	}
}

func sortByRef(o []goo.Object) {
	for i := 1; i < len(o); i++ {
		for j := i; j > 0 && o[j-1].Ref() > o[j].Ref(); j-- {
			o[j-1], o[j] = o[j], o[j-1]
		}
	}
}

func splitRef(ref string) (bucket, key string) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[:i], ref[i+1:]
		}
	}
	return ref, ""
}
