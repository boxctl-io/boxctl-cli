package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
	tea "github.com/charmbracelet/bubbletea"
)

// `:backlog`: boxctl backlog in boxctl board. The rows are
// ~/.boxctl/backlog.json, read again on every refresh; the passes that move
// it (backlogdispatch.go) run every backlogPassEvery while the board is
// open, whatever view is shown -- the board is the backlog's loop unless
// `boxctl backlog dispatch` already is. boxctl tui never runs them.

type (
	tuiBacklogPassMsg struct {
		events []backlogEvent
		err    error
	}
	tuiBacklogDoneMsg struct { // a key's change to the backlog was made
		text string
		err  error
	}
)

var backlogView = &tableView{
	name:     "backlog",
	title:    "Backlog",
	resource: "backlog",
	columns:  []column{{"ID", 'I'}, {"PRI", 'P'}, {"REPO", 'R'}, {"STATE", 'S'}, {"RUN", 0}, {"PR", 0}, {"NOTE", 'N'}, {"GOAL", 'G'}},
	stateCol: 3,
	empty:    "Nothing queued: a queues a goal (here, or on a repository in :repos).",
	hotkeys: []hotkey{{"a", "add a goal"}, {"c", "request changes"}, {"m", "merge"}, {"r", "retry"}, {"+/-", "priority"}, {"L", "slot limit"}, {"o", "open PR"},
		{"l", "run's log"}, {"s", "run's shell"}, {"ctrl-d", "cancel"}, {"enter", "runs"}, {"d", "describe"}},
	rows: func(m *tuiModel) []tableRow {
		if m.backlog == nil {
			return nil
		}
		var rows []tableRow
		for i, it := range m.backlog.Sorted() {
			rows = append(rows, tableRow{
				key:      strconv.Itoa(it.ID),
				state:    backlogStateText(it.State),
				cells:    []string{strconv.Itoa(it.ID), strconv.Itoa(it.Priority), it.Repo, backlogStateText(it.State), backlogRunText(it), backlogPRText(it), orDash(it.Note), firstLine(it.Goal)},
				sortKeys: []string{fmt.Sprintf("%08d", it.ID), fmt.Sprintf("%08d", 1<<20-it.Priority), "", fmt.Sprintf("%04d", i)},
			})
		}
		return rows
	},
}

// backlogStateText is the STATE cell, which the table colours: "needs you"
// as an error, done as finished.
func backlogStateText(s backlog.State) string {
	return strings.ReplaceAll(string(s), "_", " ")
}

// loadBacklogMsg reads the backlog file for the view.
func loadBacklogMsg() tea.Msg {
	q, err := loadBacklog()
	return tuiListMsg{resource: "backlog", backlog: q, err: err}
}

// setBacklogNote shows note on item id in the table at once, while the
// change it describes is still being made; the reload that follows
// replaces it with what the backlog file says.
func (m *tuiModel) setBacklogNote(id int, note string) {
	if m.backlog == nil {
		return
	}
	if it := m.backlog.Item(id); it != nil {
		it.Note = note
	}
}

// backlogPass runs one pass in the background, unless the tui has no runner
// (tests). With one going already, another follows it as soon as it ends:
// the running one may have read GitHub before the change that asked.
func (m *tuiModel) backlogPass() tea.Cmd {
	if m.qr == nil {
		return nil
	}
	if m.backlogPassing {
		m.backlogPassAgain = true
		return nil
	}
	m.backlogPassing = true
	r := m.qr
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		events, err := r.pass(ctx)
		return tuiBacklogPassMsg{events: events, err: err}
	}
}

// backlogPassed shows what a pass did (its last event, or its error) and
// reloads the backlog.
func (m *tuiModel) backlogPassed(msg tuiBacklogPassMsg) tea.Cmd {
	m.backlogPassing = false
	m.backlogPassAt = time.Now()
	var held errLoopHeld
	m.backlogHeld = ""
	cmds := []tea.Cmd{loadBacklogMsg}
	if m.backlogPassAgain {
		m.backlogPassAgain = false
		cmds = append(cmds, m.backlogPass())
	}
	switch {
	case errors.As(msg.err, &held):
		m.backlogHeld = held.holder
	case msg.err != nil:
		cmds = append(cmds, m.fail("backlog: %v", msg.err))
	case len(msg.events) > 0:
		e := msg.events[len(msg.events)-1]
		if e.err {
			cmds = append(cmds, m.fail("backlog %s", e.text))
		} else {
			cmds = append(cmds, m.say("backlog %s", e.text))
		}
	}
	return tea.Batch(cmds...)
}

