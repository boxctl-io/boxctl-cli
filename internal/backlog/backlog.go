// Package backlog is the work backlog behind `boxctl tasks` and the tui's
// :tasks view: goals waiting to become pull requests, each worked on by
// one unattended `boxctl claude --goal` run at a time, with follow-up
// runs on the pull request's branch (`--pr`) when its CI fails or it
// conflicts with its base. It's the boxctl-orchestrator design
// (boxctl-orchestrator's docs/designs/boxctl_orchestrator.md) built into
// the CLI on top of what goal runs already do.
//
// The backlog file (~/.boxctl/backlog.json) holds only what boxctl and
// GitHub don't know: the goals, their order, which runs belong to which
// item, and what was already tried. Everything else -- how a run is
// going (the box's run_status), the pull request's state, its checks --
// is asked for again on every pass. Decide (engine.go) is the pure part:
// the backlog and one pass's observations in, the state changes made and
// the actions to take out.
package backlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Kind is why a run was started.
type Kind string

const (
	Code        Kind = "code"         // the first attempt, and retries of it: a new branch, ending in a PR
	CIFix       Kind = "ci_fix"       // on the PR's branch: make its checks pass
	ConflictFix Kind = "conflict_fix" // on the PR's branch: merge its base in and resolve
)

// State is an item's.
type State string

const (
	Queued    State = "queued"    // waiting for a slot
	Active    State = "active"    // a run is going, or its PR is being watched
	NeedsYou  State = "needs_you" // out of attempts, or something only you can decide
	Done      State = "done"      // its PR was merged
	Cancelled State = "cancelled" // you called it off
)

// Outcomes of a run.
const (
	OutcomeDone   = "done"   // the runner said done (a PR exists)
	OutcomeFailed = "failed" // the runner said failed, or the launch failed
	OutcomeLost   = "lost"   // the box went, or stopped, without a final status: not charged
	OutcomeKilled = "killed" // you killed it: charged, and the item waits for you
)

// DefaultBudget is how many runs of each kind an item gets before it
// needs you; `retry` adds one of each.
const DefaultBudget = 2

// MaxLost is how many runs in a row may be lost (not the agent's fault,
// so not charged) before the item needs you anyway.
const MaxLost = 3

// RunTimeLimit is how long a run may report "running" before it's
// called failed and its box removed.
const RunTimeLimit = 3 * time.Hour

// LaunchGrace is how long after its start a run's box may be missing, or
// silent, before the run counts as lost: the launch (`boxctl claude`
// creating the box, cloning, installing Claude) takes a few minutes.
const LaunchGrace = 20 * time.Minute

