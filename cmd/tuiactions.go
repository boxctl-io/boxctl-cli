package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The tui's keys and the actions behind them: the `:` command prompt,
// the `/` filter, the modal dialogs, the table and text-page keys, and
// the client calls each action makes.

func (m tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	switch {
	case m.filtering:
		return m.handleFilterKey(msg)
	case m.commanding:
		return m.handleCommandKey(msg)
	case m.dialog != nil:
		return m.handleDialogKey(msg)
	case m.create != nil:
		return m.handleCreateKey(msg)
	}
	// Keys typed quickly (or held) can arrive as one message with
	// several runes; each is a keypress of its own here.
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
		var cmds []tea.Cmd
		var next tea.Model = m
		for _, r := range msg.Runes {
			var cmd tea.Cmd
			next, cmd = next.(tuiModel).handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			cmds = append(cmds, cmd)
		}
		return next, tea.Batch(cmds...)
	}
	switch m.top().kind {
	case pageText:
		return m.handleTextKey(msg)
	case pageHome:
		return m.handleHomeKey(msg)
	}
	return m.handleTableKey(msg)
}

// --- the : prompt ---------------------------------------------------------

func (m tuiModel) handleCommandKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.commanding = false
		m.command.Blur()
		return m, nil
	case tea.KeyEnter:
		m.commanding = false
		m.command.Blur()
		line := m.command.Value()
		m.command.SetValue("")
		return m.runCommand(line)
	}
	var cmd tea.Cmd
	m.command, cmd = m.command.Update(msg)
	return m, cmd
}

// runCommand is a `:` command: a view's name, q, help or create.
func (m tuiModel) runCommand(line string) (tea.Model, tea.Cmd) {
	words := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), ":"))
	if len(words) == 0 {
		return m, nil
	}
	switch words[0] {
	case "q", "quit", "exit":
		return m, tea.Quit
	case "home", "main":
		m.goHome()
		return m, nil
	case "help", "h", "?":
		m.pushHelp()
		return m, nil
	case "create", "new":
		return m.openCreate("", "")
	case "ssh", "shell":
		if len(words) < 2 {
			return m, m.fail("usage: :ssh <box>")
		}
		return m.ssh(words[1])
	}
	if v := findView(words[0]); v != nil {
		cmd := m.openView(v)
		if len(words) > 1 {
			// `:boxes web` opens the view filtered, like k9s's `:pods nginx`.
			m.filter.SetValue(strings.Join(words[1:], " "))
		}
		return m, cmd
	}
	return m, m.fail("unknown command %q (try :home, :boxes, :urls, :images, :sizes, :backups, :repos, :backlog, :runs, :help, :q)", words[0])
}

// --- the / filter ---------------------------------------------------------

func (m tuiModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.filter.SetValue("")
		m.filter.Blur()
		m.filtering = false
		return m, nil
	case tea.KeyEnter:
		m.filter.Blur()
		m.filtering = false
		return m, nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	return m, cmd
}

// --- dialogs --------------------------------------------------------------

func (m tuiModel) handleDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	d := m.dialog
	if d.area != nil {
		switch msg.Type {
		case tea.KeyEsc:
			m.dialog = nil
			return m, nil
		case tea.KeyEnter:
			m.dialog = nil
			return d.submit(m, strings.TrimSpace(d.area.Value()))
		}
		d.area.SetWidth(m.dialogAreaWidth())
		var cmd tea.Cmd
		*d.area, cmd = d.area.Update(msg)
		return m, cmd
	}
	if d.input != nil {
		switch msg.Type {
		case tea.KeyEsc:
			m.dialog = nil
			return m, nil
		case tea.KeyEnter:
			m.dialog = nil
			return d.submit(m, d.value(&m))
		}
		if d.choices != nil {
			matches := d.matches(&m)
			switch msg.String() {
			case "up", "ctrl+p", "shift+tab":
				if len(matches) > 0 {
					d.sel = (d.sel - 1 + len(matches)) % len(matches)
					d.picked = true
				}
				return m, nil
			case "down", "ctrl+n":
				if len(matches) > 0 {
					d.sel = (d.sel + 1) % len(matches)
					d.picked = true
				}
				return m, nil
			case "tab":
				if len(matches) > 0 {
					d.input.SetValue(matches[min(d.sel, len(matches)-1)])
					d.input.CursorEnd()
					d.sel, d.picked = 0, false
				}
				return m, nil
			}
		}
		before := d.input.Value()
		var cmd tea.Cmd
		*d.input, cmd = d.input.Update(msg)
		if d.input.Value() != before {
			d.sel, d.picked = 0, false
		}
		return m, cmd
	}
	m.dialog = nil
	if s := msg.String(); s == "y" || s == "Y" || msg.Type == tea.KeyEnter {
		return d.yes(m)
	}
	return m, nil
}

