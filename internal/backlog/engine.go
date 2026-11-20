package backlog

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Box is what a pass saw of a run's box: its state and the run_status
// it reported (status.json: running, done or failed; "" for none yet).
type Box struct {
	State  string
	Status string
	PRURL  string
	Branch string
	Error  string
}

// PR is what a pass saw of an item's pull request.
type PR struct {
	State     string // OPEN, MERGED, CLOSED
	Mergeable string // MERGEABLE, CONFLICTING, UNKNOWN
	Head      string // the head commit
	Base      string // the base branch
	Checks    string // none, pending, success, failure (the head's check rollup)
	Failed    []string
	// FailedRunIDs are the GitHub Actions runs among the failed checks:
	// what `gh run rerun --failed` can re-run.
	FailedRunIDs []int64
	BehindBy     int // commits on the base the head doesn't have
}

// Obs is one pass's observations.
type Obs struct {
	Now time.Time
	// Boxes is every box on the account by name; nil when they couldn't
	// be listed this pass, which leaves runs as they are.
	Boxes map[string]Box
	// PRs is the pull request of each item that has one, by item ID; an
	// item missing here wasn't observed this pass and is left alone.
	PRs map[int]*PR
	// Launching is the boxes whose `boxctl claude` is still running in
	// this process.
	Launching map[string]bool
}

// ActionKind is what an Action does.
type ActionKind string

const (
	Launch       ActionKind = "launch"        // start a run: boxctl claude --goal on Box
	RemoveBox    ActionKind = "remove_box"    // delete Box
	Rerun        ActionKind = "rerun"         // gh run rerun --failed, each of RunIDs
	UpdateBranch ActionKind = "update_branch" // bring PR's branch up to date with its base, expecting SHA
)

// Action is a side effect Decide asks for; the backlog's own state
// changes it has already made.
type Action struct {
	Kind   ActionKind
	Item   int
	Repo   string
	Box    string
	Run    Kind   // Launch
	PR     int    // Launch of a follow-up, Rerun, UpdateBranch
	Goal   string // Launch: what Claude is told
	Model  string // Launch
	SHA    string // UpdateBranch
	RunIDs []int64
}

// FreshChecks is how long after a run ended a PR with no checks at all
// is still taken to be waiting for them, not to have none: GitHub takes
// a moment to start CI on a new push.
const FreshChecks = 3 * time.Minute

// Decide is one pass: it updates q from o -- runs ended, PRs linked,
// items done or needing you, new runs planned -- and returns what to do
// about it, in order.
func Decide(q *Backlog, o Obs) []Action {
	var acts []Action
	now := o.Now
	// 1. Follow each run.
	if o.Boxes != nil {
		for _, it := range q.Items {
			// A run still launching here is left to its launch -- unless
			// its box already reports the run: Claude started, whether or
			// not boxctl claude has returned yet.
			if r := it.Current(); r != nil && (!o.Launching[r.Box] || o.Boxes[r.Box].Status != "") {
				acts = append(acts, followRun(it, r, o)...)
			}
		}
	}
	// 2. React to each active item with no run going.
	type want struct {
		it   *Item
		kind Kind
		pr   *PR
	}
	var wants []want
	for _, it := range q.Sorted() {
		if it.State != Active || it.Current() != nil {
			continue
		}
		kind, pr, more := react(it, o)
		acts = append(acts, more...)
		if kind != "" {
			wants = append(wants, want{it, kind, pr})
		}
	}
	for _, it := range q.Sorted() {
		if it.State == Queued {
			wants = append(wants, want{it, Code, nil})
		}
	}
	// 3. Done and cancelled items keep no boxes, nor does a killed run,
	// whose box may have outlived the kill (deleting it then failed).
	if o.Boxes != nil {
		for _, it := range q.Items {
			if it.State == Done || it.State == Cancelled {
				acts = append(acts, removeBoxes(it, o, "")...)
				continue
			}
			for _, r := range it.Runs {
				if _, ok := o.Boxes[r.Box]; ok && r.Outcome == OutcomeKilled {
					acts = append(acts, Action{Kind: RemoveBox, Item: it.ID, Box: r.Box})
				}
			}
		}
	}
	// 4. Start what there's room for: follow-ups first, then the backlog.
	active, perRepo := 0, map[string]int{}
	for _, it := range q.Items {
		if it.Current() != nil {
			active++
			perRepo[it.Repo]++
		}
	}
	for _, w := range wants {
		it := w.it
		switch {
		case o.Boxes == nil:
			continue // the box names can't be checked
		case now.Before(q.Backoff):
			setNote(it, fmt.Sprintf("waiting: boxctl refused a box, retrying at %s", q.Backoff.Local().Format("15:04")), now)
			continue
		case active >= q.Limits.Global:
			setNote(it, fmt.Sprintf("waiting for a slot (%d/%d running)", active, q.Limits.Global), now)
			continue
		case perRepo[it.Repo] >= q.Limits.PerRepo:
			setNote(it, fmt.Sprintf("waiting for a slot on %s (%d/%d)", it.Repo, perRepo[it.Repo], q.Limits.PerRepo), now)
			continue
		}
		// The last run's work is on GitHub; its box has nothing more to give.
		acts = append(acts, removeBoxes(it, o, "")...)
		box := boxName(it, w.kind, o.Boxes)
		it.Runs = append(it.Runs, Run{Kind: w.kind, Box: box, Started: now})
		if it.State == Queued {
			setState(it, Active, "", now)
		}
		setNote(it, "starting "+string(w.kind)+" run on "+box, now)
		active++
		perRepo[it.Repo]++
		acts = append(acts, Action{Kind: Launch, Item: it.ID, Repo: it.Repo, Box: box, Run: w.kind, PR: it.PR, Goal: GoalFor(it, w.kind, w.pr), Model: it.Model})
	}
	return acts
}

