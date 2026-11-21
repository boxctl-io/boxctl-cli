package cmd

import (
	"fmt"
	"strings"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The tui's home page: a tile per view, the agent flow's four first
// (boxes, backlog, runs, repos), each with what it shows and a live count
// from the listings already loaded. Arrows (or j/k, tab) move between
// tiles and enter opens one; the one-key screen switches (b/t/u/h) and
// `:` work here as in a table. It is the bottom of the navigation stack:
// the tui starts on it, a view opened from anywhere sits on top of it, and
// esc from a view comes back to it (`:home` from anywhere).

// homeTile is one tile: the view it opens and the line saying what it is.
type homeTile struct {
	view  *tableView
	blurb string
	noun  string // what a row is, for the count: "image" -> "3 images"
}

var homeTiles = []homeTile{
	{boxesView, "Your machines, running and paused", "box"},
	{backlogView, "Goals waiting for a run, and PRs", "goal"},
	{runsView, "Every attempt at a goal", "run"},
	{reposView, "GitHub repos to start Claude on", "repo"},
	{urlsView, "Box ports at public URLs", "URL"},
	{imagesView, "What a new box boots from", "image"},
	{sizesView, "CPU, memory and disk presets", "size"},
	{backupsView, "Snapshots to restore as a box", "backup"},
}

// Tile geometry: tileW/tileH include the tile's own border.
const (
	homeTileW   = 36
	homeTileH   = 5
	homeTileGap = 2
)

// homeListings are the listings the tui asks for at start, besides the
// boxes and the backlog, so home's tiles have counts: the server's own,
// which are quick. :repos asks GitHub, so it waits until it's opened.
var homeListings = []string{"images", "sizes", "backups"}

// homePage is the page the tui starts on.
func homePage() page { return page{kind: pageHome, title: "home"} }

// goHome makes home the only page.
func (m *tuiModel) goHome() {
	m.pages = []page{homePage()}
	m.filter.SetValue("")
}

// homeCols is how many tiles fit across the box (its borders and margin off).
func (m tuiModel) homeCols() int {
	return max(1, (max(m.width-2, 1)-1+homeTileGap)/(homeTileW+homeTileGap))
}

// tileKey is the one-key switch to v, "" for none.
func tileKey(v *tableView) string {
	for _, s := range screenKeys {
		if s.view == v {
			return s.key
		}
	}
	return ""
}

// tileCount is the tile's live line: what the view would show, from the
// listings already loaded (nothing is listed for the home page alone, but
// boxes and the backlog always are).
func (m tuiModel) tileCount(ht homeTile) string {
	v := ht.view
	if !m.loaded[v.resource] {
		return ""
	}
	switch v {
	case boxesView:
		live, paused := 0, 0
		for _, vm := range m.vms {
			switch stateRank(vm.State) {
			case 0:
				live++
			case 1:
				paused++
			}
		}
		return fmt.Sprintf("%d running, %d paused", live, paused)
	case backlogView:
		if m.backlog == nil {
			return ""
		}
		needs := 0
		for _, it := range m.backlog.Items {
			if it.State == backlog.NeedsYou {
				needs++
			}
		}
		s := plural(len(m.backlog.Items), "goal")
		if needs > 0 {
			s += fmt.Sprintf(", %d need you", needs)
		}
		return s
	case runsView:
		going, all := 0, 0
		for _, r := range m.runRows() {
			all++
			if r.run.Ended == nil {
				going++
			}
		}
		return fmt.Sprintf("%d going, %s", going, plural(all, "run"))
	}
	return plural(len(v.rows(&m)), ht.noun)
}

// plural is "1 box", "3 goals".
func plural(n int, noun string) string {
	switch {
	case n == 1:
		return fmt.Sprintf("%d %s", n, noun)
	case strings.HasSuffix(noun, "x"):
		return fmt.Sprintf("%d %ses", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// homeView renders the tiles, scrolled so the selected one's row shows.
func (m tuiModel) homeView(w, inner int) (string, []string) {
	t := m.top()
	cols := m.homeCols()
	rowsFit := max(1, (inner-1)/homeTileH)
	first := 0
	if r := t.cursor / cols; r >= rowsFit {
		first = r - rowsFit + 1
	}
	lines := []string{""}
	for start := first * cols; start < len(homeTiles) && len(lines)+homeTileH <= inner+1; start += cols {
		end := min(start+cols, len(homeTiles))
		var tiles [][]string
		for i := start; i < end; i++ {
			tiles = append(tiles, m.tile(homeTiles[i], i == t.cursor))
		}
		for row := 0; row < homeTileH; row++ {
			var parts []string
			for _, tl := range tiles {
				parts = append(parts, tl[row])
			}
			lines = append(lines, " "+strings.Join(parts, strings.Repeat(" ", homeTileGap)))
		}
	}
	return k9s.title.Render("Home") + k9s.count.Render(fmt.Sprintf("[%d]", len(homeTiles))), lines
}

// tile is one tile's lines: a border (aqua when selected), the view's
// title with its key, the blurb, and the count.
func (m tuiModel) tile(ht homeTile, selected bool) []string {
	inner := homeTileW - 2
	border := k9s.border
	name := k9s.header.Render(" " + ht.view.title + " ")
	if selected {
		border = k9s.title
		name = k9s.selected.Render(" " + ht.view.title + " ")
	}
	key := ":" + ht.view.name
	if k := tileKey(ht.view); k != "" {
		key = k + "  " + key
	}
	head := name
	head += strings.Repeat(" ", max(inner-lipgloss.Width(head)-lipgloss.Width(key)-1, 1)) + k9s.hotKey.Render(key)
	count := k9s.infoVal.Render(m.tileCount(ht))
	if !m.loaded[ht.view.resource] {
		count = k9s.dim.Render("listed when opened")
		if m.loading[ht.view.resource] || ht.view.resource == "boxes" || ht.view.resource == "backlog" {
			count = k9s.dim.Render("Loading…")
		}
	}
	body := []string{head, " " + k9s.dim.Render(fit(ht.blurb, inner-1)), " " + fit(count, inner-1)}
	out := []string{border.Render("╭" + strings.Repeat("─", inner) + "╮")}
	for _, l := range body {
		out = append(out, border.Render("│")+padRight(fit(l, inner), inner)+border.Render("│"))
	}
	return append(out, border.Render("╰"+strings.Repeat("─", inner)+"╯"))
}

// homeHotkeys are the header's keys on the home page.
var homeHotkeys = []hotkey{{"enter", "open"}, {"←↑↓→", "move"}}

// handleHomeKey is the home page's keys.
func (m tuiModel) handleHomeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := m.top()
	key := msg.String()
	if v := screenFor(key); v != nil {
		return m, m.openView(v)
	}
	cols := m.homeCols()
	move := func(n int) { t.cursor = min(max(t.cursor+n, 0), len(homeTiles)-1) }
	switch key {
	case "right", "l", "tab":
		move(1)
	case "left", "shift+tab":
		move(-1)
	case "down", "j":
		if t.cursor+cols < len(homeTiles) {
			move(cols)
		}
	case "up", "k":
		if t.cursor-cols >= 0 {
			move(-cols)
		}
	case "home", "g":
		t.cursor = 0
	case "end", "G":
		t.cursor = len(homeTiles) - 1
	case "enter":
		return m, m.openView(homeTiles[t.cursor].view)
	case ":":
		m.commanding = true
		m.command.SetValue("")
		return m, m.command.Focus()
	case "?":
		m.pushHelp()
	case "c":
		return m.openCreate("", "")
	case "R", "ctrl+r", "f5":
		return m, m.refresh()
	case "q":
		return m, m.say("type :q to quit")
	}
	return m, nil
}
