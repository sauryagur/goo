// Package api is the HTTP front-end for GOO. It owns NO storage logic: every
// handler delegates to the engine, which is the single owner of mutations and
// reads. This keeps the HTTP layer a thin, testable translation between the
// wire format and the engine's API.
//
// Routes (all under /v1):
//
//	PUT    /v1/objects/{bucket}/{key...}   store an object
//	GET    /v1/objects/{bucket}/{key...}   fetch an object's bytes
//	HEAD   /v1/objects/{bucket}/{key...}   fetch metadata only
//	DELETE /v1/objects/{bucket}/{key...}   delete an object
//	GET    /v1/buckets/{bucket}            list objects in a bucket (JSON)
//	GET    /v1/buckets/{bucket}/events     stream the durable event log (SSE)
//
// Status codes are chosen to be useful and unsurprising: 201 on a fresh PUT,
// 200 on GET/HEAD/DELETE/list, 400 on an invalid bucket/key, 404 when the
// object is missing, 500 only for genuine internal failures. Filesystem
// internals are never leaked into the response body.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gur/goo/internal/engine"
	"github.com/gur/goo/internal/goo"
)

// Server is the GOO HTTP API.
type Server struct {
	eng *engine.Engine
	mux *http.ServeMux
	sse *SSEHub // set by the sse sub-package during construction
}

// NewServer builds an HTTP server around the engine.
func NewServer(eng *engine.Engine) *Server {
	s := &Server{eng: eng, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the http.Handler for the API (useful for tests via httptest).
func (s *Server) Handler() http.Handler { return s.mux }

// AttachSSE plugs the SSE event hub into the same mux under
// GET /v1/buckets/{bucket}/events. It is called by the sse sub-package once the
// hub is constructed; until then that route simply 404s.
func (s *Server) AttachSSE(h *SSEHub) {
	s.sse = h
	s.mux.HandleFunc("GET /v1/buckets/{bucket}/events", h.Handle)
}

func (s *Server) routes() {
	s.mux.HandleFunc("PUT /v1/objects/{bucket}/{rest...}", s.handlePut)
	s.mux.HandleFunc("GET /v1/objects/{bucket}/{rest...}", s.handleGet)
	s.mux.HandleFunc("HEAD /v1/objects/{bucket}/{rest...}", s.handleHead)
	s.mux.HandleFunc("DELETE /v1/objects/{bucket}/{rest...}", s.handleDelete)
	s.mux.HandleFunc("GET /v1/buckets/{bucket}", s.handleList)
}

// refFromPath reconstructs (bucket, key) from the URL pattern. The key is the
// wildcard remainder; we re-validate it because the URL path is untrusted.
func refFromPath(r *http.Request) (bucket, key string, err error) {
	bucket = r.PathValue("bucket")
	key = r.PathValue("rest")
	if key == "" {
		return "", "", fmt.Errorf("missing object key")
	}
	if err := goo.CheckRef(bucket, key); err != nil {
		return "", "", err
	}
	return bucket, key, nil
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	bucket, key, err := refFromPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bucket/key: "+err.Error())
		return
	}
	// cap body size so a hostile client can't exhaust memory.
	const maxBytes = 1 << 30 // 1 GiB
	limited := io.LimitReader(r.Body, maxBytes)
	ev, err := s.eng.Put(bucket, key, limited, true)
	if err != nil {
		// distinguish "not found" style from internal.
		writeError(w, http.StatusInternalServerError, "failed to store object")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"bucket":   ev.Bucket,
		"key":      ev.Key,
		"version":  ev.Version,
		"size":     ev.Size,
		"hash":     ev.Hash,
		"sequence": ev.Sequence,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	bucket, key, err := refFromPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bucket/key: "+err.Error())
		return
	}
	rc, obj, err := s.eng.Get(bucket, key)
	if err != nil {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-GOO-Bucket", obj.Bucket)
	w.Header().Set("X-GOO-Key", obj.Key)
	w.Header().Set("X-GOO-Version", fmt.Sprintf("%d", obj.Version))
	w.Header().Set("X-GOO-Hash", obj.Hash)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", obj.Size))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// headers already sent; just log.
		log.Printf("api: get stream error: %v", err)
	}
}

func (s *Server) handleHead(w http.ResponseWriter, r *http.Request) {
	bucket, key, err := refFromPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bucket/key: "+err.Error())
		return
	}
	obj, err := s.eng.Stat(bucket, key)
	if err != nil {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-GOO-Bucket", obj.Bucket)
	w.Header().Set("X-GOO-Key", obj.Key)
	w.Header().Set("X-GOO-Version", fmt.Sprintf("%d", obj.Version))
	w.Header().Set("X-GOO-Hash", obj.Hash)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", obj.Size))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	bucket, key, err := refFromPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bucket/key: "+err.Error())
		return
	}
	ev, err := s.eng.Delete(bucket, key)
	if err != nil {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"bucket":   ev.Bucket,
		"key":      ev.Key,
		"sequence": ev.Sequence,
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if !goo.ValidBucket(bucket) {
		writeError(w, http.StatusBadRequest, "invalid bucket name")
		return
	}
	objs, err := s.eng.List(bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list bucket")
		return
	}
	out := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		out = append(out, map[string]any{
			"bucket":     o.Bucket,
			"key":        o.Key,
			"version":    o.Version,
			"size":       o.Size,
			"hash":       o.Hash,
			"created_at": o.CreatedAt,
			"updated_at": o.UpdatedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"bucket": bucket, "objects": out})
}
