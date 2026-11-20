package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
	"github.com/spf13/cobra"
)

// `boxctl runs` and the tui's :runs: every run of every backlog item, the
// ones going and the ones that ended, one row each. The rows come from
// the backlog file (each item keeps its runs), joined to the box listing
// for how a run that's going is doing.

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "List the backlog's runs, going and ended",
	Long: `Every run of every backlog item, one row each: the ones going first,
then the newest first. A run that's going shows how its box says it's
doing; one that ended shows how it ended (done, failed, lost or killed).
The tui's :runs shows the same, with a run's shell, logs and kill.`,
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

// cells are the columns of `boxctl runs` and :runs.
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

// backlogRunJSON is a row of `boxctl runs -o json`.
type backlogRunJSON struct {
	Item    int        `json:"item"`
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
	var boxes map[string]backlog.Box
	if vms, err := newClient().List(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Listing boxes: %v (runs going show ?)\n", err)
	} else {
		boxes = backlogBoxes(vms)
	}
	rows := backlogRuns(q, boxes, 0)
	if format == "json" {
		out := []backlogRunJSON{}
		for _, r := range rows {
			out = append(out, backlogRunJSON{Item: r.it.ID, Repo: r.it.Repo, Kind: string(r.run.Kind), Box: r.run.Box, State: r.state, Started: r.run.Started, Ended: r.run.Ended, Error: r.run.Error})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "No runs yet: boxctl backlog add a goal, and the tui or boxctl backlog dispatch starts them.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ITEM\tREPO\tKIND\tBOX\tSTATE\tAGE")
	for _, r := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(r.cells(), "\t"))
	}
	return w.Flush()
}