// backlogItem is the item under the cursor in :backlog.
func (m *tuiModel) backlogItem() *backlog.Item {
	if m.backlog == nil || m.top().view != backlogView {
		return nil
	}
	r := m.currentRow()
	if r == nil {
		return nil
	}
	id, _ := strconv.Atoi(r.key)
	return m.backlog.Item(id)
}

// backlogChange runs fn on the backlog in the background, then flashes text
// and reloads it -- and runs a pass, so the change takes effect now.
func (m *tuiModel) backlogChange(text string, fn func() error) tea.Cmd {
	return func() tea.Msg {
		if err := fn(); err != nil {
			return tuiBacklogDoneMsg{err: err}
		}
		return tuiBacklogDoneMsg{text: text}
	}
}

// handleBacklogKey is :backlog's own keys; handled says whether key was one.
func (m tuiModel) handleBacklogKey(key string) (tea.Model, tea.Cmd, bool) {
	it := m.backlogItem()
	switch key {
	case "a":
		md, cmd := m.askBacklogRepo()
		return md, cmd, true
	case "L":
		md, cmd := m.askBacklogLimit()
		return md, cmd, true
	case "enter":
		if it != nil {
			m.openRuns(it.ID)
		}
		return m, nil, true
	case "d":
		if it != nil {
			m.push(page{kind: pageText, title: fmt.Sprintf("#%d", it.ID), lines: describeBacklogItem(it)})
		}
		return m, nil, true
	case "y":
		if it != nil {
			m.push(page{kind: pageText, title: fmt.Sprintf("#%d.json", it.ID), lines: jsonLines(it)})
		}
		return m, nil, true
	}
	if it == nil {
		return m, nil, key == "c" || key == "m" || key == "r" || key == "+" || key == "-" || key == "o" || key == "l" || key == "s" || key == "ctrl+d"
	}
	id := it.ID
	switch key {
	case "m":
		if it.ReadySHA == "" {
			return m, m.fail("#%d isn't ready to merge (%s)", id, orDash(it.Note)), true
		}
		item := *it
		short := item.ReadySHA
		if len(short) > 7 {
			short = short[:7]
		}
		m.ask(fmt.Sprintf("Merge %s#%d?", item.Repo, item.PR), []string{
			fmt.Sprintf("Squash-merges it at %s, the commit CI passed on; GitHub refuses if the branch moved since.", short),
		}, func(m tuiModel) (tea.Model, tea.Cmd) {
			m.setBacklogNote(id, "merging")
			return m, tea.Batch(m.say("merging %s#%d...", item.Repo, item.PR), m.backlogChange(fmt.Sprintf("merged %s#%d", item.Repo, item.PR), func() error {
				ctx, cancel := backlogCtx()
				defer cancel()
				if err := mergeBacklogItem(ctx, &item, "squash"); err != nil {
					return err
				}
				return markBacklogMerged(id)
			}))
		})
		return m, nil, true
	case "c":
		if it.PR == 0 {
			return m, m.fail("#%d has no pull request yet", id), true
		}
		if it.State == backlog.Done || it.State == backlog.Cancelled {
			return m, m.fail("#%d is %s", id, backlogStateText(it.State)), true
		}
		md, cmd := m.askBacklogChange(it)
		return md, cmd, true
	case "r":
		return m, m.backlogChange(fmt.Sprintf("#%d is active again", id), func() error { return retryBacklogItem(id) }), true
	case "+", "-":
		d := 1
		if key == "-" {
			d = -1
		}
		return m, m.backlogChange(fmt.Sprintf("#%d's priority changed", id), func() error {
			return changeBacklogItem(id, func(it *backlog.Item) error { it.Priority += d; return nil })
		}), true
	case "ctrl+d":
		m.ask(fmt.Sprintf("Cancel #%d?", id), []string{
			"Its boxes are deleted; its branch and pull request, if any, stay on GitHub.",
		}, func(m tuiModel) (tea.Model, tea.Cmd) {
			return m, m.backlogChange(fmt.Sprintf("cancelled #%d", id), func() error { return cancelBacklogItem(id) })
		})
		return m, nil, true
	case "o":
		if it.PR == 0 {
			return m, m.fail("#%d has no pull request yet", id), true
		}
		repo, pr := it.Repo, it.PR
		return m, func() tea.Msg {
			err := openPR(repo, pr)
			if err != nil {
				return tuiBacklogDoneMsg{err: fmt.Errorf("opening %s#%d: %v", repo, pr, err)}
			}
			return nil
		}, true
	case "l", "s":
		r := it.Current()
		if r == nil {
			r = it.Last()
		}
		if r == nil || m.box(r.Box) == nil {
			return m, m.fail("#%d has no box now", id), true
		}
		if key == "l" {
			md, cmd := m.logs(r.Box)
			return md, cmd, true
		}
		md, cmd := m.ssh(r.Box)
		return md, cmd, true
	}
	return m, nil, false
}