// matches are the dialog's choices that fit the text typed so far, best
// first: those it starts, then those whose name after the owner it
// starts, then those containing it anywhere (case aside). No text
// matches them all.
func (d *tuiDialog) matches(m *tuiModel) []string {
	if d.choices == nil {
		return nil
	}
	all := d.choices(m)
	q := strings.ToLower(strings.TrimSpace(d.input.Value()))
	if q == "" {
		return all
	}
	var first, second, third []string
	for _, c := range all {
		l := strings.ToLower(c)
		_, name, _ := strings.Cut(l, "/")
		switch {
		case strings.HasPrefix(l, q):
			first = append(first, c)
		case strings.HasPrefix(name, q):
			second = append(second, c)
		case strings.Contains(l, q):
			third = append(third, c)
		}
	}
	return append(append(first, second...), third...)
}

// value is what enter submits: the selected choice when up/down picked
// one, or when the text alone isn't a choice and can't be one (no
// owner/name) but matches some; else the text.
func (d *tuiDialog) value(m *tuiModel) string {
	v := strings.TrimSpace(d.input.Value())
	if d.choices == nil {
		return v
	}
	matches := d.matches(m)
	if len(matches) == 0 {
		return v
	}
	sel := matches[min(d.sel, len(matches)-1)]
	if d.picked {
		return sel
	}
	if _, _, err := parseRepo(v); err != nil || v == "" {
		return sel
	}
	return v
}

// ask opens a y/N question.
func (m *tuiModel) ask(title string, body []string, yes func(m tuiModel) (tea.Model, tea.Cmd)) {
	m.dialog = &tuiDialog{title: title, body: body, yes: yes}
}

// prompt opens a one-line input.
func (m *tuiModel) prompt(title string, body []string, placeholder string, submit func(m tuiModel, value string) (tea.Model, tea.Cmd)) tea.Cmd {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = placeholder
	in.CharLimit = 80
	m.dialog = &tuiDialog{title: title, body: body, input: &in, submit: submit}
	return in.Focus()
}

// promptText opens a prompt for a sentence or two (a goal): unlike
// prompt's one line, what's typed wraps onto the next line inside the
// dialog instead of running off its right edge. Enter still submits.
func (m *tuiModel) promptText(title string, body []string, placeholder string, submit func(m tuiModel, value string) (tea.Model, tea.Cmd)) tea.Cmd {
	a := textarea.New()
	a.ShowLineNumbers = false
	a.CharLimit = 0
	a.Placeholder = placeholder
	a.KeyMap.InsertNewline.SetEnabled(false) // enter submits
	a.FocusedStyle.CursorLine = lipgloss.NewStyle()
	a.FocusedStyle.Base = lipgloss.NewStyle()
	a.BlurredStyle.Base = lipgloss.NewStyle()
	a.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "> "
		}
		return "  "
	})
	a.SetHeight(goalPromptLines)
	m.dialog = &tuiDialog{title: title, body: body, area: &a, submit: submit}
	a.SetWidth(m.dialogAreaWidth())
	return a.Focus()
}

// goalPromptLines is how many lines promptText's input shows; a longer
// goal scrolls inside it.
const goalPromptLines = 4

// --- table keys -----------------------------------------------------------

