package api_test

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gur/goo/internal/api"
	"github.com/gur/goo/internal/engine"
)

// TestIntegrationPutThenSSEThenReconnect is the end-to-end flow required by the
// spec:
//
//	start server
//	    -> PUT object            (event persisted)
//	    -> SSE consumer receives event
//	    -> disconnect consumer
//	    -> perform additional mutations
//	    -> reconnect from previous sequence (Last-Event-ID)
//	    -> missing events are replayed
func TestIntegrationPutThenSSEThenReconnect(t *testing.T) {
	e, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	srv := api.NewServer(e)
	srv.AttachSSE(api.NewSSEHub(e.Log()))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	put := func(bucket, key, body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/objects/"+bucket+"/"+key, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put %s/%s: %v", bucket, key, err)
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("put %s/%s: status %d", bucket, key, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// 1) initial mutation
	put("images", "a.jpg", "first")

	// 2) open an SSE consumer and confirm it receives the event.
	resp1, err := http.Get(ts.URL + "/v1/buckets/images/events?from=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	lastSeen := readSSEEventID(t, resp1.Body, 2*time.Second)
	if lastSeen == 0 {
		t.Fatal("consumer did not receive the initial event")
	}
	if lastSeen != 1 {
		t.Fatalf("expected to see sequence 1, saw %d", lastSeen)
	}

	// 3) disconnect the consumer.
	resp1.Body.Close()

	// 4) perform additional mutations while nobody is listening.
	put("images", "b.jpg", "second")
	put("images", "c.jpg", "third")
	put("images", "d.jpg", "fourth")

	// 5) reconnect, resuming from the last sequence we saw (1), so we should
	//    get the three events we missed (2,3,4) replayed.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/buckets/images/events", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(lastSeen, 10))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	seen := readSSEIDs(t, resp2.Body, 3, 3*time.Second)
	// we should have received sequences 2, 3, 4 (lastSeen+1 onward).
	want := []uint64{lastSeen + 1, lastSeen + 2, lastSeen + 3}
	if len(seen) < len(want) {
		t.Fatalf("reconnect replayed %v, want at least %v", seen, want)
	}
	for i, w := range want {
		if seen[i] != w {
			t.Fatalf("replay event %d = %d, want %d (seen=%v)", i, seen[i], w, seen)
		}
	}
}

// readSSEEventID reads a single SSE frame and returns its id (sequence).
func readSSEEventID(t *testing.T, r io.Reader, timeout time.Duration) uint64 {
	t.Helper()
	ids := readSSEIDs(t, r, 1, timeout)
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

// readSSEIDs reads up to n SSE frames (or until timeout) and returns their ids.
func readSSEIDs(t *testing.T, r io.Reader, n int, timeout time.Duration) []uint64 {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ids []uint64
	deadline := time.After(timeout)
	for len(ids) < n {
		ch := make(chan struct{})
		var line string
		var ok bool
		go func() {
			ok = sc.Scan()
			if ok {
				line = sc.Text()
			}
			close(ch)
		}()
		select {
		case <-ch:
		case <-deadline:
			return ids
		}
		if !ok {
			return ids
		}
		if strings.HasPrefix(line, "id: ") {
			if v, err := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64); err == nil {
				ids = append(ids, v)
			}
		}
	}
	return ids
}
