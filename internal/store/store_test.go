package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return s
}

func putString(t *testing.T, s *Store, bucket, key, data string) *ObjectMeta {
	t.Helper()
	m, err := s.Put(bucket, key, bytes.NewReader([]byte(data)), PutOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
	return m
}

func TestPutGetDelete(t *testing.T) {
	s := newStore(t)
	m := putString(t, s, "b", "k.txt", "hello goo")

	if m.Size != int64(len("hello goo")) {
		t.Fatalf("size = %d", m.Size)
	}
	if m.Version != 1 {
		t.Fatalf("version = %d, want 1", m.Version)
	}
	if m.Hash == "" {
		t.Fatal("hash empty")
	}

	// GET roundtrip
	rc, got, err := s.Get("b", "k.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	buf, _ := io.ReadAll(rc)
	if string(buf) != "hello goo" {
		t.Fatalf("get data = %q", buf)
	}
	if got.Hash != m.Hash || got.Size != m.Size {
		t.Fatal("meta mismatch on get")
	}

	// DELETE
	if err := s.Delete("b", "k.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.Get("b", "k.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete, get = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat("b", "k.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete, stat = %v, want ErrNotFound", err)
	}
}

func TestChecksumStable(t *testing.T) {
	s := newStore(t)
	a := putString(t, s, "b", "x", "identical content")
	b := putString(t, s, "b", "y", "identical content")
	// same content -> same hash
	if a.Hash != b.Hash {
		t.Fatalf("same content produced different hashes: %q vs %q", a.Hash, b.Hash)
	}
	c := putString(t, s, "b", "z", "different content")
	if a.Hash == c.Hash {
		t.Fatal("different content produced same hash")
	}
}

func TestOverwriteVersioning(t *testing.T) {
	s := newStore(t)
	m1 := putString(t, s, "b", "k", "v1")
	m2 := putString(t, s, "b", "k", "v2-longer")
	if m2.Version != m1.Version+1 {
		t.Fatalf("version did not increment: %d -> %d", m1.Version, m2.Version)
	}
	if m2.Size != int64(len("v2_longer")) {
		t.Fatalf("overwrite size wrong: %d", m2.Size)
	}
	// created_at must be preserved across overwrite
	if !m2.CreatedAt.Equal(m1.CreatedAt) {
		t.Fatal("overwrite changed created_at")
	}
}

func TestMissingObject(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Get("b", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	if err := s.Delete("b", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
	if _, err := s.Stat("b", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat missing = %v", err)
	}
}

func TestMissingBucketList(t *testing.T) {
	s := newStore(t)
	items, err := s.List("ghost")
	if err != nil {
		t.Fatalf("list missing bucket err: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("list missing bucket = %d items, want 0", len(items))
	}
	if _, err := s.List("../escape"); err == nil {
		t.Fatal("invalid bucket should be rejected")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	s := newStore(t)
	// these must never reach the real filesystem root.
	cases := []struct{ bucket, key string }{
		{"b", "../../etc/passwd"},
		{"b", "/etc/passwd"},
		{"b", ".."},
		{"../etc", "passwd"},
		{"b", "a/../../b"},
	}
	for _, c := range cases {
		_, err := s.Put(c.bucket, c.key, bytes.NewReader([]byte("x")), PutOptions{Overwrite: true})
		if err == nil {
			t.Fatalf("path traversal not rejected: %s/%s", c.bucket, c.key)
		}
		// ensure nothing actually landed outside the store root.
	}
}

func TestListAndBuckets(t *testing.T) {
	s := newStore(t)
	putString(t, s, "images", "a.jpg", "1")
	putString(t, s, "images", "b.jpg", "2")
	putString(t, s, "images", "c/long.jpg", "3")
	putString(t, s, "videos", "v.mp4", "4")

	items, err := s.List("images")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("images list = %d, want 3", len(items))
	}
	if items[0].Key != "a.jpg" || items[2].Key != "c/long.jpg" {
		t.Fatalf("list not sorted: %+v", items)
	}

	buckets, err := s.Buckets()
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 || buckets[0] != "images" || buckets[1] != "videos" {
		t.Fatalf("buckets = %v", buckets)
	}
}

func TestRestartReconstruction(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	putString(t, s, "b", "k1", "one")
	putString(t, s, "b", "k2", "two")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen: metadata must be reconstructed from disk.
	s2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	m1, err := s2.Stat("b", "k1")
	if err != nil {
		t.Fatalf("k1 not reconstructed: %v", err)
	}
	if m1.Size != 3 {
		t.Fatalf("k1 size after restart = %d, want 3", m1.Size)
	}
	// version counter must survive so the next put is version 1 (new key).
	m3, err := s2.Put("b", "k3", bytes.NewReader([]byte("three")), PutOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if m3.Version != 1 {
		t.Fatalf("new key version after restart = %d, want 1", m3.Version)
	}
	// overwrite k1: version should be 2 (continuing from disk).
	m1b, err := s2.Put("b", "k1", bytes.NewReader([]byte("one!")), PutOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if m1b.Version != 2 {
		t.Fatalf("overwrite after restart version = %d, want 2", m1b.Version)
	}
}

func TestConcurrentPutGet(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	const n = 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('a'+(i%26))) + string(rune('0'+(i/26)))
			putString(t, s, "b", key, "payload")
		}(i)
	}
	wg.Wait()

	items, err := s.List("b")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("after concurrent puts, list = %d, want %d", len(items), n)
	}
	// every listed object must be readable.
	for _, it := range items {
		rc, _, err := s.Get("b", it.Key)
		if err != nil {
			t.Fatalf("get %s after concurrent puts: %v", it.Key, err)
		}
		rc.Close()
	}
}

func TestAtomicPutNoPartialOnError(t *testing.T) {
	s := newStore(t)
	// a failing reader should not leave a .tmp turd in the store root's data dir.
	bad := &errReader{}
	_, err := s.Put("b", "k", bad, PutOptions{Overwrite: true})
	if err == nil {
		t.Fatal("expected error from bad reader")
	}
	// no temp file should remain at the data root.
	entries, _ := filepath.Glob(filepath.Join(s.Root(), "data", "b", "*", "k.tmp"))
	if len(entries) != 0 {
		t.Fatalf("leftover temp file: %v", entries)
	}
}

type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, io.ErrClosedPipe }
