package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/gur/goo/internal/engine"
)

// Run starts the GOO terminal UI and blocks until the user quits. It returns
// the engine's error if startup fails.
func Run(eng *engine.Engine) error {
	m, err := NewModel(eng)
	if err != nil {
		return err
	}
	p := tea.NewProgram(m)
	_, err = p.Run()
	return err
}
