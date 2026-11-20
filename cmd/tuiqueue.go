package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/queue"
	tea "github.com/charmbracelet/bubbletea"
)

// `:queue`: boxctl queue in the tui. The rows are ~/.boxctl/queue.json,
// read again on every refresh; the passes that move it (queuerun.go) run
// every queuePassEvery while the tui is open, whatever view is shown --
// the tui is the queue's loop unless `boxctl queue run` already is.

type (
	tuiQueuePassMsg struct {
		events []queueEvent
		err    error
	}
	tuiQueueDoneMsg struct { // a key's change to the queue was made
		text string
		err  error
	}
)

var queueView = &tableView{
	name:     "queue",
	aliases:  []string{"q", "goals", "work"},
	title:    "Queue",
	resource: "queue",
	columns:  []column{{"ID", 'I'}, {"PRI", 'P'}, {"REPO", 'R'}, {"STATE", 'S'}, {"RUN", 0}, {"PR", 0}, {"NOTE", 'N'}, {"GOAL", 'G'}},
	stateCol: 3,
	empty:    "Nothing queued: a queues a goal (here, or on a repository in :repos).",
	hotkeys: []hotkey{{"a", "queue a goal"}, {"m", "merge"}, {"r", "retry"}, {"+/-", "priority"}, {"o", "open PR"},
		{"l", "run's log"}, {"s", "run's shell"}, {"ctrl-d", "cancel"}, {"d", "describe"}},
	rows: func(m *tuiModel) []tableRow {
		if m.queue == nil {
			return nil
		}
		var rows []tableRow
		for i, it := range m.queue.Sorted() {
			rows = append(rows, tableRow{
				key:      strconv.Itoa(it.ID),
				state:    queueStateText(it.State),
				cells:    []string{strconv.Itoa(it.ID), strconv.Itoa(it.Priority), it.Repo, queueStateText(it.State), queueRunText(it), queuePRText(it), orDash(it.Note), firstLine(it.Goal)},
				sortKeys: []string{fmt.Sprintf("%08d", it.ID), fmt.Sprintf("%08d", 1<<20-it.Priority), "", fmt.Sprintf("%04d", i)},
			})
		}
		return rows
	},
}

// queueStateText is the STATE cell, which the table colours: "needs you"
// as an error, done as finished.
func queueStateText(s queue.State) string {
	return strings.ReplaceAll(string(s), "_", " ")
}

// loadQueueMsg reads the queue file for the view.
func loadQueueMsg() tea.Msg {
	q, err := loadQueue()
	return tuiListMsg{resource: "queue", queue: q, err: err}
}

// queuePass runs one pass in the background, unless one is going or the
// tui has no runner (tests).
func (m *tuiModel) queuePass() tea.Cmd {
	if m.qr == nil || m.queuePassing {
		return nil
	}
	m.queuePassing = true
	r := m.qr
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		events, err := r.pass(ctx)
		return tuiQueuePassMsg{events: events, err: err}
	}
}

// queuePassed shows what a pass did (its last event, or its error) and
// reloads the queue.
func (m *tuiModel) queuePassed(msg tuiQueuePassMsg) tea.Cmd {
	m.queuePassing = false
	m.queuePassAt = time.Now()
	var held errLoopHeld
	m.queueHeld = ""
	cmds := []tea.Cmd{loadQueueMsg}
	switch {
	case errors.As(msg.err, &held):
		m.queueHeld = held.holder
	case msg.err != nil:
		cmds = append(cmds, m.fail("queue: %v", msg.err))
	case len(msg.events) > 0:
		e := msg.events[len(msg.events)-1]
		if e.err {
			cmds = append(cmds, m.fail("queue %s", e.text))
		} else {
			cmds = append(cmds, m.say("queue %s", e.text))
		}
	}
	return tea.Batch(cmds...)
}

// queueItem is the item under the cursor in :queue.
func (m *tuiModel) queueItem() *queue.Item {
	if m.queue == nil || m.top().view != queueView {
		return nil
	}
	r := m.currentRow()
	if r == nil {
		return nil
	}
	id, _ := strconv.Atoi(r.key)
	return m.queue.Item(id)
}

// queueChange runs fn on the queue in the background, then flashes text
// and reloads it -- and runs a pass, so the change takes effect now.
func (m *tuiModel) queueChange(text string, fn func() error) tea.Cmd {
	return func() tea.Msg {
		if err := fn(); err != nil {
			return tuiQueueDoneMsg{err: err}
		}
		return tuiQueueDoneMsg{text: text}
	}
}

