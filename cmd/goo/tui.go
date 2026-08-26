package main

import (
	"flag"

	"github.com/gur/goo/internal/engine"
	"github.com/gur/goo/internal/tui"
)

// cmdTUI launches the Bubble Tea terminal UI.
func cmdTUI(args []string, root string) error {
	if err := flag.CommandLine.Parse(args); err != nil {
		return err
	}
	e, err := engine.Open(root)
	if err != nil {
		return err
	}
	defer e.Close()
	return tui.Run(e)
}