// askBacklogRepo asks which repository a new goal is for -- from a list
// that starts at the current item's, so enter alone takes that -- then
// the goal.
func (m tuiModel) askBacklogRepo() (tea.Model, tea.Cmd) {
	cmd := m.prompt("Add a goal: which repository?", []string{
		"owner/name; the goal is asked next. Type to narrow the list,",
		"up/down to select, tab to complete.",
	}, "owner/name", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		owner, name, err := parseRepo(value)
		if err != nil {
			return m, m.fail("%v", err)
		}
		return m.askBacklogGoal(owner + "/" + name)
	})
	if m.dialog != nil && m.dialog.input != nil {
		m.dialog.choices = backlogRepoChoices
	}
	// The repositories :repos lists: the cache at once, GitHub when it's
	// stale -- the list fills in when that answers.
	return m, tea.Batch(cmd, m.listRepos(false), m.spinIfBusy())
}

// backlogRepoChoices are the repositories the backlog's `a` offers: the
// current item's, the others already queued (newest item first), then
// the ones :repos lists.
func backlogRepoChoices(m *tuiModel) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		if r != "" && !seen[strings.ToLower(r)] {
			seen[strings.ToLower(r)] = true
			out = append(out, r)
		}
	}
	if it := m.backlogItem(); it != nil {
		add(it.Repo)
	}
	if m.backlog != nil {
		for i := len(m.backlog.Items) - 1; i >= 0; i-- {
			add(m.backlog.Items[i].Repo)
		}
	}
	for _, r := range m.repos {
		add(r.FullName)
	}
	return out
}

// askBacklogLimit asks for the backlog's slot limit: how many runs go at
// once in all, and optionally on one repository ("3", or "3 2").
func (m tuiModel) askBacklogLimit() (tea.Model, tea.Cmd) {
	cur := backlog.DefaultLimits
	if m.backlog != nil {
		cur = m.backlog.Limits
	}
	cmd := m.prompt("Slot limit", []string{
		fmt.Sprintf("Now %d runs at once, %d per repository. Each run is a box of yours.", cur.Global, cur.PerRepo),
		"Type how many in all, optionally then how many per repository: 3, or 3 2.",
	}, fmt.Sprintf("%d %d", cur.Global, cur.PerRepo), func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		global, perRepo, err := parseBacklogLimit(value)
		if err != nil {
			return m, m.fail("%v", err)
		}
		return m, func() tea.Msg {
			l, err := setBacklogLimits(&global, perRepo)
			if err != nil {
				return tuiBacklogDoneMsg{err: err}
			}
			return tuiBacklogDoneMsg{text: fmt.Sprintf("slot limit: %d at once, %d per repository", l.Global, l.PerRepo)}
		}
	})
	return m, cmd
}

// parseBacklogLimit reads L's answer: the global limit, then optionally the
// per-repository one (nil when not given), separated by a space or "/".
func parseBacklogLimit(s string) (int, *int, error) {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '/' || r == ',' || r == '\t' })
	if len(f) < 1 || len(f) > 2 {
		return 0, nil, errors.New("a slot limit is a number, or two: in all, per repository")
	}
	var n [2]int
	for i, w := range f {
		v, err := strconv.Atoi(w)
		if err != nil || v < 1 {
			return 0, nil, fmt.Errorf("%q: a slot limit is a whole number, at least 1", w)
		}
		n[i] = v
	}
	if len(f) == 1 {
		return n[0], nil, nil
	}
	return n[0], &n[1], nil
}

