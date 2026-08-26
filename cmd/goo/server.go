package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/gur/goo/internal/api"
	"github.com/gur/goo/internal/engine"
)

// cmdServer starts the HTTP object API and the SSE event stream.
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

	log.Printf("goo server listening on %s (root %s)", *addr, root)
	return http.ListenAndServe(*addr, srv.Handler())
}