// Run is one `boxctl claude --goal` on one box.
type Run struct {
	Kind     Kind       `json:"kind"`
	Box      string     `json:"box"`
	Started  time.Time  `json:"started"`
	Launched bool       `json:"launched,omitempty"` // boxctl claude returned with Claude started
	Ended    *time.Time `json:"ended,omitempty"`
	Outcome  string     `json:"outcome,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// Item is one goal: one repository, one pull request in the end.
type Item struct {
	ID       int    `json:"id"`
	Repo     string `json:"repo"`
	Goal     string `json:"goal"`
	Priority int    `json:"priority,omitempty"`
	Model    string `json:"model,omitempty"`
	State    State  `json:"state"`
	Note     string `json:"note,omitempty"` // why it needs you, or what it's waiting for
	Branch   string `json:"branch,omitempty"`
	PR       int    `json:"pr,omitempty"`
	Runs     []Run  `json:"runs,omitempty"`
	Extra    int    `json:"extra_budget,omitempty"` // added to DefaultBudget for each kind by retry
	// Reruns are the head commits whose failed jobs were re-run once;
	// Updates those GitHub was asked to bring up to date with the base.
	Reruns   []string  `json:"reruns,omitempty"`
	Updates  []string  `json:"updates,omitempty"`
	ReadySHA string    `json:"ready_sha,omitempty"` // the head CI passed on, up to date: merge at this
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// Current is the item's run that hasn't ended, nil when there's none.
func (it *Item) Current() *Run {
	if n := len(it.Runs); n > 0 && it.Runs[n-1].Ended == nil {
		return &it.Runs[n-1]
	}
	return nil
}

// Last is the item's latest run, nil before the first.
func (it *Item) Last() *Run {
	if n := len(it.Runs); n > 0 {
		return &it.Runs[n-1]
	}
	return nil
}

// Used is how many runs of kind count against its budget: every one
// that wasn't lost.
func (it *Item) Used(kind Kind) int {
	n := 0
	for _, r := range it.Runs {
		if r.Kind == kind && r.Ended != nil && r.Outcome != OutcomeLost {
			n++
		}
	}
	return n
}

// Kill ends the item's current run, which must be on box, as killed and
// leaves the item to you: nothing more runs for it until a retry. seen is
// what the box last reported: a pull request it opened is linked here, as
// a pass would, since the box (the only record of it) is about to go. The
// caller deletes the box after saving this; the passes delete it too
// while it's still there, should that fail.
func Kill(it *Item, box string, seen Box, now time.Time) error {
	r := it.Current()
	if r == nil || r.Box != box {
		return fmt.Errorf("#%d has no run going on %s", it.ID, box)
	}
	if seen.Branch != "" {
		it.Branch = seen.Branch
	}
	if n := prNumber(seen.PRURL); n > 0 && it.PR == 0 {
		it.PR = n
	}
	r.Ended, r.Outcome = &now, OutcomeKilled
	setState(it, NeedsYou, fmt.Sprintf("run killed (%s on %s)", r.Kind, box), now)
	return nil
}

// Budget is how many runs of each kind it may have.
func (it *Item) Budget() int { return DefaultBudget + it.Extra }

// lostStreak is how many of its latest runs in a row were lost.
func (it *Item) lostStreak() int {
	n := 0
	for i := len(it.Runs) - 1; i >= 0 && it.Runs[i].Outcome == OutcomeLost; i-- {
		n++
	}
	return n
}

// Limits cap how many runs are going at once.
type Limits struct {
	Global  int `json:"global"`
	PerRepo int `json:"per_repo"`
}

// DefaultLimits are a new backlog's: an account has 2 active boxes unless
// its limit was raised, and a run's box is one of them.
var DefaultLimits = Limits{Global: 2, PerRepo: 2}

// Backlog is the file.
type Backlog struct {
	Next   int     `json:"next"`
	Items  []*Item `json:"items"`
	Limits Limits  `json:"limits"`
	// Backoff holds off new runs after boxctl refused a box (the account's
	// limit, or no room on the fleet), until it passes.
	Backoff    time.Time `json:"backoff,omitempty"`
	BackoffWhy string    `json:"backoff_why,omitempty"`
	// Loop is who runs the passes (a tui or `boxctl tasks dispatch`) and when
	// it last did one, so a second one doesn't run them too.
	Loop     string    `json:"loop,omitempty"`
	LoopSeen time.Time `json:"loop_seen,omitempty"`
}

// Add queues a goal and returns it.
func (q *Backlog) Add(repo, goal, model string, priority int, now time.Time) *Item {
	q.Next++
	it := &Item{ID: q.Next, Repo: repo, Goal: goal, Model: model, Priority: priority, State: Queued, Created: now, Updated: now}
	q.Items = append(q.Items, it)
	return it
}

// Item finds id, nil when there's none.
func (q *Backlog) Item(id int) *Item {
	for _, it := range q.Items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// Sorted is the items in the order they're shown and dispatched: open
// ones first (active, needs you, queued), highest priority, oldest.
func (q *Backlog) Sorted() []*Item {
	items := append([]*Item(nil), q.Items...)
	rank := map[State]int{Active: 0, NeedsYou: 1, Queued: 2, Done: 3, Cancelled: 4}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if rank[a.State] != rank[b.State] {
			return rank[a.State] < rank[b.State]
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.ID < b.ID
	})
	return items
}

// --- the file ---------------------------------------------------------------

// Path is ~/.boxctl/backlog.json.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".boxctl", "backlog.json"), nil
}

// Load reads the backlog at path; an empty one when there's no file yet.
func Load(path string) (*Backlog, error) {
	q := &Backlog{Limits: DefaultLimits}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, q); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if q.Limits.Global < 1 {
		q.Limits.Global = DefaultLimits.Global
	}
	if q.Limits.PerRepo < 1 {
		q.Limits.PerRepo = DefaultLimits.PerRepo
	}
	return q, nil
}

// save writes q to path atomically (a temporary file, then a rename).
func save(path string, q *Backlog) error {
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lockStale is when a lock file is taken to be left over from a process
// that died holding it: Update holds it for milliseconds.
const lockStale = 30 * time.Second

// Update loads the backlog, lets fn change it and saves it, holding a lock
// file (path.lock) throughout, so a tui, `boxctl tasks dispatch` and `boxctl
// backlog add` never write over each other's changes. fn returning an
// error saves nothing.
func Update(path string, fn func(q *Backlog) error) error {
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > lockStale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the backlog is locked (%s); remove it if no boxctl is running", lock)
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer func() { _ = os.Remove(lock) }()
	q, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(q); err != nil {
		return err
	}
	return save(path, q)
}

// LoopStale is how long a loop may go without a pass before another may
// take over: a tui closed, a laptop asleep.
const LoopStale = 2 * time.Minute

// Claim makes owner the backlog's loop, unless another one ran a pass
// within LoopStale; it returns who holds it then.
func (q *Backlog) Claim(owner string, now time.Time) (holder string, ok bool) {
	if q.Loop != "" && q.Loop != owner && now.Sub(q.LoopSeen) < LoopStale {
		return q.Loop, false
	}
	q.Loop, q.LoopSeen = owner, now
	return owner, true
}

// Release gives the loop up, so the next one needn't wait LoopStale.
func (q *Backlog) Release(owner string) {
	if q.Loop == owner {
		q.Loop, q.LoopSeen = "", time.Time{}
	}
}
