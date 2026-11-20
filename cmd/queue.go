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

	"github.com/boxctl-io/boxctl-cli/internal/queue"
	"github.com/spf13/cobra"
)

// boxctl queue: goals waiting to become pull requests, worked on by
// unattended `boxctl claude --goal` runs, a few at a time (internal/queue
// has the rules). The tui's :queue view runs the passes while it's open;
// `boxctl queue run` runs them with no screen.

var queueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Queue goals for Claude to turn into pull requests, a few at a time",
	Long: `A queue of goals, each ending in one pull request. Each one becomes an
unattended boxctl claude --goal run on a box of its own, a few at a time
(boxctl queue limit), and its pull request is then watched until you merge it:

  - CI fails: the failed jobs are re-run once; if they fail again, a
    follow-up run fixes it on the pull request's branch (claude --pr).
  - It conflicts with its base: a follow-up run merges the base in.
  - It's behind its base: GitHub is asked to update the branch, and it's
    ready again only once CI passes on that.
  - Green and up to date: "ready to merge" -- boxctl queue merge, or m in
    the tui. Nothing is merged without you.

Each kind of run gets 2 attempts (boxctl queue retry adds one); after that
the item needs you. A run's box is deleted once the next run starts or the
pull request is merged.

The passes run while boxctl tui is open (:queue shows the queue) or
boxctl queue run is, never both. Nothing moves while neither is running;
runs already on boxes carry on.

  boxctl queue add --repo owner/name "bump Go and fix what breaks"
  boxctl queue                     # list it
  boxctl queue merge 3             # merge #3's pull request
  boxctl queue run                 # run the passes here, no screen`,
	Args: cobra.NoArgs,
	RunE: runQueueLs,
}

var queueLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List the queue",
	Args:    cobra.NoArgs,
	RunE:    runQueueLs,
}

var (
	queueRepo     string
	queuePriority int
	queueModel    string
	queueMethod   string
	queueGlobal   int
	queuePerRepo  int
	queueAll      bool
)

var queueAddCmd = &cobra.Command{
	Use:   "add [--repo owner/name] <goal...>",
	Short: "Queue a goal (- reads it from stdin)",
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
		repo := queueRepo
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
		it, err := addQueueItem(owner+"/"+name, goal, queueModel, queuePriority)
		if err != nil {
			return err
		}
		fmt.Printf("Queued #%d on %s.\n", it.ID, it.Repo)
		if q, err := loadQueue(); err == nil && (q.Loop == "" || time.Since(q.LoopSeen) > queue.LoopStale) {
			fmt.Fprintln(os.Stderr, "Nothing is running the queue: open boxctl tui (:queue) or run boxctl queue run.")
		}
		return nil
	},
}

var queueRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the queue's passes here until interrupted, with no screen",
	Long: `Runs a pass every 30s -- start runs, follow them, watch their pull
requests -- printing what each one does, until Ctrl-C. The tui does the
same while it's open; only one of them runs the passes at a time.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		r, err := newQueueRunner(newClient())
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
			case <-time.After(queuePassEvery):
			}
		}
	},
}

var queueMergeCmd = &cobra.Command{
	Use:   "merge <id>",
	Short: "Merge an item's pull request, at the commit CI passed on",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := queueID(args[0])
		if err != nil {
			return err
		}
		q, err := loadQueue()
		if err != nil {
			return err
		}
		it := q.Item(id)
		if it == nil {
			return fmt.Errorf("no queue item #%d", id)
		}
		if err := mergeQueueItem(cmd.Context(), it, queueMethod); err != nil {
			return err
		}
		fmt.Printf("Merged %s#%d. The next pass marks #%d done and deletes its box.\n", it.Repo, it.PR, id)
		return nil
	},
}

var queueCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Stop working on an item (its pull request and branch stay)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := queueID(args[0])
		if err != nil {
			return err
		}
		if err := cancelQueueItem(id); err != nil {
			return err
		}
		fmt.Printf("Cancelled #%d; the next pass deletes its boxes.\n", id)
		return nil
	},
}

var queueRetryCmd = &cobra.Command{
	Use:   "retry <id>",
	Short: "Give an item that needs you one more run of each kind",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := queueID(args[0])
		if err != nil {
			return err
		}
		if err := retryQueueItem(id); err != nil {
			return err
		}
		fmt.Printf("#%d is active again.\n", id)
		return nil
	},
}

var queueLimitCmd = &cobra.Command{
	Use:   "limit",
	Short: "Show or set how many runs go at once",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		var global, perRepo *int
		if cmd.Flags().Changed("global") {
			global = &queueGlobal
		}
		if cmd.Flags().Changed("per-repo") {
			perRepo = &queuePerRepo
		}
		l, err := setQueueLimits(global, perRepo)
		if err != nil {
			return err
		}
		fmt.Printf("At most %d runs at once, %d per repository.\n", l.Global, l.PerRepo)
		return nil
	},
}

func init() {
	addOutputFlag(queueCmd)
	addOutputFlag(queueLsCmd)
	for _, c := range []*cobra.Command{queueCmd, queueLsCmd} {
		c.Flags().BoolVarP(&queueAll, "all", "a", false, "also list done and cancelled items")
	}
	queueAddCmd.Flags().StringVarP(&queueRepo, "repo", "R", "", "GitHub repository (owner/name; default: this directory's origin)")
	queueAddCmd.Flags().IntVarP(&queuePriority, "priority", "p", 0, "higher starts first")
	queueAddCmd.Flags().StringVar(&queueModel, "model", "", "model for its runs (claude --model)")
	queueMergeCmd.Flags().StringVar(&queueMethod, "method", "squash", "merge, squash or rebase")
	queueLimitCmd.Flags().IntVar(&queueGlobal, "global", 0, "runs at once in all")
	queueLimitCmd.Flags().IntVar(&queuePerRepo, "per-repo", 0, "runs at once on one repository")
	queueCmd.AddCommand(queueLsCmd, queueAddCmd, queueRunCmd, queueMergeCmd, queueCancelCmd, queueRetryCmd, queueLimitCmd)
	rootCmd.AddCommand(queueCmd)
}

func mustQueuePath() string {
	p, err := queue.Path()
	if err != nil {
		return "queue.json"
	}
	return p
}

func loadQueue() (*queue.Queue, error) { return queue.Load(mustQueuePath()) }

func queueID(s string) (int, error) {
	id, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%q: use an item's number, as boxctl queue lists it", s)
	}
	return id, nil
}

func addQueueItem(repo, goal, model string, priority int) (*queue.Item, error) {
	var it *queue.Item
	err := queue.Update(mustQueuePath(), func(q *queue.Queue) error {
		it = q.Add(repo, goal, model, priority, time.Now())
		return nil
	})
	return it, err
}

// changeQueueItem applies fn to item id under the queue's lock.
// setQueueLimits sets the limits given (nil leaves one as it is) and
// returns them all: `queue limit` and :queue's L.
func setQueueLimits(global, perRepo *int) (queue.Limits, error) {
	var l queue.Limits
	err := queue.Update(mustQueuePath(), func(q *queue.Queue) error {
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

func changeQueueItem(id int, fn func(it *queue.Item) error) error {
	return queue.Update(mustQueuePath(), func(q *queue.Queue) error {
		it := q.Item(id)
		if it == nil {
			return fmt.Errorf("no queue item #%d", id)
		}
		if err := fn(it); err != nil {
			return err
		}
		it.Updated = time.Now()
		return nil
	})
}

func cancelQueueItem(id int) error {
	return changeQueueItem(id, func(it *queue.Item) error {
		if it.State == queue.Done || it.State == queue.Cancelled {
			return fmt.Errorf("#%d is %s already", id, it.State)
		}
		it.State, it.Note = queue.Cancelled, "cancelled"
		if r := it.Current(); r != nil {
			now := time.Now()
			r.Ended, r.Outcome, r.Error = &now, queue.OutcomeLost, "cancelled"
		}
		return nil
	})
}

func retryQueueItem(id int) error {
	return changeQueueItem(id, func(it *queue.Item) error {
		if it.State != queue.NeedsYou {
			return fmt.Errorf("#%d is %s, not waiting for you", id, it.State)
		}
		it.State, it.Note, it.Extra = queue.Active, "retrying", it.Extra+1
		// A failure seen before counts as new: re-run its jobs again first.
		it.Reruns, it.Updates = nil, nil
		return nil
	})
}

func runQueueLs(cmd *cobra.Command, args []string) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	q, err := loadQueue()
	if err != nil {
		return err
	}
	var items []*queue.Item
	for _, it := range q.Sorted() {
		if queueAll || it.State != queue.Done && it.State != queue.Cancelled {
			items = append(items, it)
		}
	}
	if format == "json" {
		if items == nil {
			items = []*queue.Item{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "The queue is empty: boxctl queue add --repo owner/name \"a goal\".")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tPRI\tREPO\tSTATE\tRUN\tPR\tNOTE\tGOAL")
	for _, it := range items {
		_, _ = fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.Priority, it.Repo, it.State, queueRunText(it), queuePRText(it), truncate(orDash(it.Note), 50), truncate(firstLine(it.Goal), 50))
	}
	return w.Flush()
}

// queueRunText is an item's RUN cell: its current run's kind and box, or
// how many it had.
func queueRunText(it *queue.Item) string {
	if r := it.Current(); r != nil {
		return string(r.Kind) + " " + r.Box
	}
	if len(it.Runs) == 0 {
		return "-"
	}
	return fmt.Sprintf("%d run(s)", len(it.Runs))
}

func queuePRText(it *queue.Item) string {
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

// queueCtx is a context for one quick queue call from the tui.
func queueCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Minute)
}
