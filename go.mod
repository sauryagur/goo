module github.com/gur/goo

go 1.26

require github.com/tidwall/wal v1.2.1

require (
	github.com/tidwall/gjson v1.10.2 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/tidwall/tinylru v1.1.0 // indirect
)

replace (
	charm.land/bubbles/v2 => github.com/charmbracelet/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 => github.com/charmbracelet/bubbletea/v2 v2.0.9
	charm.land/lipgloss/v2 => github.com/charmbracelet/lipgloss/v2 v2.0.6
)