func (m tuiModel) handleTableKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := m.top()
	key := msg.String()
	if v := screenFor(key); v != nil {
		return m, m.openView(v)
	}
	if t.view == backlogView {
		if md, cmd, ok := m.handleBacklogKey(key); ok {
			return md, cmd
		}
	}
	if t.view == runsView {
		if md, cmd, ok := m.handleRunsKey(key); ok {
			return md, cmd
		}
	}
	if t.view == reposView && key == "a" {
		if r := m.currentRow(); r != nil {
			return m.askBacklogGoal(r.key)
		}
		return m, nil
	}
	switch key {
	case "up", "k":
		m.move(-1)
		return m, nil
	case "down", "j":
		m.move(1)
		return m, nil
	case "pgup", "ctrl+b":
		m.move(-m.tableHeight())
		return m, nil
	case "pgdown", "ctrl+f":
		m.move(m.tableHeight())
		return m, nil
	case "home":
		m.moveTo(false)
		return m, nil
	case "end", "G":
		m.moveTo(true)
		return m, nil
	case ":":
		m.commanding = true
		m.command.SetValue("")
		return m, m.command.Focus()
	case "/":
		m.filtering = true
		return m, m.filter.Focus()
	case "?":
		m.pushHelp()
		return m, nil
	case "esc":
		switch {
		case m.filter.Value() != "":
			m.filter.SetValue("")
		case len(m.pages) > 1:
			m.pop()
		}
		return m, nil
	case "q":
		// q only goes back: quitting is :q (or ctrl-c), so a stray q on
		// the top view doesn't close the tui.
		if len(m.pages) > 1 {
			m.pop()
			return m, nil
		}
		return m, m.say("type :q to quit")
	case "R", "ctrl+r", "f5":
		if t.view == reposView {
			return m, tea.Batch(m.refresh(), m.listRepos(true), m.spinIfBusy())
		}
		return m, m.refresh()
	case "0", "1", "2":
		if t.view == boxesView {
			m.stateFilter = stateFilter(key[0] - '0')
		}
		return m, nil
	case "c":
		switch t.view {
		case imagesView:
			if r := m.currentRow(); r != nil {
				return m.openCreate(r.key, "")
			}
		case sizesView:
			if r := m.currentRow(); r != nil {
				return m.openCreate("", r.key)
			}
		}
		return m.openCreate("", "")
	case "enter":
		if t.view == reposView {
			if r := m.currentRow(); r != nil {
				return m.claudeRepo(r.key, "")
			}
			return m, nil
		}
		return m.describe()
	case "g":
		if t.view == reposView {
			if r := m.currentRow(); r != nil {
				return m.askGoal(r.key)
			}
			return m, nil
		}
		m.moveTo(false)
		return m, nil
	case "d":
		return m.describe()
	case "y":
		return m.showJSON()
	case "ctrl+d":
		if t.view == urlsView {
			return m.askUnexpose()
		}
		return m.askDestroy()
	case "r":
		if t.view == backupsView {
			return m.askRestore()
		}
	}
	// shift+letter sorts by the column with that letter.
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		for i, col := range t.view.columns {
			if col.sort == rune(key[0]) {
				if t.sortCol == i {
					t.sortDesc = !t.sortDesc
				} else {
					t.sortCol, t.sortDesc = i, false
				}
				return m, nil
			}
		}
	}
	return m.handleBoxKey(msg)
}

// handleBoxKey is the keys that act on the page's box: the row's in the
// boxes/urls views, or the one a describe page is about.
func (m tuiModel) handleBoxKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	vm := m.currentBox()
	if vm == nil {
		return m, nil
	}
	switch msg.String() {
	case "s":
		return m.ssh(vm.Name)
	case "a":
		return m.claudeSession(vm)
	case "l":
		return m.logs(vm.Name)
	case "p":
		return m.act(vm, "pause", "running", func(ctx context.Context) (string, error) {
			_, err := m.c.Pause(ctx, vm.Name)
			return "", err
		})
	case "r":
		return m.act(vm, "resume", "paused", func(ctx context.Context) (string, error) {
			_, err := m.c.Resume(ctx, vm.Name)
			return "", err
		})
	case "K":
		return m.act(vm, "keep", "", func(ctx context.Context) (string, error) {
			kept, err := m.c.Keep(ctx, vm.Name)
			if err != nil {
				return "", err
			}
			if kept.KeepUntil == nil {
				return "", nil
			}
			return fmt.Sprintf("%s is kept until %s", vm.Name, stamp(*kept.KeepUntil)), nil
		})
	case "e":
		return m.askExpose(vm)
	}
	return m, nil
}

// --- text pages -----------------------------------------------------------

