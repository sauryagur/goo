package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/gur/goo/internal/engine"
	"github.com/gur/goo/internal/goo"
)

// model is the Bubble Tea model for the GOO terminal UI.
type model struct {
	eng    *engine.Engine
	events []goo.Event // newest-last ordered full history (capped)
	sub    <-chan goo.Event

	vp        viewport.Model
	storageW  int
	treemapW  int
	showTmap  bool
	search    string
	searching bool
	width     int
	height    int
	ready     bool
	err       error
}

// maxEvents caps how many events we keep in memory for the stream view.
const maxEvents = 5000

// NewModel builds the TUI model around an open engine.
func NewModel(eng *engine.Engine) (model, error) {
	// seed the stream from the durable log so history is visible immediately.
	evs, err := eng.Log().Replay(1)
	if err != nil {
		return model{}, fmt.Errorf("replay log for tui: %w", err)
	}
	if len(evs) > maxEvents {
		evs = evs[len(evs)-maxEvents:]
	}
	sub := eng.Log().Subscribe()
	return model{
		eng:    eng,
		events: evs,
		sub:    sub,
	}, nil
}

// Init starts the live tail of new events.
func (m model) Init() tea.Cmd {
	return waitEvent(m.sub)
}

// waitEvent turns a channel receive into a tea.Cmd.
func waitEvent(ch <-chan goo.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return evMsg(ev)
	}
}

type evMsg goo.Event

// Update handles input and incoming events.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case evMsg:
		m.events = append(m.events, goo.Event(msg))
		if len(m.events) > maxEvents {
			m.events = m.events[len(m.events)-maxEvents:]
		}
		// keep the stream glued to the bottom as new events arrive.
		m.vp.GotoBottom()
		m.refreshContent()
		return m, waitEvent(m.sub)

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.Key()

	// global quit (only when not mid-search).
	if !m.searching && k.Text == "q" {
		return m, tea.Quit
	}

	if m.searching {
		switch {
		case k.Code == tea.KeyEsc, k.Code == tea.KeyEnter:
			m.searching = false
			m.refreshContent()
		case k.Code == tea.KeyBackspace:
			if len(m.search) > 0 {
				m.search = m.search[:len(m.search)-1]
				m.refreshContent()
			}
		case k.Text != "":
			m.search += k.Text
			m.refreshContent()
		}
		return m, nil
	}

	switch {
	case k.Code == tea.KeyUp:
		m.vp.ScrollUp(1)
	case k.Code == tea.KeyDown:
		m.vp.ScrollDown(1)
	case k.Code == tea.KeyPgUp:
		m.vp.HalfPageUp()
	case k.Code == tea.KeyPgDown:
		m.vp.HalfPageDown()
	case k.Text == "/":
		m.searching = true
	case k.Text == "r":
		// re-seed from the durable log in case we missed anything.
		evs, err := m.eng.Log().Replay(1)
		if err == nil {
			if len(evs) > maxEvents {
				evs = evs[len(evs)-maxEvents:]
			}
			m.events = evs
			m.refreshContent()
			m.vp.GotoBottom()
		}
	case k.Text == "t":
		m.showTmap = !m.showTmap
		m.layout()
		m.refreshContent()
	}
	return m, nil
}

// layout computes pane widths from the terminal size and whether the treemap
// is shown. Called on every resize so the UI reflows correctly.
func (m *model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	headerH := 3
	footerH := 2
	avail := m.height - headerH - footerH
	if avail < 3 {
		avail = 3
	}

	if m.showTmap {
		// three columns: events | storage | treemap
		m.treemapW = m.width / 4
		if m.treemapW < 10 {
			m.treemapW = 10
		}
		rest := m.width - m.treemapW
		m.storageW = rest / 3
		eventW := rest - m.storageW
		m.vp.SetWidth(eventW - 2)
	} else {
		m.treemapW = 0
		m.storageW = m.width / 3
		eventW := m.width - m.storageW
		m.vp.SetWidth(eventW - 2)
	}
	m.vp.SetHeight(avail)
	m.ready = true
	m.refreshContent()
}

// refreshContent rebuilds the event-stream viewport text.
func (m *model) refreshContent() {
	var b strings.Builder
	shown := m.events
	if m.search != "" {
		shown = eventFilter(m.events, m.search)
	}
	for _, ev := range shown {
		b.WriteString(formatEventLine(ev, m.vp.Width()) + "\n")
	}
	if len(shown) == 0 {
		if m.search != "" {
			b.WriteString("(no events match " + m.search + ")\n")
		} else {
			b.WriteString("(no events yet)\n")
		}
	}
	m.vp.SetContent(b.String())
}

// View renders the whole UI.
func (m model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("loading…")
	}
	header := m.renderHeader()
	eventPane := m.renderEventPane()
	rightPane := m.renderRightPane()
	footer := m.renderFooter()

	mid := lipgloss.JoinHorizontal(lipgloss.Top, eventPane, rightPane)
	full := lipgloss.JoinVertical(lipgloss.Left, header, mid, footer)
	return tea.NewView(full)
}

func (m model) renderHeader() string {
	title := lipgloss.NewStyle().Bold(true).Render("GOO")
	count := fmt.Sprintf("objects: %d", m.eng.ObjectCount())
	pad := m.width - lipgloss.Width(title) - lipgloss.Width(count) - 2
	if pad < 0 {
		pad = 0
	}
	bar := title + strings.Repeat(" ", pad) + count
	return lipgloss.NewStyle().
		Background(lipgloss.Color("63")).
		Foreground(lipgloss.Color("0")).
		Width(m.width).
		Render(bar)
}

func (m model) renderEventPane() string {
	title := lipgloss.NewStyle().Bold(true).Render(" EVENT STREAM ")
	sep := strings.Repeat("─", maxInt(0, m.vp.Width()-lipgloss.Width(title)))
	header := lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Render(title + sep)
	return header + "\n" + m.vp.View()
}

func (m model) renderRightPane() string {
	if m.showTmap && m.treemapW > 0 {
		storage := m.renderStoragePanel()
		tmap := m.renderTreemapPanel()
		return lipgloss.JoinVertical(lipgloss.Left, storage, tmap)
	}
	return m.renderStoragePanel()
}

func (m model) renderStoragePanel() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")).Render(" STORAGE ")
	objs := m.eng.Objects()
	body := storageSummary(objs)
	return title + "\n" + body
}

func (m model) renderTreemapPanel() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")).Render(" TREEMAP ")
	objs := m.eng.Objects()
	h := m.vp.Height() - 2
	if h < 3 {
		h = 3
	}
	w := m.treemapW - 2
	if w < 4 {
		w = 4
	}
	tmap := RenderTreemap(objs, w, h)
	return title + "\n" + tmap
}

func (m model) renderFooter() string {
	var hint string
	if m.searching {
		hint = fmt.Sprintf("search: %s", m.search)
	} else {
		hint = "↑↓ nav  PgUp/PgDn  / search  r replay  t treemap  q quit"
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color("240")).
		Foreground(lipgloss.Color("252")).
		Width(m.width).
		Render(hint)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
