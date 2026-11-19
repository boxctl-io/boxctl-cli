package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	"github.com/boxctl-io/boxctl-cli/internal/queue"
)

// The queue's loop: what a pass observes and what it does about it. The
// decisions are internal/queue's (Decide); this is the I/O around them,
// shared by the tui (a pass every queuePassEvery while it's open) and
// `boxctl queue run` (the same, with no screen). A run is `boxctl claude
// --repo R --box <name> --goal "..."` (plus --pr N for a follow-up) run as
// a child process, its output kept in ~/.boxctl/queue/logs/<box>.log.

// queuePassEvery paces the passes.
const queuePassEvery = 30 * time.Second

// queueBackoff is how long new runs wait after boxctl refused a box.
const queueBackoff = 2 * time.Minute

type queueRunner struct {
	path  string
	c     *client.Client
	owner string // who this loop is, in the queue file's Loop
	exe   string // this boxctl, to run `boxctl claude` with

	mu        sync.Mutex
	launching map[string]bool // boxes whose boxctl claude is running
	wg        sync.WaitGroup
}

func newQueueRunner(c *client.Client) (*queueRunner, error) {
	path, err := queue.Path()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("finding boxctl: %w", err)
	}
	host, _ := os.Hostname()
	return &queueRunner{path: path, c: c, exe: exe, owner: fmt.Sprintf("%s:%d", host, os.Getpid()), launching: map[string]bool{}}, nil
}

// errLoopHeld: another tui or `queue run` is running the passes.
type errLoopHeld struct{ holder string }

func (e errLoopHeld) Error() string {
	return "another boxctl (" + e.holder + ") is running the queue"
}

// queueEvent is something a pass did or failed to do, for the tui's
// flash or `queue run`'s output.
type queueEvent struct {
	text string
	err  bool
}

// pass observes, decides and acts once. It returns what it did.
func (r *queueRunner) pass(ctx context.Context) ([]queueEvent, error) {
	q, err := queue.Load(r.path)
	if err != nil {
		return nil, err
	}
	if !hasOpenItems(q) {
		return nil, nil
	}
	now := time.Now()
	obs := queue.Obs{Now: now, PRs: map[int]*queue.PR{}}
	var events []queueEvent
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	vms, lerr := r.c.List(listCtx)
	cancel()
	if lerr != nil {
		events = append(events, queueEvent{"listing boxes: " + lerr.Error(), true})
	} else {
		obs.Boxes = queueBoxes(vms)
	}
	// Each active item's PR, 4 at a time.
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, it := range q.Items {
		if it.State != queue.Active || it.PR == 0 || it.Current() != nil {
			continue
		}
		wg.Add(1)
		go func(id int, repo string, n int, branch string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pr, err := observePR(ctx, repo, n, branch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				events = append(events, queueEvent{fmt.Sprintf("%s#%d: %v", repo, n, err), true})
				return
			}
			obs.PRs[id] = pr
		}(it.ID, it.Repo, it.PR, it.Branch)
	}
	wg.Wait()

	var acts []queue.Action
	err = queue.Update(r.path, func(q *queue.Queue) error {
		if holder, ok := q.Claim(r.owner, now); !ok {
			return errLoopHeld{holder}
		}
		r.mu.Lock()
		obs.Launching = maps.Clone(r.launching)
		r.mu.Unlock()
		acts = queue.Decide(q, obs)
		return nil
	})
	if err != nil {
		return events, err
	}
	for _, a := range acts {
		if e := r.do(ctx, a); e.text != "" {
			events = append(events, e)
		}
	}
	return events, nil
}

func hasOpenItems(q *queue.Queue) bool {
	return slices.ContainsFunc(q.Items, func(it *queue.Item) bool {
		return it.State == queue.Queued || it.State == queue.Active
	})
}

// inFlight is how many launches are still running.
func (r *queueRunner) inFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.launching)
}

// release gives the loop up (on exit), once launches still running
// have recorded how they went.
func (r *queueRunner) release() {
	r.wg.Wait()
	if _, err := os.Stat(r.path); err != nil {
		return // never used: don't make the file
	}
	_ = queue.Update(r.path, func(q *queue.Queue) error { q.Release(r.owner); return nil })
}

// queueBoxes is what the engine needs of every box.
func queueBoxes(vms []client.VM) map[string]queue.Box {
	boxes := make(map[string]queue.Box, len(vms))
	for _, vm := range vms {
		b := queue.Box{State: vm.State}
		if st, err := parseGoalStatus(vm.RunStatus); err == nil && st != nil {
			b.Status, b.PRURL, b.Branch, b.Error = st.Status, st.PRURL, st.Branch, st.Error
		}
		if b.Branch == "" {
			b.Branch = vm.Labels["boxctl.branch"]
		}
		boxes[vm.Name] = b
	}
	return boxes
}