// followRun ends r when its box says it's over, or that it never will be.
func followRun(it *Item, r *Run, o Obs) []Action {
	now := o.Now
	b, ok := o.Boxes[r.Box]
	end := func(outcome, why string) {
		t := now
		r.Ended, r.Outcome, r.Error = &t, outcome, why
		it.Updated = now
	}
	if ok && b.Status != "" {
		// The box reports the run, so Claude started on it: whatever
		// became of the boxctl claude that launched it (a tui closed while
		// it ran, or one that never returned), follow the box.
		r.Launched = true
	}
	switch {
	case !ok && r.Launched:
		end(OutcomeLost, "its box is gone")
		return nil
	case !ok && now.Sub(r.Started) > LaunchGrace:
		end(OutcomeLost, "the launch never made its box")
		return nil
	case !ok:
		// Not launching in this process, and no box yet: a launch from
		// a tui since closed may still be making it.
		setNote(it, "waiting for "+r.Box+" to appear", now)
		return nil
	}
	if b.Branch != "" {
		it.Branch = b.Branch
	}
	if n := prNumber(b.PRURL); n > 0 && it.PR == 0 {
		it.PR = n
	}
	switch b.Status {
	case "done":
		end(OutcomeDone, "")
		setNote(it, "", now)
	case "failed":
		end(OutcomeFailed, b.Error)
		setNote(it, "run failed: "+b.Error, now)
	case "running":
		if b.State != "running" && b.State != "provisioning" {
			end(OutcomeLost, "its box is "+b.State+" mid-run")
			return []Action{{Kind: RemoveBox, Item: it.ID, Box: r.Box}}
		}
		if now.Sub(r.Started) > RunTimeLimit {
			end(OutcomeFailed, fmt.Sprintf("still running after %s", RunTimeLimit))
			return []Action{{Kind: RemoveBox, Item: it.ID, Box: r.Box}}
		}
		setNote(it, string(r.Kind)+" run going on "+r.Box, now)
	default: // nothing reported
		if b.State != "running" && b.State != "provisioning" || now.Sub(r.Started) > LaunchGrace {
			end(OutcomeLost, "its box never reported how the run went")
			return []Action{{Kind: RemoveBox, Item: it.ID, Box: r.Box}}
		}
	}
	return nil
}

