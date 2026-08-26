package events

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gur/goo/internal/goo"
)

func newTestLog(t *testing.T) *Log {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestAppendReadOrdering(t *testing.T) {
	l := newTestLog(t)

	var prev uint64
	for i := 0; i < 50; i++ {
		ev, err := l.Append(goo.ActionPut, "b", "k"+string(rune('a'+i%26)), int64(i), "h", uint64(i+1))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if ev.Sequence != prev+1 {
			t.Fatalf("sequence not monotonic: got %d want %d", ev.Sequence, prev+1)
		}
		prev = ev.Sequence

		got, err := l.Read(ev.Sequence)
		if err != nil {
			t.Fatalf("read seq %d: %v", ev.Sequence, err)
		}
		if got.Sequence != ev.Sequence || got.Action != ev.Action {
			t.Fatalf("roundtrip mismatch: %+v vs %+v", got, ev)
		}
	}

	last, _ := l.LastSequence()
	if last != 50 {
		t.Fatalf("last sequence = %d, want 50", last)
	}
	first, _ := l.FirstSequence()
	if first != 1 {
		t.Fatalf("first sequence = %d, want 1", first)
	}
}

func TestEmptyLog(t *testing.T) {
	l := newTestLog(t)
	empty, err := l.IsEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("new log should be empty")
	}
	last, _ := l.LastSequence()
	if last != 0 {
		t.Fatalf("empty log last = %d, want 0", last)
	}
	first, _ := l.FirstSequence()
	if first != 1 {
		t.Fatalf("empty log first = %d, want 1 (AllowEmpty policy)", first)
	}
	if _, err := l.Read(1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(1) on empty log: got %v, want ErrNotFound", err)
	}
}

func TestReplay(t *testing.T) {
	l := newTestLog(t)
	// write 10 then delete them 10, total 20.
	for i := 0; i < 10; i++ {
		l.Append(goo.ActionPut, "b", "k", int64(i), "h", uint64(i+1))
	}
	evs, err := l.Replay(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 10 {
		t.Fatalf("replay(1) len = %d, want 10", len(evs))
	}
	if evs[0].Sequence != 1 || evs[len(evs)-1].Sequence != 10 {
		t.Fatalf("replay bounds wrong: %d..%d", evs[0].Sequence, evs[len(evs)-1].Sequence)
	}

	// replay from 5 should give 6 events (5..10)
	evs, err = l.Replay(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 6 || evs[0].Sequence != 5 {
		t.Fatalf("replay(5) = %d events, first %d", len(evs), evs[0].Sequence)
	}

	// replay from beyond end returns empty, no error.
	evs, err = l.Replay(999)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("replay beyond end = %d events, want 0", len(evs))
	}
}

func TestSequencePersistenceAfterRestart(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		if _, err := l.Append(goo.ActionPut, "b", "k", int64(i), "h", uint64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen: sequences must survive and continue from where they left off.
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	last, _ := l2.LastSequence()
	if last != 7 {
		t.Fatalf("after restart last = %d, want 7", last)
	}
	ev, err := l2.Append(goo.ActionPut, "b", "k", 8, "h", 8)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Sequence != 8 {
		t.Fatalf("sequence after restart = %d, want 8 (must not reset to 1)", ev.Sequence)
	}
}

func TestUniqueIDPerMutation(t *testing.T) {
	l := newTestLog(t)
	// identical payload PUT to two different keys -> different IDs.
	a, _ := l.Append(goo.ActionPut, "b", "k1", 100, "samehash", 1)
	b, _ := l.Append(goo.ActionPut, "b", "k2", 100, "samehash", 1)
	if a.ID == b.ID {
		t.Fatalf("identical payloads must have distinct IDs: %q", a.ID)
	}
	// re-PUT to same key -> new ID.
	c, _ := l.Append(goo.ActionPut, "b", "k1", 100, "samehash", 2)
	if c.ID == a.ID {
		t.Fatalf("re-PUT must get a new ID: %q", c.ID)
	}
}

func TestSubscribeLiveTail(t *testing.T) {
	l := newTestLog(t)

	// historical events are NOT redelivered by subscribe; do them first.
	l.Append(goo.ActionPut, "b", "old", 1, "h", 1)

	ch := l.Subscribe()

	want := 5
	for i := 0; i < want; i++ {
		l.Append(goo.ActionPut, "b", "live", int64(i), "h", uint64(i+1))
	}

	got := 0
	for i := 0; i < want; i++ {
		ev := <-ch
		if ev.Key != "live" {
			t.Fatalf("expected live event, got key %q", ev.Key)
		}
		got++
	}
	if got != want {
		t.Fatalf("got %d live events, want %d", got, want)
	}
	l.Unsubscribe(ch)
}

func TestSubscribeUnblocksOnClose(t *testing.T) {
	l := newTestLog(t)
	ch := l.Subscribe()
	// a goroutine blocked on the channel must not leak after Close.
	done := make(chan struct{})
	go func() {
		<-ch
		close(done)
	}()
	l.Close()
	select {
	case <-done:
		// channel closed -> reader unblocked.
	case <-time.After(2 * time.Second):
		t.Fatal("reader still blocked after Close")
	}
}

func TestScanBucketFilter(t *testing.T) {
	l := newTestLog(t)
	l.Append(goo.ActionPut, "images", "a.jpg", 1, "h", 1)
	l.Append(goo.ActionPut, "videos", "b.mp4", 1, "h", 1)
	l.Append(goo.ActionPut, "images", "c.jpg", 1, "h", 1)

	count := 0
	err := l.Scan(1, "images", func(goo.Event) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("bucket scan = %d events, want 2", count)
	}
}

func TestConcurrentAppend(t *testing.T) {
	l := newTestLog(t)
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			l.Append(goo.ActionPut, "b", "k", int64(i), "h", uint64(i+1))
		}(i)
	}
	wg.Wait()

	last, _ := l.LastSequence()
	if last != n {
		t.Fatalf("after %d concurrent appends, last = %d", n, last)
	}
	// every sequence 1..n must exist exactly once.
	seen := make(map[uint64]bool)
	for s := uint64(1); s <= uint64(n); s++ {
		ev, err := l.Read(s)
		if err != nil {
			t.Fatalf("missing sequence %d: %v", s, err)
		}
		if seen[ev.Sequence] {
			t.Fatalf("duplicate sequence %d", ev.Sequence)
		}
		seen[ev.Sequence] = true
	}
}

// TestCorruptRecord ensures a single corrupted entry doesn't silently pass as
// valid data; Read surfaces the error.
func TestCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(goo.ActionPut, "b", "k", 1, "h", 1)
	l.Close()

	// smash the data file contents to force a parse failure on read.
	entries, _ := filepath.Glob(filepath.Join(dir, "*"))
	// the wal stores data in a seg file; corrupt the first one we find.
	var seg string
	for _, e := range entries {
		if fi, _ := os.Stat(e); fi != nil && !fi.IsDir() && fi.Size() > 0 {
			seg = e
			break
		}
	}
	if seg != "" {
		_ = os.WriteFile(seg, []byte("\x00\x01not-a-valid-entry-bytes"), 0o600)
	}

	l2, err := Open(dir)
	if err != nil {
		// a corrupt log may fail to open; either way we must not silently
		// report good data.
		return
	}
	defer l2.Close()
	if _, err := l2.Read(1); err == nil {
		t.Fatal("expected error reading corrupt record, got nil")
	}
}