func (m tuiModel) handleTextKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := m.top()
	h := m.tableHeight()
	n := len(m.textLines())
	switch msg.String() {
	case "up", "k":
		t.scroll = max(t.scroll-1, 0)
	case "down", "j":
		t.scroll = min(t.scroll+1, max(n-h, 0))
	case "pgup", "ctrl+b", "b":
		t.scroll = max(t.scroll-h, 0)
	case "pgdown", "ctrl+f", "f", " ":
		t.scroll = min(t.scroll+h, max(n-h, 0))
	case "home", "g":
		t.scroll = 0
	case "end", "G":
		t.scroll = max(n-h, 0)
	case "esc", "q":
		m.pop()
	case ":":
		m.commanding = true
		m.command.SetValue("")
		return m, m.command.Focus()
	case "?":
		m.pushHelp()
	case "y":
		return m.showJSON()
	case "d", "enter":
		return m.describe()
	case "ctrl+d":
		return m.askDestroy()
	default:
		return m.handleBoxKey(msg)
	}
	return m, nil
}

// pushHelp shows the keys: the view's hotkeys, the common ones, and the
// sort letters.
func (m *tuiModel) pushHelp() {
	var v *tableView // nil on home: the general keys only
	for i := len(m.pages) - 1; i >= 0; i-- {
		if m.pages[i].kind == pageTable {
			v = m.pages[i].view
			break
		}
	}
	lines := []string{"General", ""}
	general := [][2]string{
		{":<view>", "switch view: boxes, urls, images, sizes, backups, repos, backlog, runs (`:boxes web` filters too; tab completes)"},
		{"b/t/u/h", "switch to :boxes, :backlog, :runs, :repos with one key (in a table)"},
		{":home", "the home page, a tile per view (esc from a view goes there too)"},
		{":create", "create a box"}, {":ssh <box>", "open a terminal on a box"},
		{":help", "this page"}, {":q", "quit (ctrl-c too)"},
		{"/text", "filter the rows; esc clears"},
		{"j/k ↑/↓", "move; ctrl-f/ctrl-b page; g/G top/bottom"},
		{"shift+letter", "sort by that column (again: the other way)"},
		{"esc", "back to the previous page (from a view: home)"}, {"R", "refresh now (also every " + tuiRefreshEvery.String() + ")"},
	}
	for _, g := range general {
		lines = append(lines, fmt.Sprintf("  %-14s %s", g[0], g[1]))
	}
	if v == nil {
		lines = append(lines, "", "Home", "",
			"  ←↑↓→ (j/k, tab) move between tiles; enter opens the view; c creates a box.")
		m.push(page{kind: pageText, title: "help", lines: lines})
		return
	}
	lines = append(lines, "", v.title, "")
	for _, h := range v.hotkeys {
		lines = append(lines, fmt.Sprintf("  %-14s %s", h.key, h.label))
	}
	var sorts []string
	for _, col := range v.columns {
		if col.sort != 0 {
			sorts = append(sorts, fmt.Sprintf("shift-%c %s", col.sort+'a'-'A', col.title))
		}
	}
	if len(sorts) > 0 {
		lines = append(lines, "", "  sort:          "+strings.Join(sorts, ", "))
	}
	if v == backupsView {
		lines = append(lines, "", "Backups",
			"  r asks for a name (the backed-up box's by default) and restores the backup as a new box,",
			"  paused at its own size (`boxctl restore`); a size after the name boots it fresh at that size.")
	}
	if v == reposView {
		lines = append(lines, "", "GitHub Repos",
			"  The repositories gh's account can reach (gh api user/repos), newest push first;",
			"  enter runs `boxctl claude --repo owner/name`: a fresh box with the repo cloned on a new branch,",
			"  Claude attached, and the box deleted when Claude quits once its work is pushed.",
			"  g asks for a goal and runs `boxctl claude --repo owner/name --goal ...`: Claude works",
			"  unattended on a new box and opens a pull request; :boxes' RUN column follows it (home is top here).",
			"  The list is cached (~/.boxctl/repos.json) and asked of GitHub again when an hour old, or with R.")
	}
	if v == boxesView {
		lines = append(lines, "", "Boxes",
			"  s shell runs `boxctl ssh <box>`; l logs is `boxctl claude logs` (a --goal run's log);",
			"  ctrl-d asks first, and again for a --goal run that isn't done;",
			"  e asks for a port and exposes it at a public URL (`boxctl expose`); :urls lists them.")
	}
	m.push(page{kind: pageText, title: "help", lines: lines})
}

// --- actions --------------------------------------------------------------

