package cmd

import "strings"

// A tuiApp is one of the tui's two sides, on one model, its tables and
// rendering:
//
//   - boxesApp: the boxes and what goes with them -- public URLs, images,
//     sizes, backups. `boxctl tui` starts on it.
//   - boardApp: the agent work -- the backlog of goals, the runs working
//     on them, the GitHub repositories to start from. `boxctl board`
//     starts on it.
//
// Bare `boxctl` starts above both, on the split (tuitop.go), and ctrl-t
// switches between them from anywhere; each side keeps its own page
// stack, so switching back finds the page left. The backlog's passes
// start the first time the board is shown and run until the tui quits
// (tuiModel.backlogOn), so a session that never opens the board never
// starts or kills a run.
//
// A side is only which views it has (its home tiles, its one-key
// switches, the ones the : prompt offers first), what it lists, and
// whether it creates boxes; every key and listing in a view is the same
// in either. A view of the other side at the : prompt switches to it.
type tuiApp struct {
	name     string       // the command that starts on it: "tui", "board"
	label    string       // its title on the split and its crumb: "Boxes"
	blurb    string       // what it is, on the split
	views    []*tableView // the : prompt's views, in tab completion's order
	tiles    []homeTile
	screens  []screenKey
	listings []string // listed at start for the tiles' counts, besides the boxes
	backlog  bool     // shows the backlog: the passes start when it's first shown
	creates  bool     // c on its home makes a box
}

// screenKey switches to a view with one keystroke from home or any table.
type screenKey struct {
	key  string
	view *tableView
}

var boxesApp = &tuiApp{
	name:  "tui",
	label: "Boxes",
	blurb: "Your machines, their URLs, images, sizes and backups",
	views: []*tableView{boxesView, urlsView, imagesView, sizesView, backupsView},
	tiles: []homeTile{
		{boxesView, "Your machines, running and paused", "box"},
		{urlsView, "Box ports at public URLs", "URL"},
		{imagesView, "What a new box boots from", "image"},
		{sizesView, "CPU, memory and disk presets", "size"},
		{backupsView, "Snapshots to restore as a box", "backup"},
	},
	screens:  []screenKey{{"b", boxesView}, {"u", urlsView}},
	listings: []string{"images", "sizes", "backups"},
	creates:  true,
}

var boardApp = &tuiApp{
	name:  "board",
	label: "Board",
	blurb: "The agent work: goals, their runs, your repos",
	views: []*tableView{backlogView, runsView, reposView, prsView},
	tiles: []homeTile{
		{backlogView, "Goals waiting for a run, and PRs", "goal"},
		{runsView, "Every attempt at a goal", "run"},
		{reposView, "GitHub repos to start Claude on", "repo"},
		{prsView, "Open PRs of a repo: ask Claude about one", "pr"},
	},
	screens: []screenKey{{"t", backlogView}, {"u", runsView}, {"h", reposView}},
	backlog: true,
}

// screenFor is the view key switches to, nil for none.
func (a *tuiApp) screenFor(key string) *tableView {
	for _, s := range a.screens {
		if s.key == key {
			return s.view
		}
	}
	return nil
}

// screenHotkeys are the screen keys for the header and the help page.
func (a *tuiApp) screenHotkeys() []hotkey {
	var keys []hotkey
	for _, s := range a.screens {
		keys = append(keys, hotkey{s.key, ":" + s.view.name})
	}
	return keys
}

// tileKey is the one-key switch to v, "" for none.
func (a *tuiApp) tileKey(v *tableView) string {
	for _, s := range a.screens {
		if s.view == v {
			return s.key
		}
	}
	return ""
}

// viewNames are the app's views' names, comma-separated, for messages.
func (a *tuiApp) viewNames() string {
	var names []string
	for _, v := range a.views {
		names = append(names, v.name)
	}
	return strings.Join(names, ", ")
}

// tuiApps are the sides, in the split's order.
var tuiApps = []*tuiApp{boxesApp, boardApp}

// other is the side ctrl-t switches to.
func (a *tuiApp) other() *tuiApp {
	if a == boardApp {
		return boxesApp
	}
	return boardApp
}

// commandNames are what the : prompt suggests, in the order it offers
// them (tab takes the first match): this side's views, the other side's,
// then the other commands.
func (a *tuiApp) commandNames() []string {
	var names []string
	for _, v := range append(append([]*tableView{}, a.views...), a.other().views...) {
		names = append(names, v.name)
	}
	return append(names, "home", "board", "create", "help", "ssh", "quit")
}

// findView is the view `:name` means in this app, nil for none.
func (a *tuiApp) findView(name string) *tableView {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, v := range a.views {
		if v.name == name {
			return v
		}
		for _, al := range v.aliases {
			if al == name {
				return v
			}
		}
	}
	return nil
}

// appOf is the side with view v.
func appOf(v *tableView) *tuiApp {
	for _, a := range tuiApps {
		for _, av := range a.views {
			if av == v {
				return a
			}
		}
	}
	return boxesApp
}