// do carries one action out.
func (r *queueRunner) do(ctx context.Context, a queue.Action) queueEvent {
	switch a.Kind {
	case queue.Launch:
		r.launch(a)
		return queueEvent{text: fmt.Sprintf("#%d: starting a %s run on %s", a.Item, a.Run, a.Box)}
	case queue.RemoveBox:
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := r.c.Destroy(ctx, a.Box); err != nil && !strings.Contains(err.Error(), "not found") {
			return queueEvent{fmt.Sprintf("#%d: deleting %s: %v", a.Item, a.Box, err), true}
		}
		return queueEvent{text: fmt.Sprintf("#%d: deleted %s", a.Item, a.Box)}
	case queue.Rerun:
		for _, id := range a.RunIDs {
			if _, err := ghRun(ctx, "run", "rerun", strconv.FormatInt(id, 10), "--failed", "-R", a.Repo); err != nil {
				return queueEvent{fmt.Sprintf("#%d: re-running %s's failed jobs: %v", a.Item, a.Repo, err), true}
			}
		}
		return queueEvent{text: fmt.Sprintf("#%d: re-running the failed jobs of %s#%d", a.Item, a.Repo, a.PR)}
	case queue.UpdateBranch:
		if _, err := ghRun(ctx, "api", "-X", "PUT", fmt.Sprintf("repos/%s/pulls/%d/update-branch", a.Repo, a.PR), "-f", "expected_head_sha="+a.SHA); err != nil {
			return queueEvent{fmt.Sprintf("#%d: updating %s#%d's branch: %v", a.Item, a.Repo, a.PR, err), true}
		}
		return queueEvent{text: fmt.Sprintf("#%d: updating %s#%d's branch with its base", a.Item, a.Repo, a.PR)}
	}
	return queueEvent{}
}

// queueLaunchArgs is the boxctl command line of a run, less the binary.
func queueLaunchArgs(a queue.Action) []string {
	args := []string{"claude", "--repo", a.Repo, "--box", a.Box, "--queue-item", strconv.Itoa(a.Item), "--goal", a.Goal}
	if a.Run != queue.Code && a.PR != 0 {
		args = append(args, "--pr", strconv.Itoa(a.PR))
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if apiURLFlag != "" {
		args = append(args, "--api-url", apiURLFlag)
	}
	return args
}

// queueLogPath is where a run's boxctl claude output goes.
func queueLogPath(queuePath, box string) string {
	return filepath.Join(filepath.Dir(queuePath), "queue", "logs", box+".log")
}

// launchCommand runs a launch; a variable so tests stand in for boxctl
// claude. It returns the end of its output, for a failure's reason.
var launchCommand = func(exe string, args []string, logPath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return "", err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = log.Close() }()
	tail := &tailBuffer{max: 4096}
	c := exec.Command(exe, args...)
	c.Stdout, c.Stderr = log, io.MultiWriter(log, tail)
	err = c.Run()
	return tail.String(), err
}

// launch starts a run's boxctl claude in the background and records how
// it went when it returns.
func (r *queueRunner) launch(a queue.Action) {
	r.mu.Lock()
	r.launching[a.Box] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		out, err := launchCommand(r.exe, queueLaunchArgs(a), queueLogPath(r.path, a.Box))
		_ = queue.Update(r.path, func(q *queue.Queue) error {
			it := q.Item(a.Item)
			if it == nil {
				return nil
			}
			run := it.Current()
			if run == nil || run.Box != a.Box {
				return nil
			}
			if err == nil {
				run.Launched = true
				return nil
			}
			why := claudeFailure(out, err)
			now := time.Now()
			if isRefusal(why) {
				// boxctl said no box now: not the run's fault, so it never was.
				it.Runs = it.Runs[:len(it.Runs)-1]
				if len(it.Runs) == 0 && it.PR == 0 {
					it.State = queue.Queued
				}
				q.Backoff, q.BackoffWhy = now.Add(queueBackoff), why
				it.Note = "boxctl refused a box: " + why
				return nil
			}
			run.Ended, run.Outcome, run.Error = &now, queue.OutcomeFailed, "starting it: "+why
			return nil
		})
		r.mu.Lock()
		delete(r.launching, a.Box)
		r.mu.Unlock()
	}()
}

// isRefusal: boxctl's words for a refused box (internal/client's
// refusalMessage).
func isRefusal(msg string) bool {
	return strings.Contains(msg, "active boxes") || strings.Contains(msg, "at capacity")
}

