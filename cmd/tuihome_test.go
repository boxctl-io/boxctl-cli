package cmd

import (
	"strings"
	"testing"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	tea "github.com/charmbracelet/bubbletea"
)

// boxesTUI is boxctl tui with :boxes open over home, where most tests
// start (the app itself starts on home).
func boxesTUI(c *client.Client) tuiModel {
	m := newTUI(c, boxesApp)
	m.pages = append(m.pages, page{kind: pageTable, view: boxesView, sortCol: -1})
	return m
}

// boardTUI is boxctl board with :backlog open over home.
func boardTUI(c *client.Client) tuiModel {
	m := newTUI(c, boardApp)
	m.pages = append(m.pages, page{kind: pageTable, view: backlogView, sortCol: -1})
	return m
}

func TestTUIHome(t *testing.T) {
	m := newTUI(nil, boxesApp)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(tuiModel)
	m.loaded["boxes"] = true
	m.vms = []client.VM{{Name: "a", State: "running"}, {Name: "b", State: "paused"}}

	if m.top().kind != pageHome {
		t.Fatalf("the tui starts on %v, want home", m.top().kind)
	}
	view := m.View()
	for _, want := range []string{"Home[5]", "Boxes", "Public URLs", "Images", "Sizes", "Backups",
		"1 running, 1 paused", "b  :boxes", "<home>"} {
		if !strings.Contains(view, want) {
			t.Errorf("home lacks %q:\n%s", want, view)
		}
	}
	for _, not := range []string{"Backlog", "Runs", "GitHub Repos"} {
		if strings.Contains(view, not) {
			t.Errorf("boxctl tui's home shows %q, which is the board's", not)
		}
	}

	// 120 columns fit 3 tiles: right, then down, lands on the fifth.
	if got := m.homeCols(); got != 3 {
		t.Fatalf("homeCols = %d, want 3", got)
	}
	m, _ = press(t, m, "right")
	m, _ = press(t, m, "down")
	if got := m.app.tiles[m.top().cursor].view; got != backupsView {
		t.Fatalf("cursor on %s, want backups", got.name)
	}
	// Down from the last row stays put; up from the first too.
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "up")
	if got := m.app.tiles[m.top().cursor].view; got != urlsView {
		t.Fatalf("cursor on %s, want urls", got.name)
	}

	// enter opens the tile's view over home; esc comes back to it.
	m, _ = press(t, m, "enter")
	if top := m.top(); top.kind != pageTable || top.view != urlsView || len(m.pages) != 2 {
		t.Fatalf("enter opened %+v, want urls over home", top)
	}
	m, _ = press(t, m, "esc")
	if m.top().kind != pageHome {
		t.Fatalf("esc went to %v, want home", m.top().kind)
	}

	// The one-key switches work on home; :home goes to the split above
	// both sides from anywhere.
	m, _ = press(t, m, "b")
	if m.top().view != boxesView {
		t.Fatalf("b opened %v, want boxes", m.top().view)
	}
	next, _ = m.runCommand("home")
	m = next.(tuiModel)
	if !m.atTop {
		t.Fatal(":home didn't go to the split")
	}
}

func TestBoardHome(t *testing.T) {
	m := newTUI(nil, boardApp)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(tuiModel)
	view := m.View()
	for _, want := range []string{"Home[4]", "Backlog", "Runs", "GitHub Repos", "t  :backlog", "u  :runs", "h  :repos"} {
		if !strings.Contains(view, want) {
			t.Errorf("board home lacks %q:\n%s", want, view)
		}
	}
	for _, not := range []string{"Public URLs", "Backups", "b  :boxes"} {
		if strings.Contains(view, not) {
			t.Errorf("the board's home shows %q, which is boxctl tui's", not)
		}
	}
	m, _ = press(t, m, "enter")
	if m.top().view != backlogView {
		t.Fatalf("enter opened %v, want backlog", m.top().view)
	}
	// c makes no box on the board's home; :create switches to the boxes.
	m.goHome()
	m, _ = press(t, m, "c")
	if m.create != nil {
		t.Error("c on the board's home opened the create form")
	}
	next, _ = m.runCommand("create")
	m = next.(tuiModel)
	if m.create == nil || m.app != boxesApp {
		t.Errorf(":create on the board: create=%v app=%s", m.create != nil, m.app.name)
	}
}
