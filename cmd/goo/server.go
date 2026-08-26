package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/gur/goo/internal/api"
	"github.com/gur/goo/internal/engine"
)

// cmdServer starts the HTTP object API and the SSE event stream. It shuts down
// gracefully on SIGINT/SIGTERM so in-flight requests and the log aren't cut
// off mid-write.
func cmdServer(args []string, root string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := engine.Open(root)
	if err != nil {
		return err
	}
	defer e.Close()

	srv := api.NewServer(e)
	srv.AttachSSE(api.NewSSEHub(e.Log()))

	s := &http.Server{Addr: *addr, Handler: srv.Handler()}

	// stop on signal for a clean shutdown (no goroutine/port leak).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("goo server shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutCtx)
	}()

	log.Printf("goo server listening on %s (root %s)", *addr, root)
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
