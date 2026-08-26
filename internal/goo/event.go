// Package goo holds the core domain types shared across GOO's subsystems.
//
// The central idea of GOO is that the object namespace is a materialized view
// of an append-only mutation log. These types are the vocabulary used by both
// the durable event log (internal/events) and the object store (internal/store).
package goo

import "time"

// Mutation action names. They are stored as strings in the durable log so the
// log format stays forward-readable, but we keep typed constants to avoid typos.
const (
	ActionPut    = "PUT"
	ActionDelete = "DELETE"
)

// SSE event names mirror the actions but are lowercased and namespaced so a
// browser client can switch on them directly.
const (
	EventNamePut    = "object.put"
	EventNameDelete = "object.delete"
)

// Event is one durable, ordered mutation of the object namespace.
//
// Every successful mutation (PUT or DELETE) produces exactly one Event that is
// appended to the durable log before the mutation is reported as committed. The
// Sequence number is monotonically increasing and unique; it doubles as the SSE
// event id, which is what makes reconnect/resume trivial.
type Event struct {
	// Sequence is the global, monotonic position of this event in the log.
	Sequence uint64 `json:"sequence"`
	// ID is a content-derived unique id (sha256 of the object's data for PUTs,
	// a synthetic id for deletes). Useful for idempotency debugging.
	ID string `json:"id"`
	// Action is one of ActionPut / ActionDelete.
	Action string `json:"action"`
	// Bucket is the object's bucket.
	Bucket string `json:"bucket"`
	// Key is the object's key within the bucket.
	Key string `json:"key"`
	// Size is the object's byte size (0 for deletes).
	Size int64 `json:"size"`
	// Hash is the sha256 hex of the object's data (empty for deletes).
	Hash string `json:"hash"`
	// Version is the per-(bucket,key) monotonically increasing version.
	Version uint64 `json:"version"`
	// Timestamp is when the mutation was committed (UTC).
	Timestamp time.Time `json:"timestamp"`
}

// Object is the materialized metadata for a single object.
//
// It is derived entirely from the event log, so it never needs its own
// durability beyond what the log provides.
type Object struct {
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	Hash      string    `json:"hash"`
	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Ref returns the "bucket/key" address of the object.
func (o Object) Ref() string {
	return o.Bucket + "/" + o.Key
}