// askBacklogGoal asks for a goal on repo and adds it to the backlog.
func (m tuiModel) askBacklogGoal(repo string) (tea.Model, tea.Cmd) {
	cmd := m.promptText("Add a goal on "+repo, []string{
		"It runs unattended (boxctl claude --goal) when a slot is free, and ends in a pull request",
		"that :backlog then watches: CI and conflicts get follow-up runs; m merges it once it's green.",
	}, "what to do, e.g. bump Go and fix what breaks", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		if value == "" {
			return m, m.fail("a goal is needed")
		}
		return m, m.backlogChange("queued a goal on "+repo, func() error {
			_, err := addBacklogItem(repo, value, "", 0)
			return err
		})
	})
	return m, cmd
}

// askBacklogChange asks what to change on it's pull request, for a
// follow-up run on its branch.
func (m tuiModel) askBacklogChange(it *backlog.Item) (tea.Model, tea.Cmd) {
	id, pr := it.ID, it.PR
	body := []string{
		"A follow-up run makes them on the pull request's branch, pushes, and comments on it,",
		"once no other run of this item is going. Review comments on the PR are read too.",
	}
	if it.Change != "" {
		body = append(body, "Already asked, not started yet: "+firstLine(it.Change))
	}
	cmd := m.promptText(fmt.Sprintf("Request changes on %s#%d", it.Repo, pr), body,
		"what to change, e.g. rename the flag to --foo and add a test", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
			if strings.TrimSpace(value) == "" {
				return m, m.fail("say what to change")
			}
			m.setBacklogNote(id, "changes requested")
			return m, m.backlogChange(fmt.Sprintf("asked for changes on #%d", id), func() error { return requestBacklogChange(id, value) })
		})
	return m, cmd
}

// describeBacklogItem is :backlog's describe page.
func describeBacklogItem(it *backlog.Item) []string {
	kv := func(k, v string) string { return fmt.Sprintf("%-12s%s", k+":", v) }
	lines := []string{
		kv("Item", "#"+strconv.Itoa(it.ID)),
		kv("Repo", it.Repo),
		kv("State", backlogStateText(it.State)),
		kv("Note", orDash(it.Note)),
		kv("Priority", strconv.Itoa(it.Priority)),
		kv("Branch", orDash(it.Branch)),
		kv("PR", backlogPRText(it)),
		kv("Budget", fmt.Sprintf("%d runs of each kind (code %d, ci_fix %d, conflict_fix %d used)", it.Budget(), it.Used(backlog.Code), it.Used(backlog.CIFix), it.Used(backlog.ConflictFix))),
	}
	if it.Change != "" {
		lines = append(lines, "", "Changes requested (no run yet)")
		for _, l := range strings.Split(it.Change, "\n") {
			lines = append(lines, "  "+l)
		}
	}
	if it.Model != "" {
		lines = append(lines, kv("Model", it.Model))
	}
	lines = append(lines, "", "Runs")
	if len(it.Runs) == 0 {
		lines = append(lines, "  none yet")
	}
	for i, r := range it.Runs {
		outcome := "going"
		if r.Ended != nil {
			outcome = r.Outcome
			if r.Error != "" {
				outcome += ": " + r.Error
			}
		}
		lines = append(lines, fmt.Sprintf("  %d. %-12s %-18s %s  %s", i+1, r.Kind, r.Box, stamp(r.Started), outcome))
		if r.Ask != "" {
			lines = append(lines, "     asked: "+firstLine(r.Ask))
		}
	}
	lines = append(lines, "", "Goal")
	for _, l := range strings.Split(strings.TrimSpace(it.Goal), "\n") {
		lines = append(lines, "  "+l)
	}
	return lines
}

// backlogSummary is the header's Backlog line in :backlog: how many items run,
// wait and need you, against the limit -- or who runs the backlog instead.
func (m tuiModel) backlogSummary() string {
	if m.backlogHeld != "" {
		return "run by " + m.backlogHeld
	}
	if m.backlog == nil {
		return "-"
	}
	running, queued, needs := 0, 0, 0
	for _, it := range m.backlog.Items {
		switch {
		case it.Current() != nil:
			running++
		case it.State == backlog.Queued:
			queued++
		case it.State == backlog.NeedsYou:
			needs++
		}
	}
	s := fmt.Sprintf("%d/%d running, %d queued, %d need you", running, m.backlog.Limits.Global, queued, needs)
	if time.Now().Before(m.backlog.Backoff) {
		s += ", boxctl refused a box"
	}
	return s
}
