module github.com/gur/goo

go 1.26

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.0.9
	charm.land/lipgloss/v2 v2.0.6
	github.com/jeffwilliams/squarify v0.0.0-20150517023534-f38712eec14e
	github.com/tidwall/wal v1.2.1
)

replace (
	charm.land/bubbles/v2 => github.com/charmbracelet/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 => github.com/charmbracelet/bubbletea/v2 v2.0.9
	charm.land/lipgloss/v2 => github.com/charmbracelet/lipgloss/v2 v2.0.6
)
