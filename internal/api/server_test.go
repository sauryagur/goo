package api

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gur/goo/internal/engine"
)

func newTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	e, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	srv := NewServer(e)
	srv.AttachSSE(NewSSEHub(e.Log()))
	return srv, func() { e.Close() }
}

func putObj(t *testing.T, h http.Handler, bucket, key, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/objects/"+bucket+"/"+key, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("put %s/%s: code %d body %s", bucket, key, w.Code, w.Body.String())
	}
	return w.Code
}

func TestHTTPPutGetDeleteHeadList(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	putObj(t, h, "b", "k.txt", "hello")

	// GET
	req := httptest.NewRequest(http.MethodGet, "/v1/objects/b/k.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("get: code %d body %q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-GOO-Version") == "" {
		t.Fatal("missing version header")
	}

	// HEAD
	req = httptest.NewRequest(http.MethodHead, "/v1/objects/b/k.txt", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("head: code %d body %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Length") != "5" {
		t.Fatalf("head content-length = %q", w.Header().Get("Content-Length"))
	}

	// LIST
	req = httptest.NewRequest(http.MethodGet, "/v1/buckets/b", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "k.txt") {
		t.Fatalf("list: code %d body %q", w.Code, w.Body.String())
	}

	// DELETE
	req = httptest.NewRequest(http.MethodDelete, "/v1/objects/b/k.txt", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("delete: code %d", w.Code)
	}

	// GET after delete -> 404
	req = httptest.NewRequest(http.MethodGet, "/v1/objects/b/k.txt", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("get after delete: code %d, want 404", w.Code)
	}
}

func TestHTTPInvalidRef(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	// a path-traversal key must never be written/read. The mux may 307-redirect
	// the cleaned path (which is itself safe) or 400 — either way it must not
	// be a 2xx success that wrote a file.
	req := httptest.NewRequest(http.MethodPut, "/v1/objects/b/../../etc/passwd", strings.NewReader("x"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK || w.Code == http.StatusCreated {
		t.Fatalf("traversal accepted: code %d", w.Code)
	}
}

func TestSSEHistoricalReplay(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	putObj(t, h, "b", "k1", "1")
	putObj(t, h, "b", "k2", "2")

	// ?from=1 should replay both events.
	req := httptest.NewRequest(http.MethodGet, "/v1/buckets/b/events?from=1", nil)
	w := httptest.NewRecorder()
	// run in a goroutine so we can read partial output then cancel.
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel() // disconnect after we've read the history
	}()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "event: object.put") {
		t.Fatalf("replay missing put event:\n%s", body)
	}
	// count data lines with sequence ids
	if strings.Count(body, "id: ") < 2 {
		t.Fatalf("expected >=2 events in replay, got:\n%s", body)
	}
}

func TestSSELiveDelivery(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	// open SSE, then perform a mutation, expect it delivered live.
	req := httptest.NewRequest(http.MethodGet, "/v1/buckets/b/events?from=1", nil)
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, req)
		close(done)
	}()

	// give the handler a moment to subscribe.
	time.Sleep(50 * time.Millisecond)
	putObj(t, h, "b", "live", "ping")

	// wait for delivery, then stop the handler before we read its recorder
	// (httptest.ResponseRecorder is not safe for concurrent read+write).
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if !strings.Contains(w.Body.String(), "live") {
		t.Fatalf("live event not delivered:\n%s", w.Body.String())
	}
}

func TestSSEResumeFromLastEventID(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	putObj(t, h, "b", "a", "1")
	putObj(t, h, "b", "b", "2")
	putObj(t, h, "b", "c", "3")

	// reconnect with Last-Event-ID: 2 -> should receive events 3 onward only.
	req := httptest.NewRequest(http.MethodGet, "/v1/buckets/b/events", nil)
	req.Header.Set("Last-Event-ID", "2")
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "id: 1\n") || strings.Contains(body, "id: 2\n") {
		t.Fatalf("resume sent events before Last-Event-ID:\n%s", body)
	}
	if !strings.Contains(body, "id: 3\n") {
		t.Fatalf("resume missing event 3:\n%s", body)
	}
}

func TestSSEBucketFilter(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	putObj(t, h, "images", "a.jpg", "1")
	putObj(t, h, "videos", "b.mp4", "2")

	req := httptest.NewRequest(http.MethodGet, "/v1/buckets/images/events?from=1", nil)
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `"bucket":"images"`) {
		t.Fatalf("expected images event:\n%s", body)
	}
	if strings.Contains(body, `"bucket":"videos"`) {
		t.Fatalf("bucket filter leaked videos event:\n%s", body)
	}
}

func TestSSEMultipleConsumers(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	const n = 5
	recs := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		req := httptest.NewRecorder()
		recs[i] = req
	}
	// start all consumers
	ctxs := make([]context.CancelFunc, n)
	recDone := make([]chan struct{}, n)
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/buckets/b/events?from=1", nil)
		ctx, cancel := context.WithCancel(req.Context())
		ctxs[i] = cancel
		done := make(chan struct{})
		recDone[i] = done
		req = req.WithContext(ctx)
		go func(i int) {
			h.ServeHTTP(recs[i], req)
			close(done)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)

	putObj(t, h, "b", "shared", "x")
	time.Sleep(200 * time.Millisecond)
	// stop the consumers first so the handler goroutines finish writing before
	// we read their recorders (httptest.ResponseRecorder isn't safe for
	// concurrent read+write — that's a test-harness limitation, not a bug in
	// the server, which writes only to its own http.ResponseWriter).
	for i := 0; i < n; i++ {
		ctxs[i]()
	}
	for i := 0; i < n; i++ {
		<-recDone[i]
		if !strings.Contains(recs[i].Body.String(), "shared") {
			t.Fatalf("consumer %d missed shared event:\n%s", i, recs[i].Body.String())
		}
	}
}

// TestSSEClientDisconnect ensures a cancelled context stops the handler
// (no goroutine leak / hang).
func TestSSEClientDisconnect(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/v1/buckets/b/events?from=1", nil)
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, req); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel() // simulate client disconnect
	select {
	case <-done:
		// handler returned promptly on disconnect.
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
}

// helper: parse the last SSE id from a buffer (used in manual inspection).
func lastEventID(b *bytes.Buffer) uint64 {
	sc := bufio.NewScanner(b)
	var last uint64
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			if n, err := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64); err == nil {
				last = n
			}
		}
	}
	return last
}
