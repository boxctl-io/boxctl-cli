package cmd

import (
	"strings"
	"testing"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	tea "github.com/charmbracelet/bubbletea"
)

// boxesTUI is the tui with :boxes open over home, where most tests start
// (the tui itself starts on home).
func boxesTUI(c *client.Client) tuiModel {
	m := newTUI(c)
	m.pages = append(m.pages, page{kind: pageTable, view: boxesView, sortCol: -1})
	return m
}

func TestTUIHome(t *testing.T) {
	m := newTUI(nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(tuiModel)
	m.loaded["boxes"] = true
	m.vms = []client.VM{{Name: "a", State: "running"}, {Name: "b", State: "paused"}}

	if m.top().kind != pageHome {
		t.Fatalf("the tui starts on %v, want home", m.top().kind)
	}
	view := m.View()
	for _, want := range []string{"Home[8]", "Boxes", "Backlog", "Runs", "GitHub Repos", "Public URLs", "Backups",
		"1 running, 1 paused", "b  :boxes", "<home>"} {
		if !strings.Contains(view, want) {
			t.Errorf("home lacks %q:\n%s", want, view)
		}
	}

	// 120 columns fit 3 tiles: right, then down, lands on the fifth.
	if got := m.homeCols(); got != 3 {
		t.Fatalf("homeCols = %d, want 3", got)
	}
	m, _ = press(t, m, "right")
	m, _ = press(t, m, "down")
	if got := homeTiles[m.top().cursor].view; got != urlsView {
		t.Fatalf("cursor on %s, want urls", got.name)
	}
	// Down from the last row stays put; up from the first too.
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "up")
	if got := homeTiles[m.top().cursor].view; got != backlogView {
		t.Fatalf("cursor on %s, want backlog", got.name)
	}

	// enter opens the tile's view over home; esc comes back to it.
	m, _ = press(t, m, "enter")
	if top := m.top(); top.kind != pageTable || top.view != backlogView || len(m.pages) != 2 {
		t.Fatalf("enter opened %+v, want backlog over home", top)
	}
	m, _ = press(t, m, "esc")
	if m.top().kind != pageHome {
		t.Fatalf("esc went to %v, want home", m.top().kind)
	}

	// The one-key switches work on home; :home comes back from anywhere.
	m, _ = press(t, m, "u")
	if m.top().view != runsView {
		t.Fatalf("u opened %v, want runs", m.top().view)
	}
	next, _ = m.runCommand("home")
	m = next.(tuiModel)
	if len(m.pages) != 1 || m.top().kind != pageHome {
		t.Fatalf(":home left %d pages, top %v", len(m.pages), m.top().kind)
	}
}