// ghRun runs gh with args, with gh's own reason when it fails.
var ghRun = func(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var stderr strings.Builder
	c := exec.CommandContext(ctx, "gh", args...)
	c.Stderr = &stderr
	out, err := c.Output()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, errors.New("gh isn't installed")
	case err != nil:
		if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
			return out, errors.New(line)
		}
		return out, err
	}
	return out, nil
}

const prQuery = `query($owner:String!,$name:String!,$n:Int!,$head:String!){repository(owner:$owner,name:$name){pullRequest(number:$n){
state mergeable headRefOid baseRefName
baseRef{compare(headRef:$head){behindBy}}
commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:100){nodes{
... on CheckRun{name conclusion checkSuite{workflowRun{databaseId}}}
... on StatusContext{context state}
}}}}}}}}}`

// observePR asks GitHub how pull request n of repo is: one GraphQL query.
func observePR(ctx context.Context, repo string, n int, branch string) (*queue.PR, error) {
	owner, name, _ := strings.Cut(repo, "/")
	out, err := ghRun(ctx, "api", "graphql", "-f", "query="+prQuery, "-f", "owner="+owner, "-f", "name="+name, "-F", "n="+strconv.Itoa(n), "-f", "head="+branch)
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return parsePR(out)
}

// parsePR reads observePR's answer.
func parsePR(out []byte) (*queue.PR, error) {
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					State       string `json:"state"`
					Mergeable   string `json:"mergeable"`
					HeadRefOid  string `json:"headRefOid"`
					BaseRefName string `json:"baseRefName"`
					BaseRef     *struct {
						Compare *struct {
							BehindBy int `json:"behindBy"`
						} `json:"compare"`
					} `json:"baseRef"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								StatusCheckRollup *struct {
									State    string `json:"state"`
									Contexts struct {
										Nodes []struct {
											Name       string `json:"name"`
											Conclusion string `json:"conclusion"`
											CheckSuite *struct {
												WorkflowRun *struct {
													DatabaseID int64 `json:"databaseId"`
												} `json:"workflowRun"`
											} `json:"checkSuite"`
											Context string `json:"context"`
											State   string `json:"state"`
										} `json:"nodes"`
									} `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("reading GitHub's answer: %w", err)
	}
	p := resp.Data.Repository.PullRequest
	if p == nil {
		if len(resp.Errors) > 0 {
			return nil, errors.New(resp.Errors[0].Message)
		}
		return nil, errors.New("no such pull request")
	}
	pr := &queue.PR{State: p.State, Mergeable: p.Mergeable, Head: p.HeadRefOid, Base: p.BaseRefName, Checks: "none"}
	if p.BaseRef != nil && p.BaseRef.Compare != nil {
		pr.BehindBy = p.BaseRef.Compare.BehindBy
	}
	if len(p.Commits.Nodes) > 0 {
		if roll := p.Commits.Nodes[0].Commit.StatusCheckRollup; roll != nil {
			switch roll.State {
			case "SUCCESS":
				pr.Checks = "success"
			case "FAILURE", "ERROR":
				pr.Checks = "failure"
			default: // PENDING, EXPECTED
				pr.Checks = "pending"
			}
			for _, c := range roll.Contexts.Nodes {
				switch {
				case c.Name != "" && slices.Contains([]string{"FAILURE", "TIMED_OUT", "CANCELLED", "STARTUP_FAILURE", "ACTION_REQUIRED"}, c.Conclusion):
					pr.Failed = append(pr.Failed, c.Name)
					if c.CheckSuite != nil && c.CheckSuite.WorkflowRun != nil {
						if id := c.CheckSuite.WorkflowRun.DatabaseID; !slices.Contains(pr.FailedRunIDs, id) {
							pr.FailedRunIDs = append(pr.FailedRunIDs, id)
						}
					}
				case c.Context != "" && (c.State == "FAILURE" || c.State == "ERROR"):
					pr.Failed = append(pr.Failed, c.Context)
				}
			}
		}
	}
	return pr, nil
}

// mergeQueueItem merges it's pull request at the head CI passed on.
func mergeQueueItem(ctx context.Context, it *queue.Item, method string) error {
	if it.PR == 0 {
		return fmt.Errorf("#%d has no pull request yet", it.ID)
	}
	if it.ReadySHA == "" {
		return fmt.Errorf("#%d isn't ready to merge (%s)", it.ID, orDash(it.Note))
	}
	_, err := ghRun(ctx, "pr", "merge", strconv.Itoa(it.PR), "--repo", it.Repo, "--"+method, "--match-head-commit", it.ReadySHA)
	return err
}
