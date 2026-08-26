// Package store is the raw object blob + metadata layer of GOO.
//
// It is deliberately dumb: it knows how to write, read, delete, list and stat
// object bytes and their metadata on the local filesystem. It does NOT emit
// events and does NOT understand the materialized object index. That
// coordination lives in internal/engine, which calls this package and then the
// event log together under the commit protocol.
//
// Filesystem layout (all names are validated; nothing here is ever a
// user-supplied path component):
//
//	<root>/data/<bucket>/<2-char-shard>/<url-encoded-key>
//	<root>/meta/<bucket>/<2-char-shard>/<url-encoded-key>.json
//
// The 2-char shard spreads many keys across sub-directories so a hot bucket
// doesn't produce one giant directory. Keys are url-encoded on disk so that a
// key containing a slash (hierarchical) becomes its own path segment safely.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gur/goo/internal/goo"
)

// Errors returned by the store.
var (
	ErrNotFound   = errors.New("store: object not found")
	ErrExists     = errors.New("store: object already exists")
	ErrInvalidRef = errors.New("store: invalid bucket/key")
)

// ObjectMeta is the on-disk metadata document for one object.
type ObjectMeta struct {
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	Hash      string    `json:"hash"`
	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is a local filesystem-backed object store.
type Store struct {
	root string

	mu   sync.Mutex             // serializes metadata reads/writes for a stable version counter
	meta map[string]*ObjectMeta // in-memory mirror; rebuilt from disk on open
}

// Open opens (and, on first use, initializes) a store rooted at root.
func Open(root string) (*Store, error) {
	for _, sub := range []string{"data", "meta"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o750); err != nil {
			return nil, fmt.Errorf("create store dir %q: %w", sub, err)
		}
	}
	s := &Store{
		root: root,
		meta: make(map[string]*ObjectMeta),
	}
	if err := s.reindex(); err != nil {
		return nil, fmt.Errorf("reindex store: %w", err)
	}
	return s, nil
}

// reindex walks the meta directory and loads every object's metadata into the
// in-memory mirror. It is called on Open so the store survives a restart with
// its version counters intact.
func (s *Store) reindex() error {
	metaRoot := filepath.Join(s.root, "meta")
	return filepath.Walk(metaRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read meta %q: %w", path, err)
		}
		var m ObjectMeta
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("corrupt meta %q: %w", path, err)
		}
		s.meta[m.Bucket+"/"+m.Key] = &m
		return nil
	})
}

// Root returns the store's on-disk root.
func (s *Store) Root() string { return s.root }

// ---- path helpers (never trust user input as a path component) ----

// shard returns a stable 2-char prefix for a bucket/key pair so we can spread
// objects across sub-directories. It is derived from a hash, so it's
// deterministic and collision-spreading, not guessable.
func shardFor(bucket, key string) string {
	sum := sha256.Sum256([]byte(bucket + "\x00" + key))
	return hex.EncodeToString(sum[:])[:2]
}

// dataPath returns the on-disk path for an object's bytes.
func (s *Store) dataPath(bucket, key string) string {
	return filepath.Join(s.root, "data", bucket, shardFor(bucket, key), url.PathEscape(key))
}

// metaPath returns the on-disk path for an object's metadata document.
func (s *Store) metaPath(bucket, key string) string {
	return filepath.Join(s.root, "meta", bucket, shardFor(bucket, key), url.PathEscape(key)+".json")
}

// ensureParent makes the parent directory of a path, rejecting any attempt to
// write outside the store root (defence in depth against path traversal).
func (s *Store) ensureParent(path string) error {
	parent := filepath.Dir(path)
	if !s.contains(parent) {
		return fmt.Errorf("%w: resolved path %q escapes store root", ErrInvalidRef, path)
	}
	return os.MkdirAll(parent, 0o750)
}

// contains reports whether path is inside the store root.
func (s *Store) contains(path string) bool {
	clean := filepath.Clean(path)
	return clean == s.root || strings.HasPrefix(clean, s.root+string(os.PathSeparator))
}

// ---- CRUD ----

// Put writes an object's bytes. If the object already exists, opts.Overwrite
// decides behavior: when false, Put returns ErrExists and writes nothing; when
// true, it replaces the bytes and bumps the version.
//
// Put is responsible ONLY for durable bytes + metadata. It returns the new
// ObjectMeta. The caller (engine) is responsible for appending the matching
// event afterwards.
func (s *Store) Put(bucket, key string, data io.Reader, opts PutOptions) (*ObjectMeta, error) {
	if err := goo.CheckRef(bucket, key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.meta[bucket+"/"+key]
	if existing != nil && !opts.Overwrite {
		return nil, fmt.Errorf("%w: %s/%s", ErrExists, bucket, key)
	}

	// hash while streaming so we pay for it once.
	h := sha256.New()
	tmp := s.dataPath(bucket, key) + ".tmp"
	if err := s.ensureParent(tmp); err != nil {
		return nil, err
	}
	// write to a temp file first, then atomically rename. A crash mid-write
	// leaves the old object (or nothing) in place, never a half-written blob.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create temp object: %w", err)
	}
	size, err := io.Copy(io.MultiWriter(f, h), data)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("write object bytes: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, fmt.Errorf("sync object bytes: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("close object bytes: %w", err)
	}

	// atomic swap into place.
	final := s.dataPath(bucket, key)
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("rename object bytes: %w", err)
	}

	now := time.Now().UTC()
	version := uint64(1)
	if existing != nil {
		version = existing.Version + 1
	}
	m := &ObjectMeta{
		Bucket:    bucket,
		Key:       key,
		Size:      size,
		Hash:      hex.EncodeToString(h.Sum(nil)),
		Version:   version,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing != nil {
		m.CreatedAt = existing.CreatedAt
	}
	if err := s.writeMeta(m); err != nil {
		return nil, err
	}
	s.meta[bucket+"/"+key] = m
	return m, nil
}

