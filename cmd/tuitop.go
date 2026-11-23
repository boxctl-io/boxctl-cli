package cmd

import (
	"fmt"
	"strings"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The split: the page above both sides of the tui (tuiapp.go), where bare
// `boxctl` starts -- a tile for the boxes and one for the board, each with
// a live summary. enter opens a side's home; esc on a side's home and `:home` come back here. ctrl-t switches
// sides from anywhere without coming back.

// topTileH is a split tile's height, its border included.
const topTileH = 6

// appIndex is a's place on the split.
func appIndex(a *tuiApp) int {
	for i, o := range tuiApps {
		if o == a {
			return i
		}
	}
	return 0
}

// goTop shows the split. The side's pages stay, for enter to come back to.
func (m *tuiModel) goTop() {
	m.atTop = true
	m.topCursor = appIndex(m.app)
	m.filter.SetValue("")
}

// enter shows side a where it was left (its home the first time), and
// starts the backlog's passes the first time it's the board.
func (m *tuiModel) enter(a *tuiApp) tea.Cmd {
	m.atTop = false
	if a == m.app {
		return nil
	}
	m.stacks[m.app] = m.pages
	m.app = a
	m.pages = m.stacks[a]
	if len(m.pages) == 0 {
		m.pages = []page{homePage(a)}
	}
	m.filter.SetValue("")
	m.command.SetSuggestions(a.commandNames())
	var cmds []tea.Cmd
	if a.backlog && !m.backlogOn {
		m.backlogOn = true
		cmds = append(cmds, loadBacklogMsg, m.backlogPass())
	}
	if t := m.top(); t.kind == pageTable && !t.view.onDemand {
		cmds = append(cmds, m.list(t.view.resource))
	}
	return tea.Batch(cmds...)
}

// topHotkeys are the header's keys on the split.
var topHotkeys = []hotkey{{"enter", "open"}, {"←→", "move"}, {"ctrl-t", "switch side"}}

// handleTopKey is the split's keys.
func (m tuiModel) handleTopKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "right", "l", "tab", "down", "j":
		m.topCursor = min(m.topCursor+1, len(tuiApps)-1)
	case "left", "h", "shift+tab", "up", "k":
		m.topCursor = max(m.topCursor-1, 0)
	case "enter":
		return m, m.enter(tuiApps[m.topCursor])
	case ":":
		m.commanding = true
		m.command.SetValue("")
		return m, m.command.Focus()
	case "?":
		m.atTop = false
		m.pushHelp()
	case "R", "ctrl+r", "f5":
		return m, m.refresh()
	case "q":
		return m, tea.Quit
	}
	return m, nil
}

// topView renders the split: the two sides' tiles, side by side when they
// fit, else one above the other.
func (m tuiModel) topView(w, inner int) (string, []string) {
	tileW := min(max((w-4-homeTileGap)/2, homeTileW), 60)
	side := 2+2*tileW+homeTileGap <= w
	if !side {
		tileW = min(max(w-4, 20), 60)
	}
	var tiles [][]string
	for i, a := range tuiApps {
		tiles = append(tiles, m.topTile(a, tileW, i == m.topCursor))
	}
	lines := []string{""}
	if side {
		for row := 0; row < topTileH; row++ {
			lines = append(lines, " "+tiles[0][row]+strings.Repeat(" ", homeTileGap)+tiles[1][row])
		}
	} else {
		for _, t := range tiles {
			for _, l := range t {
				lines = append(lines, " "+l)
			}
		}
	}
	lines = append(lines, "", " "+k9s.dim.Render("ctrl-t switches between the two from anywhere; esc on a side's home comes back here."))
	return k9s.title.Render("Home"), lines
}

// topTile is one side's tile: its name and views, what it is, and two lines
// of what's going on there.
func (m tuiModel) topTile(a *tuiApp, width int, selected bool) []string {
	inner := width - 2
	border, name := k9s.border, k9s.header.Render(" "+a.label+" ")
	if selected {
		border, name = k9s.title, k9s.selected.Render(" "+a.label+" ")
	}
	views := k9s.hotKey.Render(":" + strings.ReplaceAll(a.viewNames(), ", ", " :"))
	head := name + strings.Repeat(" ", max(inner-lipgloss.Width(name)-lipgloss.Width(views)-1, 1)) + views
	first, second := m.topSummary(a)
	body := []string{head, " " + k9s.dim.Render(fit(a.blurb, inner-1)), " " + fit(first, inner-1), " " + fit(second, inner-1)}
	out := []string{border.Render("╭" + strings.Repeat("─", inner) + "╮")}
	for _, l := range body {
		out = append(out, border.Render("│")+padRight(fit(l, inner), inner)+border.Render("│"))
	}
	return append(out, border.Render("╰"+strings.Repeat("─", inner)+"╯"))
}

// topSummary is a side's two live lines on the split.
func (m tuiModel) topSummary(a *tuiApp) (string, string) {
	loading := k9s.dim.Render("Loading…")
	if a == boxesApp {
		if !m.loaded["boxes"] {
			return loading, ""
		}
		live, paused := 0, 0
		urls := 0
		for _, vm := range m.vms {
			switch stateRank(vm.State) {
			case 0:
				live++
			case 1:
				paused++
			}
			urls += len(vm.Ingress)
		}
		return k9s.infoVal.Render(fmt.Sprintf("%d running, %d paused", live, paused)),
			k9s.dim.Render(plural(urls, "public URL"))
	}
	if m.backlog == nil {
		return loading, ""
	}
	needs, going := 0, 0
	for _, it := range m.backlog.Items {
		if it.State == backlog.NeedsYou {
			needs++
		}
		if it.Current() != nil {
			going++
		}
	}
	first := plural(len(m.backlog.Items), "goal")
	if needs > 0 {
		first += fmt.Sprintf(", %d need you", needs)
	}
	second := k9s.dim.Render("the backlog runs once you open the board")
	switch {
	case m.backlogHeld != "":
		second = k9s.dim.Render("the backlog is run by " + m.backlogHeld)
	case m.backlogOn:
		second = k9s.infoVal.Render(fmt.Sprintf("%d running; the backlog is running here", going))
	}
	return k9s.infoVal.Render(first), second
}
