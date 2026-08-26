package main

import (
	"flag"
	"fmt"

	"github.com/gur/goo/internal/engine"
)

// cmdServer starts the HTTP object API + SSE event stream.
// Implemented in internal/api (Phase 5).
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
	// the api package owns the HTTP wiring; we hand it the engine.
	return runServer(*addr, e)
}

// runServer is defined in internal/api once the HTTP layer lands.
// Until then it returns a clear message so the CLI is runnable for local ops.
var runServer = func(addr string, e *engine.Engine) error {
	return fmt.Errorf("server not wired yet (HTTP layer coming in phase 5)")
}
