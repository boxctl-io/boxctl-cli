package cmd

import "strings"

// A tuiApp is one of the two full-screen apps built on the tui's model,
// tables and rendering:
//
//   - `boxctl tui` (and bare `boxctl`), boxesApp: the boxes and what goes
//     with them -- public URLs, images, sizes, backups. It never runs the
//     backlog.
//   - `boxctl board`, boardApp: the agent work -- the backlog of goals,
//     the runs working on them, the GitHub repositories to start from. It
//     runs the backlog's passes while it's open, as `boxctl backlog
//     dispatch` does.
//
// An app is only which views it has (the : prompt's, its home tiles, its
// one-key switches), what it lists at start, and whether it runs the
// backlog; every key and listing in a view is the same in either. A run's
// box is still reachable from the board (`s`/`l` in :runs), since the
// boxes are listed in both.
type tuiApp struct {
	name     string       // the command, for messages: "tui", "board"
	views    []*tableView // the : prompt's views, in tab completion's order
	tiles    []homeTile
	screens  []screenKey
	listings []string // listed at start for the tiles' counts, besides the boxes
	backlog  bool     // loads the backlog and runs its passes
	creates  bool     // c on home and :create make a box
}

// screenKey switches to a view with one keystroke from home or any table.
type screenKey struct {
	key  string
	view *tableView
}

var boxesApp = &tuiApp{
	name:  "tui",
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
	views: []*tableView{backlogView, runsView, reposView},
	tiles: []homeTile{
		{backlogView, "Goals waiting for a run, and PRs", "goal"},
		{runsView, "Every attempt at a goal", "run"},
		{reposView, "GitHub repos to start Claude on", "repo"},
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

// commandNames are what the : prompt suggests, in the order it offers
// them: the views, then the other commands.
func (a *tuiApp) commandNames() []string {
	var names []string
	for _, v := range a.views {
		names = append(names, v.name)
	}
	names = append(names, "home")
	if a.creates {
		names = append(names, "create")
	}
	return append(names, "help", "ssh", "quit")
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

// otherApp names the app that has view name, for `:runs` in the tui:
// "boxctl board", or "" when neither app has it.
func (a *tuiApp) otherApp(name string) string {
	for _, o := range []*tuiApp{boxesApp, boardApp} {
		if o != a && o.findView(name) != nil {
			return "boxctl " + o.name
		}
	}
	return ""
}
