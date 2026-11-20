package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
)

// `:runs`: the backlog's runs, going and ended (runs.go's rows), with a
// run's box keys -- shell, logs, kill. Enter on an item in :backlog opens
// it scoped to that item's runs (page.item).

var runsView = &tableView{
	name:     "runs",
	title:    "Runs",
	resource: "backlog",
	columns:  []column{{"ITEM", 'I'}, {"REPO", 'R'}, {"KIND", 'K'}, {"BOX", 'B'}, {"STATE", 'S'}, {"AGE", 0}},
	stateCol: 4,
	empty:    "No runs yet: a goal in :backlog gets one when there's a slot.",
	hotkeys:  []hotkey{{"s", "shell"}, {"l", "logs"}, {"ctrl-d", "kill"}, {"L", "slot limit"}, {"d", "describe"}},
	rows: func(m *tuiModel) []tableRow {
		var rows []tableRow
		for _, r := range m.runRows() {
			rows = append(rows, tableRow{key: r.key(), cells: r.cells(), state: r.state})
		}
		return rows
	},
}

// runRows are the runs the top page shows: its item's, or every item's.
func (m *tuiModel) runRows() []backlogRun {
	var boxes map[string]backlog.Box
	if m.loaded["boxes"] {
		boxes = backlogBoxes(m.vms)
	}
	return backlogRuns(m.backlog, boxes, m.top().item)
}

// currentRun is the run under the cursor in :runs.
func (m *tuiModel) currentRun() *backlogRun {
	row := m.currentRow()
	if row == nil {
		return nil
	}
	for _, r := range m.runRows() {
		if r.key() == row.key {
			return &r
		}
	}
	return nil
}

// openRuns shows item's runs (all of them for 0) over the current page.
func (m *tuiModel) openRuns(item int) {
	m.push(page{kind: pageTable, view: runsView, sortCol: -1, item: item})
	m.filter.SetValue("")
}

// handleRunsKey is :runs' own keys; handled says whether key was one.
func (m tuiModel) handleRunsKey(key string) (tea.Model, tea.Cmd, bool) {
	switch key {
	case "L":
		md, cmd := m.askBacklogLimit()
		return md, cmd, true
	case "s", "l", "ctrl+d", "enter", "d", "y":
	default:
		return m, nil, false
	}
	r := m.currentRun()
	if r == nil {
		return m, nil, true
	}
	switch key {
	case "enter", "d":
		m.push(page{kind: pageText, title: "#" + r.key(), lines: describeRun(*r)})
		return m, nil, true
	case "y":
		m.push(page{kind: pageText, title: "#" + r.key() + ".json", lines: jsonLines(r.run)})
		return m, nil, true
	}
	if m.box(r.run.Box) == nil {
		return m, m.fail("%s", noRunBox(*r)), true
	}
	switch key {
	case "s":
		md, cmd := m.ssh(r.run.Box)
		return md, cmd, true
	case "l":
		md, cmd := m.logs(r.run.Box)
		return md, cmd, true
	}
	if r.run.Ended != nil {
		return m, m.fail("#%d's %s run already ended %s", r.it.ID, r.run.Kind, r.run.Outcome), true
	}
	md, cmd := m.askKill(*r)
	return md, cmd, true
}

// noRunBox is the flash for a key that needs a run's box when it isn't
// there.
func noRunBox(r backlogRun) string {
	switch {
	case r.run.Ended != nil:
		return fmt.Sprintf("#%d's %s run ended %s; its box is gone", r.it.ID, r.run.Kind, r.run.Outcome)
	case r.state == "gone":
		return fmt.Sprintf("#%d's %s run's box %s is missing", r.it.ID, r.run.Kind, r.run.Box)
	}
	return fmt.Sprintf("#%d's %s run isn't up yet", r.it.ID, r.run.Kind)
}

// askKill kills a run that's going: recorded in the backlog first (so no
// pass takes the box's going for a lost run and starts another), then its
// box deleted. A failed delete leaves the box to the next pass.
func (m tuiModel) askKill(r backlogRun) (tea.Model, tea.Cmd) {
	id, box, kind := r.it.ID, r.run.Box, r.run.Kind
	body := []string{
		"Its box is deleted, with whatever is on it.",
		fmt.Sprintf("#%d then waits for you: nothing more runs for it until r (retry) in :backlog.", id),
	}
	m.ask(fmt.Sprintf("Kill #%d's %s run on %s?", id, kind, box), body, func(m tuiModel) (tea.Model, tea.Cmd) {
		c := m.c
		return m, m.backlogChange(fmt.Sprintf("killed #%d's %s run; deleted %s", id, kind, box), func() error {
			err := backlog.Update(mustBacklogPath(), func(q *backlog.Backlog) error {
				it := q.Item(id)
				if it == nil {
					return fmt.Errorf("no backlog item #%d", id)
				}
				return backlog.Kill(it, box, time.Now())
			})
			if err != nil {
				return err
			}
			ctx, cancel := backlogCtx()
			defer cancel()
			if err := c.Destroy(ctx, box); err != nil && !strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("#%d's run is killed, but deleting %s failed (the next pass tries again): %v", id, box, err)
			}
			return nil
		})
	})
	return m, nil
}

// describeRun is :runs' describe page.
func describeRun(r backlogRun) []string {
	kv := func(k, v string) string { return fmt.Sprintf("%-10s%s", k+":", v) }
	ended, outcome := "-", "going: "+r.state
	if r.run.Ended != nil {
		ended, outcome = stamp(*r.run.Ended), r.run.Outcome
	}
	return []string{
		kv("Item", "#"+strconv.Itoa(r.it.ID)+" ("+r.it.Repo+")"),
		kv("Run", fmt.Sprintf("%d of %d", r.n, len(r.it.Runs))),
		kv("Kind", string(r.run.Kind)),
		kv("Box", r.run.Box),
		kv("Started", stamp(r.run.Started)),
		kv("Ended", ended),
		kv("Outcome", outcome),
		kv("Error", orDash(r.run.Error)),
	}
}