// writeMeta atomically persists an object's metadata document.
func (s *Store) writeMeta(m *ObjectMeta) error {
	path := s.metaPath(m.Bucket, m.Key)
	if err := s.ensureParent(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write meta temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename meta: %w", err)
	}
	return nil
}

// PutOptions controls Put behavior.
type PutOptions struct {
	Overwrite bool
}

// Get opens an object's bytes for reading. The caller must close the returned
// ReadCloser.
func (s *Store) Get(bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	if err := goo.CheckRef(bucket, key); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	s.mu.Lock()
	m := s.meta[bucket+"/"+key]
	s.mu.Unlock()
	if m == nil {
		return nil, nil, ErrNotFound
	}
	f, err := os.Open(s.dataPath(bucket, key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("open object bytes: %w", err)
	}
	return f, m, nil
}

// Stat returns an object's metadata without reading its bytes.
func (s *Store) Stat(bucket, key string) (*ObjectMeta, error) {
	if err := goo.CheckRef(bucket, key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	s.mu.Lock()
	m := s.meta[bucket+"/"+key]
	s.mu.Unlock()
	if m == nil {
		return nil, ErrNotFound
	}
	// return a copy so callers can't mutate the store's mirror.
	cp := *m
	return &cp, nil
}

// Delete removes an object's bytes and metadata. Deleting a missing object
// returns ErrNotFound (the engine turns that into a no-op or error as needed).
func (s *Store) Delete(bucket, key string) error {
	if err := goo.CheckRef(bucket, key); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRef, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta[bucket+"/"+key] == nil {
		return ErrNotFound
	}
	// remove bytes first, then metadata. Either way the store stays
	// consistent: if bytes removal fails we abort; if meta removal fails the
	// bytes are gone but meta lingers (rare, recoverable on next reindex which
	// would then point at missing bytes -> treat as not-found).
	if err := os.Remove(s.dataPath(bucket, key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove object bytes: %w", err)
	}
	if err := os.Remove(s.metaPath(bucket, key)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove object meta: %w", err)
	}
	delete(s.meta, bucket+"/"+key)
	return nil
}

// ListItem is one entry returned by List.
type ListItem struct {
	Bucket  string
	Key     string
	Size    int64
	Hash    string
	Version uint64
}

// List returns every object in a bucket, sorted by key. An empty bucket returns
// an empty slice, not an error.
func (s *Store) List(bucket string) ([]ListItem, error) {
	if !goo.ValidBucket(bucket) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRef, bucket)
	}
	s.mu.Lock()
	var items []ListItem
	for ref, m := range s.meta {
		if m.Bucket != bucket {
			continue
		}
		_ = ref
		items = append(items, ListItem{
			Bucket:  m.Bucket,
			Key:     m.Key,
			Size:    m.Size,
			Hash:    m.Hash,
			Version: m.Version,
		})
	}
	s.mu.Unlock()

	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	return items, nil
}

// ListAll returns the "bucket/key" refs of every object currently on disk,
// regardless of the in-memory mirror. Used by the engine for orphan pruning.
func (s *Store) ListAll() ([]string, error) {
	root := filepath.Join(s.root, "meta")
	var refs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		// path: <root>/meta/<bucket>/<shard>/<key>.json
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			return nil
		}
		bucket := parts[0]
		key := strings.TrimSuffix(parts[2], ".json")
		key, err = url.PathUnescape(key)
		if err != nil {
			return fmt.Errorf("bad key on disk %q: %w", parts[2], err)
		}
		refs = append(refs, bucket+"/"+key)
		return nil
	})
	return refs, err
}

// Close releases store resources. The metadata mirror is rebuilt on the next
// Open, so nothing needs flushing here, but we provide it for symmetry with
// the event log and for defer-based cleanup.
func (s *Store) Close() error {
	s.mu.Lock()
	s.meta = nil
	s.mu.Unlock()
	return nil
}

// Buckets returns the sorted list of bucket names present in the store.
func (s *Store) Buckets() ([]string, error) {
	s.mu.Lock()
	seen := make(map[string]struct{})
	for _, m := range s.meta {
		seen[m.Bucket] = struct{}{}
	}
	s.mu.Unlock()
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, nil
}
