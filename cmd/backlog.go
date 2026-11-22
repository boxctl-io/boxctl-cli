package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
	"github.com/spf13/cobra"
)

// boxctl backlog: goals waiting to become pull requests, worked on by
// unattended `boxctl claude --goal` runs, a few at a time (internal/backlog
// has the rules). boxctl board runs the passes while it's open;
// `boxctl backlog dispatch` runs them with no screen.

var backlogCmd = &cobra.Command{
	Use:   "backlog",
	Short: "Goals for Claude to turn into pull requests, a few at a time",
	Long: `A backlog of goals, each ending in one pull request. Each one becomes an
unattended boxctl claude --goal run on a box of its own, a few at a time
(boxctl backlog limit), and its pull request is then watched until you merge it:

  - CI fails: the failed jobs are re-run once; if they fail again, a
    follow-up run fixes it on the pull request's branch (claude --pr).
  - It conflicts with its base: a follow-up run merges the base in.
  - It's behind its base: GitHub is asked to update the branch, and it's
    ready again only once CI passes on that.
  - Green and up to date: "ready to merge" -- boxctl backlog merge, or m in
    boxctl board. Nothing is merged without you.

Each kind of run gets 2 attempts (boxctl backlog retry adds one); after that
the item needs you. A run's box is deleted once the next run starts or the
pull request is merged.

The passes run while boxctl board is open (its :backlog shows the backlog)
or boxctl backlog dispatch is, never both. Nothing moves while neither is running;
runs already on boxes carry on.

  boxctl backlog add --repo owner/name "bump Go and fix what breaks"
  boxctl backlog                     # list it
  boxctl backlog merge 3             # merge #3's pull request
  boxctl backlog dispatch           # run the passes here, no screen`,
	Args: cobra.NoArgs,
	RunE: runBacklogLs,
}

var backlogLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List the backlog",
	Args:    cobra.NoArgs,
	RunE:    runBacklogLs,
}

var (
	backlogRepo     string
	backlogPriority int
	backlogModel    string
	backlogMethod   string
	backlogGlobal   int
	backlogPerRepo  int
	backlogAll      bool
)

var backlogAddCmd = &cobra.Command{
	Use:   "add [--repo owner/name] <goal...>",
	Short: "Add a goal (- reads it from stdin)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		goal := strings.TrimSpace(strings.Join(args, " "))
		if goal == "-" {
			data, err := readAllLimited(os.Stdin, 1<<20)
			if err != nil {
				return err
			}
			goal = strings.TrimSpace(data)
		}
		if goal == "" {
			return errors.New("an empty goal")
		}
		repo := backlogRepo
		if repo == "" {
			dir, err := projectDir()
			if err != nil {
				return err
			}
			if repo = githubRepo(dir); repo == "" {
				return fmt.Errorf("--repo owner/name: %s has no GitHub origin", dir)
			}
		}
		owner, name, err := parseRepo(repo)
		if err != nil {
			return err
		}
		it, err := addBacklogItem(owner+"/"+name, goal, backlogModel, backlogPriority)
		if err != nil {
			return err
		}
		fmt.Printf("Queued #%d on %s.\n", it.ID, it.Repo)
		if q, err := loadBacklog(); err == nil && (q.Loop == "" || time.Since(q.LoopSeen) > backlog.LoopStale) {
			fmt.Fprintln(os.Stderr, "Nothing is running the backlog: open boxctl board or run boxctl backlog dispatch.")
		}
		return nil
	},
}

var backlogDispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Run the backlog's passes here until interrupted, with no screen",
	Long: `Runs a pass every 30s -- start runs, follow them, watch their pull
requests -- printing what each one does, until Ctrl-C. The tui does the
same while it's open; only one of them runs the passes at a time.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		r, err := newBacklogRunner(newClient())
		if err != nil {
			return err
		}
		defer r.release()
		for {
			events, err := r.pass(ctx)
			for _, e := range events {
				prefix := ""
				if e.err {
					prefix = "warning: "
				}
				fmt.Fprintf(os.Stderr, "%s %s%s\n", time.Now().Format("15:04:05"), prefix, e.text)
			}
			var held errLoopHeld
			if errors.As(err, &held) {
				return err
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s warning: %v\n", time.Now().Format("15:04:05"), err)
			}
			select {
			case <-ctx.Done():
				fmt.Fprintln(os.Stderr, "Stopping; runs already on boxes carry on.")
				return nil
			case <-time.After(backlogPassEvery):
			}
		}
	},
}

var backlogMergeCmd = &cobra.Command{
	Use:   "merge <id>",
	Short: "Merge an item's pull request, at the commit CI passed on",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := backlogID(args[0])
		if err != nil {
			return err
		}
		q, err := loadBacklog()
		if err != nil {
			return err
		}
		it := q.Item(id)
		if it == nil {
			return fmt.Errorf("no backlog item #%d", id)
		}
		if err := mergeBacklogItem(cmd.Context(), it, backlogMethod); err != nil {
			return err
		}
		if err := markBacklogMerged(id); err != nil {
			return fmt.Errorf("merged %s#%d, but couldn't record it: %w", it.Repo, it.PR, err)
		}
		fmt.Printf("Merged %s#%d; #%d is done. The next pass deletes its box.\n", it.Repo, it.PR, id)
		return nil
	},
}

var backlogCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Stop working on an item (its pull request and branch stay)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := backlogID(args[0])
		if err != nil {
			return err
		}
		if err := cancelBacklogItem(id); err != nil {
			return err
		}
		fmt.Printf("Cancelled #%d; the next pass deletes its boxes.\n", id)
		return nil
	},
}

var backlogRetryCmd = &cobra.Command{
	Use:   "retry <id>",
	Short: "Give an item that needs you one more run of each kind",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := backlogID(args[0])
		if err != nil {
			return err
		}
		if err := retryBacklogItem(id); err != nil {
			return err
		}
		fmt.Printf("#%d is active again.\n", id)
		return nil
	},
}

var backlogLimitCmd = &cobra.Command{
	Use:   "limit",
	Short: "Show or set how many runs go at once",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		var global, perRepo *int
		if cmd.Flags().Changed("global") {
			global = &backlogGlobal
		}
		if cmd.Flags().Changed("per-repo") {
			perRepo = &backlogPerRepo
		}
		l, err := setBacklogLimits(global, perRepo)
		if err != nil {
			return err
		}
		fmt.Printf("At most %d runs at once, %d per repository.\n", l.Global, l.PerRepo)
		return nil
	},
}

func init() {
	addOutputFlag(backlogCmd)
	addOutputFlag(backlogLsCmd)
	for _, c := range []*cobra.Command{backlogCmd, backlogLsCmd} {
		c.Flags().BoolVarP(&backlogAll, "all", "a", false, "also list done and cancelled items")
	}
	backlogAddCmd.Flags().StringVarP(&backlogRepo, "repo", "R", "", "GitHub repository (owner/name; default: this directory's origin)")
	backlogAddCmd.Flags().IntVarP(&backlogPriority, "priority", "p", 0, "higher starts first")
	backlogAddCmd.Flags().StringVar(&backlogModel, "model", "", "model for its runs (claude --model)")
	backlogMergeCmd.Flags().StringVar(&backlogMethod, "method", "squash", "merge, squash or rebase")
	backlogLimitCmd.Flags().IntVar(&backlogGlobal, "global", 0, "runs at once in all")
	backlogLimitCmd.Flags().IntVar(&backlogPerRepo, "per-repo", 0, "runs at once on one repository")
	backlogCmd.AddCommand(backlogLsCmd, backlogAddCmd, backlogDispatchCmd, backlogMergeCmd, backlogCancelCmd, backlogRetryCmd, backlogLimitCmd)
	rootCmd.AddCommand(backlogCmd)
}

func mustBacklogPath() string {
	p, err := backlog.Path()
	if err != nil {
		return "backlog.json"
	}
	return p
}

func loadBacklog() (*backlog.Backlog, error) { return backlog.Load(mustBacklogPath()) }

func backlogID(s string) (int, error) {
	id, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%q: use an item's number, as boxctl backlog lists it", s)
	}
	return id, nil
}

func addBacklogItem(repo, goal, model string, priority int) (*backlog.Item, error) {
	var it *backlog.Item
	err := backlog.Update(mustBacklogPath(), func(q *backlog.Backlog) error {
		it = q.Add(repo, goal, model, priority, time.Now())
		return nil
	})
	return it, err
}

// changeBacklogItem applies fn to item id under the backlog's lock.
// setBacklogLimits sets the limits given (nil leaves one as it is) and
// returns them all: `backlog limit` and :backlog's L.
func setBacklogLimits(global, perRepo *int) (backlog.Limits, error) {
	var l backlog.Limits
	err := backlog.Update(mustBacklogPath(), func(q *backlog.Backlog) error {
		if global != nil {
			q.Limits.Global = *global
		}
		if perRepo != nil {
			q.Limits.PerRepo = *perRepo
		}
		if q.Limits.Global < 1 || q.Limits.PerRepo < 1 {
			return errors.New("a limit is at least 1")
		}
		l = q.Limits
		return nil
	})
	return l, err
}

func changeBacklogItem(id int, fn func(it *backlog.Item) error) error {
	return backlog.Update(mustBacklogPath(), func(q *backlog.Backlog) error {
		it := q.Item(id)
		if it == nil {
			return fmt.Errorf("no backlog item #%d", id)
		}
		if err := fn(it); err != nil {
			return err
		}
		it.Updated = time.Now()
		return nil
	})
}

func cancelBacklogItem(id int) error {
	return changeBacklogItem(id, func(it *backlog.Item) error {
		if it.State == backlog.Done || it.State == backlog.Cancelled {
			return fmt.Errorf("#%d is %s already", id, it.State)
		}
		it.State, it.Note = backlog.Cancelled, "cancelled"
		if r := it.Current(); r != nil {
			now := time.Now()
			r.Ended, r.Outcome, r.Error = &now, backlog.OutcomeLost, "cancelled"
		}
		return nil
	})
}

// markBacklogMerged records a merge made here at once, rather than leaving
// the item "ready to merge" until a pass sees the PR merged. The pass
// still deletes its boxes (done items keep none).
func markBacklogMerged(id int) error {
	return changeBacklogItem(id, func(it *backlog.Item) error {
		if it.State == backlog.Cancelled {
			return nil
		}
		it.State, it.Note, it.ReadySHA = backlog.Done, "merged", ""
		return nil
	})
}

func retryBacklogItem(id int) error {
	return changeBacklogItem(id, func(it *backlog.Item) error {
		if it.State != backlog.NeedsYou {
			return fmt.Errorf("#%d is %s, not waiting for you", id, it.State)
		}
		it.State, it.Note, it.Extra = backlog.Active, "retrying", it.Extra+1
		// A failure seen before counts as new: re-run its jobs again first.
		it.Reruns, it.Updates = nil, nil
		return nil
	})
}

func runBacklogLs(cmd *cobra.Command, args []string) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	q, err := loadBacklog()
	if err != nil {
		return err
	}
	var items []*backlog.Item
	for _, it := range q.Sorted() {
		if backlogAll || it.State != backlog.Done && it.State != backlog.Cancelled {
			items = append(items, it)
		}
	}
	if format == "json" {
		if items == nil {
			items = []*backlog.Item{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "The backlog is empty: boxctl backlog add --repo owner/name \"a goal\".")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tPRI\tREPO\tSTATE\tRUN\tPR\tNOTE\tGOAL")
	for _, it := range items {
		_, _ = fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.Priority, it.Repo, it.State, backlogRunText(it), backlogPRText(it), truncate(orDash(it.Note), 50), truncate(firstLine(it.Goal), 50))
	}
	return w.Flush()
}

// backlogRunText is an item's RUN cell: its current run's kind and box, or
// how many it had.
func backlogRunText(it *backlog.Item) string {
	if r := it.Current(); r != nil {
		return string(r.Kind) + " " + r.Box
	}
	if len(it.Runs) == 0 {
		return "-"
	}
	return fmt.Sprintf("%d run(s)", len(it.Runs))
}

func backlogPRText(it *backlog.Item) string {
	switch {
	case it.PR == 0:
		return "-"
	case it.ReadySHA != "":
		return fmt.Sprintf("#%d ready", it.PR)
	}
	return fmt.Sprintf("#%d", it.PR)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func readAllLimited(f *os.File, max int64) (string, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for int64(len(buf)) <= max {
		n, err := f.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	if int64(len(buf)) > max {
		return "", fmt.Errorf("a goal over %d bytes", max)
	}
	return string(buf), nil
}

// backlogCtx is a context for one quick backlog call from the tui.
func backlogCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Minute)
}
