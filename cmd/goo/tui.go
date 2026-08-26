package main

import (
	"flag"
	"fmt"

	"github.com/gur/goo/internal/engine"
)

// cmdTUI launches the Bubble Tea terminal UI. Implemented in internal/tui
// (Phase 8). Until then it returns a clear message so the CLI is runnable for
// local operations.
func cmdTUI(args []string, root string) error {
	_ = flag.NewFlagSet("tui", flag.ContinueOnError)
	e, err := engine.Open(root)
	if err != nil {
		return err
	}
	return runTUI(e)
}

// runTUI is wired up in internal/tui once the Bubble Tea UI lands.
var runTUI = func(e *engine.Engine) error {
	return fmt.Errorf("tui not wired yet (Bubble Tea UI coming in phase 8)")
}