// handleQueueKey is :queue's own keys; handled says whether key was one.
func (m tuiModel) handleQueueKey(key string) (tea.Model, tea.Cmd, bool) {
	it := m.queueItem()
	switch key {
	case "a":
		md, cmd := m.askQueueRepo()
		return md, cmd, true
	case "enter", "d":
		if it != nil {
			m.push(page{kind: pageText, title: fmt.Sprintf("#%d", it.ID), lines: describeQueueItem(it)})
		}
		return m, nil, true
	case "y":
		if it != nil {
			m.push(page{kind: pageText, title: fmt.Sprintf("#%d.json", it.ID), lines: jsonLines(it)})
		}
		return m, nil, true
	}
	if it == nil {
		return m, nil, key == "m" || key == "r" || key == "+" || key == "-" || key == "o" || key == "l" || key == "s" || key == "ctrl+d"
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
			return m, m.queueChange(fmt.Sprintf("merged %s#%d", item.Repo, item.PR), func() error {
				ctx, cancel := queueCtx()
				defer cancel()
				return mergeQueueItem(ctx, &item, "squash")
			})
		})
		return m, nil, true
	case "r":
		return m, m.queueChange(fmt.Sprintf("#%d is active again", id), func() error { return retryQueueItem(id) }), true
	case "+", "-":
		d := 1
		if key == "-" {
			d = -1
		}
		return m, m.queueChange(fmt.Sprintf("#%d's priority changed", id), func() error {
			return changeQueueItem(id, func(it *queue.Item) error { it.Priority += d; return nil })
		}), true
	case "ctrl+d":
		m.ask(fmt.Sprintf("Cancel #%d?", id), []string{
			"Its boxes are deleted; its branch and pull request, if any, stay on GitHub.",
		}, func(m tuiModel) (tea.Model, tea.Cmd) {
			return m, m.queueChange(fmt.Sprintf("cancelled #%d", id), func() error { return cancelQueueItem(id) })
		})
		return m, nil, true
	case "o":
		if it.PR == 0 {
			return m, m.fail("#%d has no pull request yet", id), true
		}
		repo, pr := it.Repo, it.PR
		return m, func() tea.Msg {
			err := exec.Command("gh", "pr", "view", strconv.Itoa(pr), "--repo", repo, "--web").Run()
			if err != nil {
				return tuiQueueDoneMsg{err: fmt.Errorf("opening %s#%d: %v", repo, pr, err)}
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

// askQueueRepo asks which repository a new goal is for (the current
// item's, to start with), then the goal.
func (m tuiModel) askQueueRepo() (tea.Model, tea.Cmd) {
	cmd := m.prompt("Queue a goal: which repository?", []string{"owner/name; the goal is asked next."}, "owner/name", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		owner, name, err := parseRepo(value)
		if err != nil {
			return m, m.fail("%v", err)
		}
		return m.askQueueGoal(owner + "/" + name)
	})
	if it := m.queueItem(); it != nil && m.dialog != nil && m.dialog.input != nil {
		m.dialog.input.SetValue(it.Repo)
	}
	return m, cmd
}

// askQueueGoal asks for a goal on repo and queues it.
func (m tuiModel) askQueueGoal(repo string) (tea.Model, tea.Cmd) {
	cmd := m.promptText("Queue a goal on "+repo, []string{
		"It runs unattended (boxctl claude --goal) when a slot is free, and ends in a pull request",
		"that :queue then watches: CI and conflicts get follow-up runs; m merges it once it's green.",
	}, "what to do, e.g. bump Go and fix what breaks", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		if value == "" {
			return m, m.fail("a goal is needed")
		}
		return m, m.queueChange("queued a goal on "+repo, func() error {
			_, err := addQueueItem(repo, value, "", 0)
			return err
		})
	})
	return m, cmd
}

// describeQueueItem is :queue's describe page.
func describeQueueItem(it *queue.Item) []string {
	kv := func(k, v string) string { return fmt.Sprintf("%-12s%s", k+":", v) }
	lines := []string{
		kv("Item", "#"+strconv.Itoa(it.ID)),
		kv("Repo", it.Repo),
		kv("State", queueStateText(it.State)),
		kv("Note", orDash(it.Note)),
		kv("Priority", strconv.Itoa(it.Priority)),
		kv("Branch", orDash(it.Branch)),
		kv("PR", queuePRText(it)),
		kv("Budget", fmt.Sprintf("%d runs of each kind (code %d, ci_fix %d, conflict_fix %d used)", it.Budget(), it.Used(queue.Code), it.Used(queue.CIFix), it.Used(queue.ConflictFix))),
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
	}
	lines = append(lines, "", "Goal")
	for _, l := range strings.Split(strings.TrimSpace(it.Goal), "\n") {
		lines = append(lines, "  "+l)
	}
	return lines
}

// queueSummary is the header's Queue line in :queue: how many items run,
// wait and need you, against the limit -- or who runs the queue instead.
func (m tuiModel) queueSummary() string {
	if m.queueHeld != "" {
		return "run by " + m.queueHeld
	}
	if m.queue == nil {
		return "-"
	}
	running, queued, needs := 0, 0, 0
	for _, it := range m.queue.Items {
		switch {
		case it.Current() != nil:
			running++
		case it.State == queue.Queued:
			queued++
		case it.State == queue.NeedsYou:
			needs++
		}
	}
	s := fmt.Sprintf("%d/%d running, %d queued, %d need you", running, m.queue.Limits.Global, queued, needs)
	if time.Now().Before(m.queue.Backoff) {
		s += ", boxctl refused a box"
	}
	return s
}