// react is what an active item with no run going needs: a run of a kind
// (with the PR it's for), and/or other actions.
func react(it *Item, o Obs) (Kind, *PR, []Action) {
	now := o.Now
	if it.PR == 0 {
		switch {
		case len(it.Runs) == 0:
			return Code, nil, nil
		case it.lostStreak() >= MaxLost:
			setState(it, NeedsYou, fmt.Sprintf("%d runs in a row were lost: %s", MaxLost, it.Last().Error), now)
		case it.Used(Code) < it.Budget():
			return Code, nil, nil
		default:
			setState(it, NeedsYou, "no pull request after "+strconv.Itoa(it.Used(Code))+" runs: "+orNone(it.Last().Error), now)
		}
		return "", nil, nil
	}
	pr := o.PRs[it.ID]
	if pr == nil {
		return "", nil, nil
	}
	if pr.Head != it.ReadySHA {
		it.ReadySHA = ""
	}
	followUp := func(kind Kind, why string) (Kind, *PR, []Action) {
		switch {
		case it.lostStreak() >= MaxLost:
			setState(it, NeedsYou, fmt.Sprintf("%s, and %d runs in a row were lost", why, MaxLost), now)
		case it.Used(kind) >= it.Budget():
			setState(it, NeedsYou, fmt.Sprintf("%s after %d %s runs", why, it.Used(kind), kind), now)
		default:
			return kind, pr, nil
		}
		return "", nil, nil
	}
	switch {
	case pr.State == "MERGED":
		setState(it, Done, "merged", now)
	case pr.State == "CLOSED":
		setState(it, NeedsYou, fmt.Sprintf("#%d was closed without merging", it.PR), now)
	case pr.Mergeable == "CONFLICTING":
		return followUp(ConflictFix, "conflicts with "+pr.Base)
	case pr.Checks == "failure" && len(pr.FailedRunIDs) > 0 && !slices.Contains(it.Reruns, pr.Head):
		it.Reruns = append(it.Reruns, pr.Head)
		setNote(it, "CI failed; re-running the failed jobs once", now)
		return "", nil, []Action{{Kind: Rerun, Item: it.ID, Repo: it.Repo, PR: it.PR, RunIDs: pr.FailedRunIDs}}
	case pr.Checks == "failure":
		return followUp(CIFix, "CI fails ("+strings.Join(pr.Failed, ", ")+")")
	case pr.Checks == "pending" || pr.Mergeable == "UNKNOWN" || pr.Checks == "none" && justEnded(it, now):
		setNote(it, "waiting for CI", now)
	case pr.BehindBy > 0 && !slices.Contains(it.Updates, pr.Head):
		it.Updates = append(it.Updates, pr.Head)
		setNote(it, fmt.Sprintf("%d behind %s; updating the branch", pr.BehindBy, pr.Base), now)
		return "", nil, []Action{{Kind: UpdateBranch, Item: it.ID, Repo: it.Repo, PR: it.PR, SHA: pr.Head}}
	case pr.BehindBy > 0:
		setNote(it, "waiting for the branch update", now)
	default:
		it.ReadySHA = pr.Head
		setNote(it, "ready to merge", now)
	}
	return "", nil, nil
}

// justEnded: the item's last run ended within FreshChecks.
func justEnded(it *Item, now time.Time) bool {
	r := it.Last()
	return r != nil && r.Ended != nil && now.Sub(*r.Ended) < FreshChecks
}

// removeBoxes asks for every box of it's runs that's still there,
// except keep.
func removeBoxes(it *Item, o Obs, keep string) []Action {
	var acts []Action
	for _, r := range it.Runs {
		if _, ok := o.Boxes[r.Box]; ok && r.Box != keep && r.Ended != nil {
			acts = append(acts, Action{Kind: RemoveBox, Item: it.ID, Box: r.Box})
		}
	}
	return acts
}

// boxName is a new run's box: q<item>-<kind>-<n>, with a letter added
// while that's taken.
func boxName(it *Item, kind Kind, boxes map[string]Box) string {
	short := map[Kind]string{Code: "code", CIFix: "ci", ConflictFix: "merge"}[kind]
	base := fmt.Sprintf("q%d-%s-%d", it.ID, short, len(it.Runs)+1)
	name := base
	for i := 0; ; i++ {
		taken := false
		if _, ok := boxes[name]; ok {
			taken = true
		}
		for _, r := range it.Runs {
			if r.Box == name {
				taken = true
			}
		}
		if !taken {
			return name
		}
		name = base + string(rune('a'+i%26))
	}
}

var prURL = regexp.MustCompile(`/pull/(\d+)`)

func prNumber(url string) int {
	if m := prURL.FindStringSubmatch(url); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func setState(it *Item, s State, note string, now time.Time) {
	it.State, it.Note, it.Updated = s, note, now
}

func setNote(it *Item, note string, now time.Time) {
	if it.Note != note {
		it.Note, it.Updated = note, now
	}
}

func orNone(s string) string {
	if s == "" {
		return "no reason given"
	}
	return s
}

// GoalFor is what a run of kind on it is told: the item's goal for a
// code run, and for a follow-up what's wrong with the pull request,
// with the goal after it for context.
func GoalFor(it *Item, kind Kind, pr *PR) string {
	n := strconv.Itoa(it.PR)
	switch kind {
	case CIFix:
		failed, head := "", ""
		if pr != nil {
			failed, head = strings.Join(pr.Failed, ", "), pr.Head
		}
		return "The checks of pull request #" + n + " fail on its head commit " + head + ": " + failed + `.
Find out why (gh pr checks ` + n + `; gh run view <run id> --log-failed) and fix it on this branch. Run what failed locally before you push, if you can.

The pull request's goal, for context:

` + it.Goal
	case ConflictFix:
		base := "the default branch"
		if pr != nil && pr.Base != "" {
			base = pr.Base
		}
		return "Pull request #" + n + " conflicts with " + base + `. Merge origin/` + base + ` into this branch (git merge origin/` + base + ` -- not a rebase), resolve the conflicts keeping what both sides meant, run the tests, and push.

The pull request's goal, for context:

` + it.Goal
	}
	return it.Goal
}
