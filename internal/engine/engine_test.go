package engine

import (
	"bytes"
	"io"
	"testing"

	"github.com/gur/goo/internal/goo"
	"github.com/gur/goo/internal/store"
)

func openEngine(t *testing.T) *Engine {
	t.Helper()
	root := t.TempDir()
	e, err := Open(root)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestPutProducesEvent(t *testing.T) {
	e := openEngine(t)
	ev, err := e.Put("b", "k.txt", bytes.NewReader([]byte("hello")), true)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Action != goo.ActionPut || ev.Sequence != 1 {
		t.Fatalf("event wrong: %+v", ev)
	}
	// the object must now be readable
	rc, obj, err := e.Get("b", "k.txt")
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	rc.Close()
	if obj.Size != 5 || obj.Version != 1 {
		t.Fatalf("object meta wrong: %+v", obj)
	}

	// and a historical read of the log shows the event
	last, _ := e.LastSequence()
	if last != 1 {
		t.Fatalf("last sequence = %d, want 1", last)
	}
}

func TestEveryMutationHasEventNoSilentLoss(t *testing.T) {
	e := openEngine(t)
	// 3 puts + 1 delete = 4 events, sequences 1..4
	e.Put("b", "a", bytes.NewReader([]byte("1")), true)
	e.Put("b", "b", bytes.NewReader([]byte("2")), true)
	e.Put("b", "c", bytes.NewReader([]byte("3")), true)
	e.Delete("b", "b")

	last, _ := e.LastSequence()
	if last != 4 {
		t.Fatalf("last = %d, want 4", last)
	}
	// replay the whole log and count events
	evs, err := e.Log().Replay(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("replay = %d events, want 4", len(evs))
	}
	// the namespace must reflect: a, c present; b deleted
	if _, err := e.Stat("b", "b"); err == nil {
		t.Fatal("deleted object still present in index")
	}
	if _, err := e.Stat("b", "a"); err != nil {
		t.Fatal("a missing")
	}
	if _, err := e.Stat("b", "c"); err != nil {
		t.Fatal("c missing")
	}
}

func TestFailedMutationNoFalseEvent(t *testing.T) {
	e := openEngine(t)
	// delete a missing object -> no event
	if _, err := e.Delete("b", "ghost"); err == nil {
		t.Fatal("delete missing should error")
	}
	if last, _ := e.LastSequence(); last != 0 {
		t.Fatalf("missing-object delete created events: last=%d", last)
	}
}

func TestVersionCorrectness(t *testing.T) {
	e := openEngine(t)
	e.Put("b", "k", bytes.NewReader([]byte("v1")), true)
	ev2, _ := e.Put("b", "k", bytes.NewReader([]byte("v2-longer")), true)
	if ev2.Version != 2 {
		t.Fatalf("version = %d, want 2", ev2.Version)
	}
	if ev2.Size != 9 {
		t.Fatalf("size = %d, want 9", ev2.Size)
	}
}

func TestCrashRecoveryMaterializedView(t *testing.T) {
	root := t.TempDir()
	e, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	e.Put("b", "k1", bytes.NewReader([]byte("one")), true)
	e.Put("b", "k2", bytes.NewReader([]byte("two")), true)
	e.Delete("b", "k1")
	e.Close()

	// reopen: index must be reconstructed from the log, not from disk bytes.
	e2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()

	// k1 was deleted -> not in namespace, even though its bytes were pruned.
	if _, err := e2.Stat("b", "k1"); err == nil {
		t.Fatal("k1 should not survive (it was deleted and pruned)")
	}
	if _, err := e2.Stat("b", "k2"); err != nil {
		t.Fatalf("k2 missing after recovery: %v", err)
	}
	// no orphan bytes should remain on disk for k1.
	if _, err := e2.store.Stat("b", "k1"); err == nil {
		t.Fatal("orphan bytes for k1 should have been pruned")
	}
}

func TestOrphanPruning(t *testing.T) {
	// simulate the crash window: bytes written but event never appended.
	root := t.TempDir()
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// write an object directly to the store (no event).
	if _, err := st.Put("b", "orphan", bytes.NewReader([]byte("zombie")), store.PutOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// opening the engine must prune it because the log has no such event.
	e, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Stat("b", "orphan"); err == nil {
		t.Fatal("orphan object survived engine open; should have been pruned")
	}
	if _, err := e.store.Stat("b", "orphan"); err == nil {
		t.Fatal("orphan bytes survived; pruning failed")
	}
}

func TestConcurrentPutNoLostEvents(t *testing.T) {
	e := openEngine(t)
	const n = 40
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			key := "k" + string(rune('a'+(i%26))) + string(rune('0'+(i/26)))
			e.Put("b", key, bytes.NewReader([]byte("x")), true)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	last, _ := e.LastSequence()
	if last != n {
		t.Fatalf("after %d concurrent puts, last=%d (events lost?)", n, last)
	}
	if e.ObjectCount() != n {
		t.Fatalf("object count = %d, want %d", e.ObjectCount(), n)
	}
}

func TestGetReadsBytes(t *testing.T) {
	e := openEngine(t)
	e.Put("b", "f", bytes.NewReader([]byte("payload-data")), true)
	rc, _, err := e.Get("b", "f")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf, _ := io.ReadAll(rc)
	if string(buf) != "payload-data" {
		t.Fatalf("get bytes = %q", buf)
	}
}