// describe pushes the page's box or backup as a describe page.
func (m tuiModel) describe() (tea.Model, tea.Cmd) {
	t := m.top()
	if t.kind == pageTable && t.view == reposView {
		if r := m.currentRow(); r != nil {
			if repo := m.repo(r.key); repo != nil {
				m.push(page{kind: pageText, title: repo.FullName, lines: describeRepo(*repo)})
			}
		}
		return m, nil
	}
	if t.kind == pageTable && t.view == backupsView {
		r := m.currentRow()
		if r == nil {
			return m, nil
		}
		for _, b := range m.backups {
			if b.ID == r.key {
				m.push(page{kind: pageText, title: b.ID, lines: describeBackup(b)})
				break
			}
		}
		return m, nil
	}
	vm := m.currentBox()
	if vm == nil {
		return m, nil
	}
	if t.kind == pageText && t.box == vm.Name && t.title == vm.Name {
		return m, nil // already there
	}
	name := vm.Name
	m.push(page{kind: pageText, title: name, box: name, lines: describeBox(*vm),
		refresh: func(m *tuiModel) []string {
			if vm := m.box(name); vm != nil {
				return describeBox(*vm)
			}
			return nil
		}})
	return m, nil
}

// showJSON pushes the page's box as the API returns it.
func (m tuiModel) showJSON() (tea.Model, tea.Cmd) {
	t := m.top()
	if t.kind == pageTable && t.view == reposView {
		if r := m.currentRow(); r != nil {
			if repo := m.repo(r.key); repo != nil {
				m.push(page{kind: pageText, title: repo.FullName + ".json", lines: jsonLines(*repo)})
			}
		}
		return m, nil
	}
	if t.kind == pageTable && t.view == backupsView {
		if r := m.currentRow(); r != nil {
			for _, b := range m.backups {
				if b.ID == r.key {
					m.push(page{kind: pageText, title: b.ID + ".json", lines: jsonLines(b)})
				}
			}
		}
		return m, nil
	}
	vm := m.currentBox()
	if vm == nil {
		return m, nil
	}
	name := vm.Name
	m.push(page{kind: pageText, title: name + ".json", box: name, lines: jsonLines(*vm),
		refresh: func(m *tuiModel) []string {
			if vm := m.box(name); vm != nil {
				return jsonLines(*vm)
			}
			return nil
		}})
	return m, nil
}

func jsonLines(v any) []string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []string{err.Error()}
	}
	return strings.Split(string(data), "\n")
}

// act runs one of the client's per-box calls on vm in the background,
// with the box marked pending meanwhile. wantState, when not empty, is
// the state the box must be in (pause needs running), so a
// sure-to-fail request isn't sent. fn's string, if any, is what to
// flash on success.
func (m tuiModel) act(vm *client.VM, verb, wantState string, fn func(ctx context.Context) (string, error)) (tea.Model, tea.Cmd) {
	if v, ok := m.pending[vm.Name]; ok {
		return m, m.fail("%s is being %s", vm.Name, pastTense(v))
	}
	if wantState != "" && vm.State != wantState {
		return m, m.fail("%s is %s, not %s; can't %s it", vm.Name, vm.State, wantState, verb)
	}
	name := vm.Name
	m.pending[name] = verb
	run := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		result, err := fn(ctx)
		return tuiActionMsg{name: name, verb: verb, err: err, result: result}
	}
	return m, tea.Batch(run, m.say("%s %s…", presentTense(verb), name), m.spinIfBusy())
}

// askDestroy is rm's two confirmations in one dialog: the usual
// question, with the unfinished-goal-run warning when it applies (from
// the list already loaded, as rm reads the one List call).
func (m tuiModel) askDestroy() (tea.Model, tea.Cmd) {
	vm := m.currentBox()
	if vm == nil {
		return m, nil
	}
	if v, ok := m.pending[vm.Name]; ok {
		return m, m.fail("%s is being %s", vm.Name, pastTense(v))
	}
	body := []string{"This cannot be undone."}
	if len(filterUnfinished([]client.VM{*vm}, nil)) > 0 {
		body = append(body, "", fmt.Sprintf("%s is not done (%s):", vm.Name, runText(*vm)), "its work may not be fetched yet.")
	}
	m.ask(fmt.Sprintf("Delete %s?", vm.Name), body, func(m tuiModel) (tea.Model, tea.Cmd) {
		vm := m.box(vm.Name)
		if vm == nil {
			return m, nil
		}
		return m.act(vm, "destroy", "", func(ctx context.Context) (string, error) {
			return "", m.c.Destroy(ctx, vm.Name)
		})
	})
	return m, nil
}

