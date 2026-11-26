package cmd

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
	"github.com/boxctl-io/boxctl-cli/internal/client"
	"github.com/spf13/cobra"
)

// `boxctl runs`: every run, the backlog's and the rest, one row each.
// A backlog run's row comes from the backlog file (each item keeps its
// runs), joined to the box listing for how a run that's going is doing.
// Every other run is a box boxctl claude made outside the backlog, told
// from its labels and run_status alone, as claude ls does. boxctl
// board's :runs shows the backlog's.
//
// A run has a kind. An unattended run's box is labeled boxctl.run with
// it (backlog.Kind's: code, ci_fix, conflict_fix, change); an
// interactive run's with what made its box -- session (an ephemeral
// run), box (the project's own) or repo (an attended --repo run).

// runInteractive is the kind of a run someone is attached to.
const runInteractive = "interactive"

// unattendedKind reports whether kind is an unattended run's.
func unattendedKind(kind string) bool {
	switch backlog.Kind(kind) {
	case backlog.Code, backlog.CIFix, backlog.ConflictFix, backlog.Change:
		return true
	}
	return false
}

// unattendedBox reports whether vm holds an unattended run.
func unattendedBox(vm client.VM) bool { return unattendedKind(vm.Labels["boxctl.run"]) }

// runKind is the kind of vm's run, "" for a box boxctl claude didn't make.
func runKind(vm client.VM) string {
	switch run := vm.Labels["boxctl.run"]; {
	case run == "":
		return ""
	case unattendedKind(run):
		return run
	}
	return runInteractive
}

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "List every run, going and ended",
	Long: `Every run, one row each: the backlog's (ITEM is the backlog item) and
every other run boxctl claude started -- an unattended --task or --issue
run, or an interactive one -- going first, then the newest first. KIND
is interactive, code (a new branch, ending in a pull request), or a
follow-up on a pull request's branch: ci_fix, conflict_fix or change.
A run that's going shows how its box says it's doing; one that ended
shows how it ended (done, failed, lost or killed). A run outside the
backlog is listed while its box is there. boxctl board's :runs shows
the backlog's runs, with a run's shell, logs and kill.`,
	Args: cobra.NoArgs,
	RunE: runRuns,
}

func init() {
	addOutputFlag(runsCmd)
	rootCmd.AddCommand(runsCmd)
}

// backlogRun is one run of an item: its n-th (from 1).
type backlogRun struct {
	it    *backlog.Item
	n     int
	run   backlog.Run
	state string
}

func (r backlogRun) key() string { return fmt.Sprintf("%d/%d", r.it.ID, r.n) }

// cells are the columns of :runs (and `boxctl runs`, through allRuns).
func (r backlogRun) cells() []string {
	return []string{"#" + strconv.Itoa(r.it.ID), r.it.Repo, string(r.run.Kind), r.run.Box, r.state, age(r.run.Started)}
}

// backlogRuns are the runs of q, of item only when it isn't 0: the ones going
// first, then the newest first. boxes is every box by name, nil when they
// couldn't be listed.
func backlogRuns(q *backlog.Backlog, boxes map[string]backlog.Box, item int) []backlogRun {
	var rows []backlogRun
	if q == nil {
		return nil
	}
	for _, it := range q.Items {
		if item != 0 && it.ID != item {
			continue
		}
		for i, r := range it.Runs {
			rows = append(rows, backlogRun{it: it, n: i + 1, run: r, state: runState(r, boxes)})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].run, rows[j].run
		if (a.Ended == nil) != (b.Ended == nil) {
			return a.Ended == nil
		}
		return a.Started.After(b.Started)
	})
	return rows
}

// runState is the STATE cell: how an ended run ended, or how a run that's
// going is doing by its box -- the status the box reported, else the
// box's state. With no box: "starting" until boxctl claude has made it,
// "gone" after (the next pass ends it as lost), "?" when the boxes
// couldn't be listed.
func runState(r backlog.Run, boxes map[string]backlog.Box) string {
	if r.Ended != nil {
		return r.Outcome
	}
	if boxes == nil {
		return "?"
	}
	b, ok := boxes[r.Box]
	switch {
	case !ok && r.Launched:
		return "gone"
	case !ok:
		return "starting"
	case b.Status != "":
		return b.Status
	}
	return b.State
}

// runsRow is a row of `boxctl runs`: a backlog run's, or a box's run
// (item 0).
type runsRow struct {
	item                   int
	repo, kind, box, state string
	started                time.Time
	ended                  *time.Time
	going                  bool
	err                    string
}

func (r runsRow) cells() []string {
	item := "-"
	if r.item != 0 {
		item = "#" + strconv.Itoa(r.item)
	}
	return []string{item, r.repo, r.kind, r.box, r.state, age(r.started)}
}

// allRuns are `boxctl runs`' rows: q's runs, and the run of every box in
// vms that boxctl claude made outside the backlog; going first, then the
// newest first. vms is nil when the boxes couldn't be listed.
func allRuns(q *backlog.Backlog, vms []client.VM) []runsRow {
	var boxes map[string]backlog.Box
	if vms != nil {
		boxes = backlogBoxes(vms)
	}
	var rows []runsRow
	for _, r := range backlogRuns(q, boxes, 0) {
		rows = append(rows, runsRow{item: r.it.ID, repo: r.it.Repo, kind: string(r.run.Kind), box: r.run.Box, state: r.state,
			started: r.run.Started, ended: r.run.Ended, going: r.run.Ended == nil, err: r.run.Error})
	}
	for _, vm := range vms {
		kind := runKind(vm)
		if kind == "" || vm.Labels["boxctl.backlog"] != "" {
			continue
		}
		row := runsRow{repo: projectText(vm), kind: kind, box: vm.Name, state: vm.State, started: vm.CreatedAt, going: true}
		if st, err := parseGoalStatus(vm.RunStatus); err == nil && st != nil && st.Status != "" {
			row.state, row.going, row.err = st.Status, !st.finished(), st.Error
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.going != b.going {
			return a.going
		}
		return a.started.After(b.started)
	})
	return rows
}

// runJSON is a row of `boxctl runs -o json`.
type runJSON struct {
	Item    int        `json:"item,omitempty"`
	Repo    string     `json:"repo"`
	Kind    string     `json:"kind"`
	Box     string     `json:"box"`
	State   string     `json:"state"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	Error   string     `json:"error,omitempty"`
}

func runRuns(cmd *cobra.Command, args []string) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	q, err := loadBacklog()
	if err != nil {
		return err
	}
	ctx, cancel := backlogCtx()
	defer cancel()
	vms, err := newClient().List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Listing boxes: %v (backlog runs going show ?, and runs outside the backlog are missing)\n", err)
		vms = nil
	}
	rows := allRuns(q, vms)
	if format == "json" {
		var out []runJSON
		for _, r := range rows {
			out = append(out, runJSON{Item: r.item, Repo: r.repo, Kind: r.kind, Box: r.box, State: r.state, Started: r.started, Ended: r.ended, Error: r.err})
		}
		return printJSON(out)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "No runs yet: boxctl claude --task \"...\" starts one, and so do boxctl board and boxctl backlog dispatch for the backlog's tasks.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ITEM\tREPO\tKIND\tBOX\tSTATE\tAGE")
	for _, r := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(r.cells(), "\t"))
	}
	return w.Flush()
}
