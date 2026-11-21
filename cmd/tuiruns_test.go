package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
	"github.com/boxctl-io/boxctl-cli/internal/client"
)

// runsFixture saves a backlog where #3 had a failed code run (its box
// still there), a done code run (its box gone) and has a CI fix going on
// q3-ci-3, and #31 has a run still starting. It returns the boxes.
func runsFixture(t *testing.T) []client.VM {
	t.Helper()
	t0 := time.Now().Add(-3 * time.Hour)
	at := func(h int) *time.Time { x := t0.Add(time.Duration(h) * time.Hour); return &x }
	err := backlog.Update(mustBacklogPath(), func(q *backlog.Backlog) error {
		q.Next = 32
		q.Items = []*backlog.Item{
			{ID: 3, Repo: "o/r", Goal: "g", State: backlog.Active, PR: 7, Created: t0, Runs: []backlog.Run{
				{Kind: backlog.Code, Box: "q3-code-1", Started: t0, Launched: true, Ended: at(1), Outcome: backlog.OutcomeFailed, Error: "tests fail"},
				{Kind: backlog.Code, Box: "q3-code-2", Started: *at(1), Launched: true, Ended: at(2), Outcome: backlog.OutcomeDone},
				{Kind: backlog.CIFix, Box: "q3-ci-3", Started: *at(2), Launched: true},
			}},
			{ID: 31, Repo: "o/s", Goal: "h", State: backlog.Active, Created: t0, Runs: []backlog.Run{
				{Kind: backlog.Code, Box: "q31-code-1", Started: *at(2)},
			}},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return []client.VM{
		{ID: "1", Name: "q3-code-1", State: "running", RunStatus: json.RawMessage(`{"status":"failed"}`)},
		{ID: "2", Name: "q3-ci-3", State: "running", RunStatus: json.RawMessage(`{"status":"running"}`)},
	}
}

// runsTUI is a tui on :backlog over runsFixture, its listings loaded.
func runsTUI(t *testing.T, c *client.Client, vms []client.VM) tuiModel {
	t.Helper()
	m := newTUI(c)
	m.vms = vms
	m.loaded["boxes"] = true
	q, err := loadBacklog()
	if err != nil {
		t.Fatal(err)
	}
	m.backlog = q
	m.pages = []page{{kind: pageTable, view: backlogView, sortCol: -1}}
	return m
}

// feedBacklog runs cmd into m, and the backlog reload a change asks for.
func feedBacklog(m tuiModel, cmd tea.Cmd) tuiModel {
	for _, msg := range runCmd(cmd) {
		if msg == nil {
			continue
		}
		next, more := m.Update(msg)
		m = next.(tuiModel)
		if _, ok := msg.(tuiBacklogDoneMsg); ok {
			for _, msg := range runCmd(more) {
				if lm, ok := msg.(tuiListMsg); ok {
					next, _ := m.Update(lm)
					m = next.(tuiModel)
				}
			}
		}
	}
	return m
}

func TestTUIRuns(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	vms := runsFixture(t)
	c, calls := fakeBoxServer(t, vms)
	m := runsTUI(t, c, vms)

	// enter on #3 opens its runs: the one going first, then the newest.
	m.top().selected = "3"
	m, _ = press(t, m, "enter")
	if m.top().view != runsView || m.top().item != 3 {
		t.Fatalf("enter: view %s item %d", m.top().view.name, m.top().item)
	}
	if got := keys(m.rows()); got != "3/3 3/2 3/1" {
		t.Fatalf("#3's runs = %q", got)
	}
	if rows := m.rows(); rows[0].cells[4] != "running" || rows[1].cells[4] != "done" || rows[2].cells[4] != "failed" {
		t.Errorf("states = %v", rows)
	}
	if !strings.Contains(m.crumbsView(), "runs(#3)") {
		t.Errorf("crumbs = %q", m.crumbsView())
	}
	// d describes the run.
	m.top().selected = "3/1"
	m2, _ := press(t, m, "d")
	if page := strings.Join(m2.top().lines, "\n"); !strings.Contains(page, "failed") || !strings.Contains(page, "tests fail") {
		t.Errorf("describe:\n%s", page)
	}
	// l on an ended run whose box is still there reads its log.
	m2, _ = press(t, m, "l")
	if _, ok := m2.pending["logs:q3-code-1"]; !ok {
		t.Errorf("l on a failed run's box: pending %v, flash %q", m2.pending, m2.flash)
	}
	// s on an ended run whose box went says so.
	m.top().selected = "3/2"
	m2, cmd := press(t, m, "s")
	if cmd == nil || !strings.Contains(m2.flash, "its box is gone") {
		t.Errorf("s on a gone box: %q", m2.flash)
	}
	// esc goes back to :backlog, still on #3.
	m, _ = press(t, m, "esc")
	if m.top().view != backlogView || m.currentRow().key != "3" {
		t.Fatalf("esc: %s %v", m.top().view.name, m.currentRow())
	}

	// :runs is every item's.
	next, _ := m.runCommand("runs")
	m = next.(tuiModel)
	if got := keys(m.rows()); got != "3/3 31/1 3/2 3/1" {
		t.Fatalf(":runs = %q", got)
	}
	// ctrl-d on a run still starting does nothing.
	m.top().selected = "31/1"
	m2, _ = press(t, m, "ctrl+d")
	if m2.dialog != nil || !strings.Contains(m2.flash, "isn't up yet") {
		t.Errorf("kill while starting: dialog %v, flash %q", m2.dialog, m2.flash)
	}
	// L asks for the slot limit here too.
	if m2, _ = press(t, m, "L"); m2.dialog == nil {
		t.Error("L: no dialog")
	}

	// ctrl-d on the CI fix asks; y kills it: recorded, then its box deleted.
	m.top().selected = "3/3"
	m, _ = press(t, m, "ctrl+d")
	if m.dialog == nil || m.dialog.title != "Kill #3's ci_fix run on q3-ci-3?" {
		t.Fatalf("dialog = %+v", m.dialog)
	}
	m, cmd = press(t, m, "y")
	m = feedBacklog(m, cmd)
	q, _ := loadBacklog()
	it := q.Item(3)
	if it.State != backlog.NeedsYou || it.Last().Outcome != backlog.OutcomeKilled || !strings.Contains(it.Note, "run killed") {
		t.Errorf("after the kill: %s %q %+v", it.State, it.Note, it.Last())
	}
	if !strings.Contains(strings.Join(*calls, " "), "DELETE /api/vms/q3-ci-3") {
		t.Errorf("calls = %v", *calls)
	}
}

func TestTUIRunsKillDeleteFails(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	vms := runsFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(vms)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	m := runsTUI(t, client.New(srv.URL, "tok"), vms)
	m.openRuns(0)
	m.top().selected = "3/3"
	m, _ = press(t, m, "ctrl+d")
	m, cmd := press(t, m, "y")
	m = feedBacklog(m, cmd)
	if !strings.Contains(m.flash, "deleting q3-ci-3 failed") {
		t.Errorf("flash = %q", m.flash)
	}
	q, _ := loadBacklog()
	if q.Item(3).Last().Outcome != backlog.OutcomeKilled {
		t.Errorf("run = %+v", q.Item(3).Last())
	}
}

func TestBacklogRunsStates(t *testing.T) {
	q := &backlog.Backlog{Items: []*backlog.Item{{ID: 1, Runs: []backlog.Run{
		{Box: "a", Launched: true},
		{Box: "b"},
		{Box: "c", Launched: true},
	}}}}
	boxes := map[string]backlog.Box{"c": {State: "paused"}}
	var got []string
	for _, r := range backlogRuns(q, boxes, 0) {
		got = append(got, r.run.Box+"="+r.state)
	}
	if strings.Join(got, " ") != "a=gone b=starting c=paused" {
		t.Errorf("states = %v", got)
	}
	if r := backlogRuns(q, nil, 0); r[0].state != "?" {
		t.Errorf("no listing: %q", r[0].state)
	}
}

// A run whose box has reported how it ended can't be killed: the next pass
// records it, with the pull request it opened.
func TestTUIRunsNoKillAfterTheEnd(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	vms := runsFixture(t)
	vms[1].RunStatus = json.RawMessage(`{"status":"done","pr_url":"https://github.com/o/r/pull/7"}`)
	c, calls := fakeBoxServer(t, vms)
	m := runsTUI(t, c, vms)
	m.openRuns(3)
	m.top().selected = "3/3"
	m, _ = press(t, m, "ctrl+d")
	if m.dialog != nil || !strings.Contains(m.flash, "already done") || len(*calls) != 0 {
		t.Errorf("dialog %v, flash %q, calls %v", m.dialog, m.flash, *calls)
	}
}

// ITEM sorts by number, not text: #9 before #10.
func TestTUIRunsSortByItem(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	now := time.Now()
	_ = backlog.Update(mustBacklogPath(), func(q *backlog.Backlog) error {
		q.Items = []*backlog.Item{
			{ID: 10, Repo: "o/r", Runs: []backlog.Run{{Kind: backlog.Code, Box: "q10-code-1", Started: now}}},
			{ID: 9, Repo: "o/r", Runs: []backlog.Run{{Kind: backlog.Code, Box: "q9-code-1", Started: now.Add(-time.Hour)}}},
		}
		return nil
	})
	m := runsTUI(t, nil, nil)
	m.openRuns(0)
	m.top().sortCol = 0
	if got := keys(m.rows()); got != "9/1 10/1" {
		t.Errorf("by item = %q", got)
	}
}

// r in :runs resumes a run's paused box (a goal box the idle reaper
// paused once its run ended), p pauses a running one; both refuse the
// wrong state, and a run whose box is gone says so.
func TestTUIRunsPauseResume(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	vms := runsFixture(t)
	vms[0].State = "paused" // q3-code-1, the failed run's box
	c, calls := fakeBoxServer(t, vms)
	m := runsTUI(t, c, vms)
	next, _ := m.runCommand("runs")
	m = next.(tuiModel)

	m.top().selected = "3/1"
	m2, cmd := press(t, m, "p")
	if hasAction(cmd) != nil || !strings.Contains(m2.flash, "not running") {
		t.Errorf("p on a paused box: flash %q", m2.flash)
	}
	m2, cmd = press(t, m, "r")
	if hasAction(cmd) == nil || m2.pending["q3-code-1"] != "resume" {
		t.Fatalf("r on a paused box: pending %v, flash %q", m2.pending, m2.flash)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST /api/vms/q3-code-1/resume" {
		t.Errorf("server saw %v", *calls)
	}
	*calls = nil

	m.top().selected = "3/3"
	m2, cmd = press(t, m, "r")
	if hasAction(cmd) != nil || !strings.Contains(m2.flash, "not paused") {
		t.Errorf("r on a running box: flash %q", m2.flash)
	}
	m2, cmd = press(t, m, "p")
	if hasAction(cmd) == nil || m2.pending["q3-ci-3"] != "pause" {
		t.Fatalf("p on a running box: pending %v, flash %q", m2.pending, m2.flash)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST /api/vms/q3-ci-3/pause" {
		t.Errorf("server saw %v", *calls)
	}

	m.top().selected = "3/2"
	if m2, _ = press(t, m, "r"); !strings.Contains(m2.flash, "its box is gone") {
		t.Errorf("r on a gone box: flash %q", m2.flash)
	}
}