// askExpose asks for a port and gives it a public URL.
func (m tuiModel) askExpose(vm *client.VM) (tea.Model, tea.Cmd) {
	name := vm.Name
	cmd := m.prompt("Expose a port on "+name, []string{"The port gets a public https:// URL (boxctl expose)."}, "port, e.g. 3000",
		func(m tuiModel, value string) (tea.Model, tea.Cmd) {
			port, err := strconv.Atoi(value)
			if err != nil || port < 1 || port > 65535 {
				return m, m.fail("%q is not a port", value)
			}
			vm := m.box(name)
			if vm == nil {
				return m, m.fail("%s is gone", name)
			}
			return m.act(vm, "expose", "", func(ctx context.Context) (string, error) {
				in, err := m.c.Expose(ctx, name, port, client.ExposeOptions{})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%s → %s port %d (%s)", in.URL, name, port, in.Status), nil
			})
		})
	return m, cmd
}

// askRestore is `boxctl restore <backup> [-n name] [--size]` for the
// backups view's row: a prompt for the new box's name, the backed-up
// box's by default, with an optional size after it.
func (m tuiModel) askRestore() (tea.Model, tea.Cmd) {
	r := m.currentRow()
	if r == nil {
		return m, nil
	}
	var b *client.Backup
	for i := range m.backups {
		if m.backups[i].ID == r.key {
			b = &m.backups[i]
		}
	}
	if b == nil {
		return m, nil
	}
	backup := *b
	cmd := m.prompt("Restore "+backup.ID, []string{
		fmt.Sprintf("%s's backup from %s ago, %s.", backup.VMName, age(backup.CreatedAt), client.FormatBytes(backup.SizeBytes)),
		"",
		"Name for the new box; it comes back paused at its own size.",
		"A size after the name (`" + backup.VMName + " large`) boots it fresh at that size instead.",
	}, "name [size]", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		words := strings.Fields(value)
		if len(words) == 0 || len(words) > 2 {
			return m, m.fail("want a name, optionally followed by a size")
		}
		name, size := words[0], ""
		if len(words) == 2 {
			size = words[1]
		}
		if m.box(name) != nil {
			return m, m.fail("a box named %s already exists", name)
		}
		if v, ok := m.pending[name]; ok {
			return m, m.fail("%s is being %s", name, pastTense(v))
		}
		m.pending[name] = "restore"
		c := m.c
		run := func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			vm, notice, err := c.Restore(ctx, backup.ID, name, size)
			if err != nil {
				return tuiActionMsg{name: name, verb: "restore", err: err}
			}
			result := fmt.Sprintf("restored %s (%s, %s)", vm.Name, vm.State, sizeLabel(*vm))
			if vm.State == "paused" {
				result += " -- r in :boxes resumes it"
			}
			if t := notice.Text(); t != "" {
				result += " -- " + t
			}
			return tuiActionMsg{name: name, verb: "restore", result: result}
		}
		return m, tea.Batch(run, m.say("restoring %s's backup as %s…", backup.VMName, name), m.spinIfBusy())
	})
	if m.dialog != nil && m.dialog.input != nil {
		m.dialog.input.SetValue(backup.VMName)
		m.dialog.input.CursorEnd()
	}
	return m, cmd
}

// askUnexpose takes the urls view's row off the internet, after asking.
func (m tuiModel) askUnexpose() (tea.Model, tea.Cmd) {
	r := m.currentRow()
	if r == nil {
		return m, nil
	}
	name, portText, _ := strings.Cut(r.key, ":")
	port, _ := strconv.Atoi(portText)
	m.ask(fmt.Sprintf("Unexpose %s port %d?", name, port), []string{"Its URL is retired; exposing the port again gives a new one."},
		func(m tuiModel) (tea.Model, tea.Cmd) {
			vm := m.box(name)
			if vm == nil {
				return m, nil
			}
			return m.act(vm, "unexpose", "", func(ctx context.Context) (string, error) {
				return fmt.Sprintf("unexposed %s port %d", name, port), m.c.Unexpose(ctx, name, port)
			})
		})
	return m, nil
}

// ssh hands the terminal to `boxctl ssh <name>` -- a separate process,
// so its stdin reader (which runSSH can't cancel) dies with it instead
// of swallowing this program's next keypress. Its stderr is kept to show
// why it failed, since the alternate screen hides whatever it printed.
func (m tuiModel) ssh(name string) (tea.Model, tea.Cmd) {
	if vm := m.box(name); vm != nil && vm.State == "paused" {
		return m, m.fail("%s is paused; press r to resume it first", name)
	}
	exe, err := os.Executable()
	if err != nil {
		return m, m.fail("finding boxctl: %v", err)
	}
	args := []string{"ssh", name}
	if apiURLFlag != "" {
		args = append(args, "--api-url", apiURLFlag)
	}
	cmd := exec.Command(exe, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return tuiSSHDoneMsg{name: name, err: err, stderr: stderr.String()}
	})
}

// logs fetches a --goal run's log off the box (what `boxctl claude
// logs` prints: the same script, over Exec instead of ssh, through the
// same formatter) into a text page.
func (m tuiModel) logs(name string) (tea.Model, tea.Cmd) {
	key := "logs:" + name
	if _, ok := m.pending[key]; ok {
		return m, nil
	}
	if vm := m.box(name); vm != nil && vm.State != "running" {
		return m, m.fail("%s is %s; its log can be read once it's running", name, vm.State)
	}
	m.pending[key] = "logs"
	c := m.c
	fetch := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		msg := tuiTextMsg{title: name + ".log", box: name}
		res, err := c.Exec(ctx, name, logsScript(false), time.Minute)
		if err != nil {
			msg.err = err
			return msg
		}
		if res.Error != "" {
			msg.err = errors.New(res.Error)
			return msg
		}
		if res.ExitCode != 0 {
			msg.err = errors.New(strings.TrimSpace(res.Stderr))
			if msg.err.Error() == "" {
				msg.err = fmt.Errorf("exit status %d", res.ExitCode)
			}
			return msg
		}
		var buf bytes.Buffer
		fm := &logFormatter{w: &buf}
		_, _ = fm.Write([]byte(res.Stdout))
		_ = fm.Flush()
		msg.lines = strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		if len(msg.lines) == 1 && msg.lines[0] == "" {
			msg.lines = []string{"(empty log)"}
		}
		return msg
	}
	return m, tea.Batch(fetch, m.say("reading %s's log…", name), m.spinIfBusy())
}

// claudeSessionArgs is the command that opens the box's Claude session
// -- `boxctl claude --box <name>`, which attaches to the session running
// there or starts one in the project the box holds -- or an error saying
// why this box has none to open. A box holding a local project (its
// boxctl.project label) is opened with --project, so it works from
// whatever directory the tui runs in; a --repo/--goal box needs none,
// its clone exists only on the box. A goal run's Claude ran with
// claude -p, so once that's over, --continue picks its conversation up
// to ask for changes.
func claudeSessionArgs(vm *client.VM) ([]string, error) {
	run := vm.Labels["boxctl.run"]
	if run == "" {
		return nil, fmt.Errorf("%s wasn't made by boxctl claude, so it has no Claude session; s opens a shell", vm.Name)
	}
	if a := vm.Labels["boxctl.agent"]; a != "" && a != "claude" {
		return nil, fmt.Errorf("%s runs %s, not Claude", vm.Name, a)
	}
	args := []string{"claude", "--box", vm.Name}
	if p := vm.Labels["boxctl.project"]; p != "" {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("%s holds %s, which isn't on this machine; run boxctl claude --box %s where it is", vm.Name, p, vm.Name)
		}
		args = append(args, "--project", p)
	}
	if apiURLFlag != "" {
		args = append(args, "--api-url", apiURLFlag)
	}
	if run == "goal" {
		args = append(args, "--", "--continue")
	}
	return args, nil
}

// claudeSession hands the terminal to the box's Claude session (see
// claudeSessionArgs), a child process as ssh is. Detaching (or Claude
// quitting) comes back here.
func (m tuiModel) claudeSession(vm *client.VM) (tea.Model, tea.Cmd) {
	args, err := claudeSessionArgs(vm)
	if err != nil {
		return m, m.fail("%v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return m, m.fail("finding boxctl: %v", err)
	}
	cmd := exec.Command(exe, args...)
	errTail := &tailBuffer{max: 4096}
	cmd.Stderr = io.MultiWriter(os.Stderr, errTail)
	name := vm.Name
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return tuiClaudeDoneMsg{box: name, err: err, stderr: errTail.String()}
	})
}
