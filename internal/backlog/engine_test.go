package backlog

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func kinds(acts []Action) []string {
	var s []string
	for _, a := range acts {
		s = append(s, string(a.Kind)+":"+a.Box+string(a.Run))
	}
	return s
}

func newQ(n int) *Backlog {
	q := &Backlog{Limits: DefaultLimits}
	for i := 0; i < n; i++ {
		q.Add("o/r"+string(rune('a'+i)), "goal "+string(rune('a'+i)), "", 0, t0)
	}
	return q
}

func TestDispatchRespectsLimits(t *testing.T) {
	q := newQ(3)
	q.Items[2].Priority = 5
	acts := Decide(q, Obs{Now: t0, Boxes: map[string]Box{}})
	if got := kinds(acts); len(got) != 2 || got[0] != "launch:q3-code-1code" || got[1] != "launch:q1-code-1code" {
		t.Fatalf("launches = %v", got)
	}
	if q.Items[1].State != Queued || !strings.Contains(q.Items[1].Note, "2/2") {
		t.Errorf("third item: %s %q", q.Items[1].State, q.Items[1].Note)
	}
	if q.Items[0].State != Active || q.Items[0].Current() == nil {
		t.Errorf("first item not started: %+v", q.Items[0])
	}
	// Per repository too.
	q = newQ(0)
	q.Limits = Limits{Global: 5, PerRepo: 1}
	q.Add("o/r", "a", "", 0, t0)
	q.Add("o/r", "b", "", 0, t0)
	if got := kinds(Decide(q, Obs{Now: t0, Boxes: map[string]Box{}})); len(got) != 1 {
		t.Errorf("per-repo: %v", got)
	}
	// Nothing while boxctl backs off, nor without a box listing.
	q = newQ(1)
	q.Backoff = t0.Add(time.Minute)
	if got := Decide(q, Obs{Now: t0, Boxes: map[string]Box{}}); len(got) != 0 {
		t.Errorf("backoff: %v", kinds(got))
	}
	q = newQ(1)
	if got := Decide(q, Obs{Now: t0}); len(got) != 0 {
		t.Errorf("no listing: %v", kinds(got))
	}
}

// started is an item with a launched code run on q1-code-1.
func started(t *testing.T) *Backlog {
	t.Helper()
	q := newQ(1)
	Decide(q, Obs{Now: t0, Boxes: map[string]Box{}})
	q.Items[0].Runs[0].Launched = true
	return q
}

func TestRunEndsWithPR(t *testing.T) {
	q := started(t)
	it := q.Items[0]
	boxes := map[string]Box{"q1-code-1": {State: "running", Status: "running"}}
	if acts := Decide(q, Obs{Now: t0.Add(time.Minute), Boxes: boxes}); len(acts) != 0 || it.Current() == nil {
		t.Fatalf("running: %v %+v", kinds(acts), it.Runs)
	}
	boxes["q1-code-1"] = Box{State: "running", Status: "done", PRURL: "https://github.com/o/ra/pull/42", Branch: "boxctl/x-1"}
	Decide(q, Obs{Now: t0.Add(10 * time.Minute), Boxes: boxes})
	if it.Current() != nil || it.Runs[0].Outcome != OutcomeDone || it.PR != 42 || it.Branch != "boxctl/x-1" {
		t.Fatalf("after done: %+v", it)
	}
	// Green and up to date: ready.
	pr := &PR{State: "OPEN", Mergeable: "MERGEABLE", Head: "abc", Base: "main", Checks: "success"}
	Decide(q, Obs{Now: t0.Add(20 * time.Minute), Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if it.ReadySHA != "abc" || it.Note != "ready to merge" {
		t.Errorf("ready: %q %q", it.ReadySHA, it.Note)
	}
	// Merged: done, and the box goes.
	pr.State = "MERGED"
	acts := Decide(q, Obs{Now: t0.Add(30 * time.Minute), Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if it.State != Done || len(acts) != 1 || acts[0].Kind != RemoveBox || acts[0].Box != "q1-code-1" {
		t.Errorf("merged: %s %v", it.State, kinds(acts))
	}
}

func TestCodeRetriesThenNeedsYou(t *testing.T) {
	q := started(t)
	it := q.Items[0]
	boxes := map[string]Box{"q1-code-1": {State: "running", Status: "failed", Error: "no PR"}}
	acts := Decide(q, Obs{Now: t0.Add(time.Minute), Boxes: boxes})
	if got := kinds(acts); len(got) != 2 || got[0] != "remove_box:q1-code-1" || got[1] != "launch:q1-code-2code" {
		t.Fatalf("retry: %v", got)
	}
	it.Runs[1].Launched = true
	boxes = map[string]Box{"q1-code-2": {State: "running", Status: "failed", Error: "still no PR"}}
	Decide(q, Obs{Now: t0.Add(2 * time.Minute), Boxes: boxes})
	if it.State != NeedsYou || !strings.Contains(it.Note, "still no PR") {
		t.Errorf("budget spent: %s %q", it.State, it.Note)
	}
}

func TestLostRunsArentCharged(t *testing.T) {
	q := started(t)
	it := q.Items[0]
	for i := 0; i < MaxLost; i++ {
		if it.State != Active {
			t.Fatalf("lost %d: %s %q", i, it.State, it.Note)
		}
		Decide(q, Obs{Now: t0.Add(time.Duration(i+1) * time.Minute), Boxes: map[string]Box{}})
		if c := it.Current(); c != nil {
			c.Launched = true
		}
	}
	if it.Used(Code) != 0 || it.State != NeedsYou || !strings.Contains(it.Note, "lost") {
		t.Errorf("used %d, %s %q", it.Used(Code), it.State, it.Note)
	}
}

func TestInterruptedLaunch(t *testing.T) {
	q := newQ(1)
	Decide(q, Obs{Now: t0, Boxes: map[string]Box{}})
	it := q.Items[0]
	// Still launching in this process: left alone.
	if acts := Decide(q, Obs{Now: t0.Add(time.Minute), Boxes: map[string]Box{}, Launching: map[string]bool{"q1-code-1": true}}); len(acts) != 0 || it.Current() == nil {
		t.Fatalf("launching: %v", kinds(acts))
	}
	// Not launching here any more (a tui closed mid-launch), never said
	// launched, box there but silent: waited for, then lost after the grace.
	silent := map[string]Box{"q1-code-1": {State: "running"}}
	if acts := Decide(q, Obs{Now: t0.Add(2 * time.Minute), Boxes: silent}); len(acts) != 0 || it.Current() == nil {
		t.Fatalf("silent within grace: %v %+v", kinds(acts), it.Runs)
	}
	acts := Decide(q, Obs{Now: t0.Add(LaunchGrace + time.Minute), Boxes: silent})
	if got := kinds(acts); len(got) < 2 || got[0] != "remove_box:q1-code-1" || it.Runs[0].Outcome != OutcomeLost {
		t.Errorf("silent past grace: %v %+v", got, it.Runs[0])
	}

	// No box at all, past the grace: lost, nothing to remove.
	q = newQ(1)
	Decide(q, Obs{Now: t0, Boxes: map[string]Box{}})
	it = q.Items[0]
	if Decide(q, Obs{Now: t0.Add(time.Minute), Boxes: map[string]Box{}}); it.Current() == nil {
		t.Fatal("a launch from a closed tui wasn't given time to make its box")
	}
	Decide(q, Obs{Now: t0.Add(LaunchGrace + time.Minute), Boxes: map[string]Box{}})
	if it.Runs[0].Outcome != OutcomeLost {
		t.Errorf("no box past grace: %+v", it.Runs[0])
	}
}

// TestReportingBoxBeatsLaunch: a box that reports the run is followed
// whether or not its launch returned -- still running here, or lost with
// a closed tui -- so a done run's PR is picked up and its box kept.
func TestReportingBoxBeatsLaunch(t *testing.T) {
	for _, launching := range []bool{true, false} {
		q := newQ(1)
		Decide(q, Obs{Now: t0, Boxes: map[string]Box{}})
		it := q.Items[0]
		boxes := map[string]Box{"q1-code-1": {State: "running", Status: "done", PRURL: "https://github.com/o/r/pull/1330", Branch: "boxctl/x-1"}}
		acts := Decide(q, Obs{Now: t0.Add(5 * time.Minute), Boxes: boxes, Launching: map[string]bool{"q1-code-1": launching}})
		for _, k := range kinds(acts) {
			if strings.HasPrefix(k, "remove_box") || strings.HasPrefix(k, "launch") {
				t.Errorf("launching=%v: %s", launching, k)
			}
		}
		if r := it.Runs[0]; r.Outcome != OutcomeDone || !r.Launched || it.PR != 1330 || it.Branch != "boxctl/x-1" {
			t.Errorf("launching=%v: %+v pr %d", launching, r, it.PR)
		}
	}
}

func TestSilentAndStuckRuns(t *testing.T) {
	q := started(t)
	it := q.Items[0]
	boxes := map[string]Box{"q1-code-1": {State: "running"}}
	if Decide(q, Obs{Now: t0.Add(time.Minute), Boxes: boxes}); it.Current() == nil {
		t.Fatal("a box that hasn't reported yet ended the run")
	}
	Decide(q, Obs{Now: t0.Add(LaunchGrace + time.Minute), Boxes: boxes})
	if it.Runs[0].Outcome != OutcomeLost {
		t.Errorf("silent: %+v", it.Runs[0])
	}

	q = started(t)
	it = q.Items[0]
	boxes = map[string]Box{"q1-code-1": {State: "running", Status: "running"}}
	Decide(q, Obs{Now: t0.Add(RunTimeLimit + time.Minute), Boxes: boxes})
	if it.Runs[0].Outcome != OutcomeFailed {
		t.Errorf("over time: %+v", it.Runs[0])
	}

	q = started(t)
	it = q.Items[0]
	boxes = map[string]Box{"q1-code-1": {State: "paused", Status: "running"}}
	Decide(q, Obs{Now: t0.Add(time.Minute), Boxes: boxes})
	if it.Runs[0].Outcome != OutcomeLost {
		t.Errorf("paused mid-run: %+v", it.Runs[0])
	}
}

// withPR is an item whose code run is done with PR #7.
func withPR(t *testing.T) (*Backlog, *Item, map[string]Box) {
	t.Helper()
	q := started(t)
	boxes := map[string]Box{"q1-code-1": {State: "paused", Status: "done", PRURL: "https://github.com/o/ra/pull/7"}}
	Decide(q, Obs{Now: t0.Add(time.Minute), Boxes: boxes})
	return q, q.Items[0], boxes
}

func TestCIFailure(t *testing.T) {
	q, _, boxes := withPR(t)
	pr := &PR{State: "OPEN", Mergeable: "MERGEABLE", Head: "h1", Base: "main", Checks: "failure", Failed: []string{"Lint"}, FailedRunIDs: []int64{99}}
	now := t0.Add(time.Hour)
	acts := Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if len(acts) != 1 || acts[0].Kind != Rerun || acts[0].RunIDs[0] != 99 {
		t.Fatalf("first failure: %v", kinds(acts))
	}
	acts = Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if got := kinds(acts); len(got) != 2 || got[1] != "launch:q1-ci-2ci_fix" || acts[1].PR != 7 || !strings.Contains(acts[1].Goal, "Lint") {
		t.Fatalf("second failure: %v", got)
	}
	// A status context alone can't be re-run: straight to a fix.
	q, _, boxes = withPR(t)
	pr = &PR{State: "OPEN", Head: "h1", Checks: "failure", Failed: []string{"ci/x"}}
	if got := kinds(Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}})); len(got) != 2 || got[1] != "launch:q1-ci-2ci_fix" {
		t.Errorf("status context: %v", got)
	}
	// Out of ci_fix runs: needs you.
	q, it, boxes := withPR(t)
	it.Runs = append(it.Runs, Run{Kind: CIFix, Box: "a", Ended: &now, Outcome: OutcomeDone}, Run{Kind: CIFix, Box: "b", Ended: &now, Outcome: OutcomeFailed})
	it.Reruns = []string{"h1"}
	pr = &PR{State: "OPEN", Head: "h1", Checks: "failure", Failed: []string{"Lint"}, FailedRunIDs: []int64{1}}
	Decide(q, Obs{Now: now.Add(time.Hour), Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if it.State != NeedsYou || !strings.Contains(it.Note, "Lint") {
		t.Errorf("budget: %s %q", it.State, it.Note)
	}
}

func TestConflictBehindAndWaiting(t *testing.T) {
	now := t0.Add(time.Hour)
	q, _, boxes := withPR(t)
	pr := &PR{State: "OPEN", Mergeable: "CONFLICTING", Head: "h", Base: "dev", Checks: "success"}
	acts := Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if got := kinds(acts); len(got) != 2 || got[1] != "launch:q1-merge-2conflict_fix" || !strings.Contains(acts[1].Goal, "git merge origin/dev") {
		t.Fatalf("conflict: %v", got)
	}

	q, it, boxes := withPR(t)
	pr = &PR{State: "OPEN", Mergeable: "MERGEABLE", Head: "h", Base: "main", Checks: "success", BehindBy: 3}
	acts = Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if len(acts) != 1 || acts[0].Kind != UpdateBranch || acts[0].SHA != "h" {
		t.Fatalf("behind: %v", kinds(acts))
	}
	if acts = Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}}); len(acts) != 0 || it.ReadySHA != "" {
		t.Errorf("behind, asked already: %v %q", kinds(acts), it.ReadySHA)
	}

	for _, pr := range []*PR{
		{State: "OPEN", Mergeable: "MERGEABLE", Head: "h", Checks: "pending"},
		{State: "OPEN", Mergeable: "UNKNOWN", Head: "h", Checks: "success"},
	} {
		q, it, boxes := withPR(t)
		if acts := Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}}); len(acts) != 0 || it.Note != "waiting for CI" {
			t.Errorf("%+v: %v %q", pr, kinds(acts), it.Note)
		}
	}
	// No checks right after a run: wait; later: ready.
	q, it, boxes = withPR(t)
	pr = &PR{State: "OPEN", Mergeable: "MERGEABLE", Head: "h", Checks: "none"}
	Decide(q, Obs{Now: t0.Add(2 * time.Minute), Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if it.ReadySHA != "" {
		t.Error("ready before CI could start")
	}
	Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: pr}})
	if it.ReadySHA != "h" {
		t.Error("no checks at all: not ready")
	}
	// Closed without merging.
	q, it, boxes = withPR(t)
	Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: {State: "CLOSED"}}})
	if it.State != NeedsYou {
		t.Errorf("closed: %s", it.State)
	}
}

func TestCancelledRemovesBoxes(t *testing.T) {
	q, it, boxes := withPR(t)
	it.State = Cancelled
	acts := Decide(q, Obs{Now: t0.Add(time.Hour), Boxes: boxes})
	if len(acts) != 1 || acts[0].Kind != RemoveBox {
		t.Errorf("cancelled: %v", kinds(acts))
	}
}

func TestBoxNameAvoidsTaken(t *testing.T) {
	q := newQ(1)
	acts := Decide(q, Obs{Now: t0, Boxes: map[string]Box{"q1-code-1": {}, "q1-code-1a": {}}})
	if acts[0].Box != "q1-code-1b" {
		t.Errorf("box = %s", acts[0].Box)
	}
}

func TestUpdateAndClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backlog.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Update(path, func(q *Backlog) error { q.Add("o/r", "g", "", 0, t0); return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	q, err := Load(path)
	if err != nil || len(q.Items) != 20 || q.Next != 20 {
		t.Fatalf("concurrent adds: %v, %d items", err, len(q.Items))
	}
	if q.Limits != DefaultLimits {
		t.Errorf("limits = %+v", q.Limits)
	}
	if _, ok := q.Claim("a", t0); !ok {
		t.Fatal("first claim refused")
	}
	if who, ok := q.Claim("b", t0.Add(time.Minute)); ok || who != "a" {
		t.Errorf("second claim: %s %v", who, ok)
	}
	if _, ok := q.Claim("b", t0.Add(LoopStale+time.Minute)); !ok {
		t.Error("stale claim refused")
	}
	q.Release("b")
	if q.Loop != "" {
		t.Error("not released")
	}
}

// killedCI is item #3 with a code run done and a CI fix going on q3-ci-2,
// then killed.
func killedCI(t *testing.T) (*Backlog, *Item) {
	t.Helper()
	q := newQ(3)
	it := q.Item(3)
	end := t0.Add(time.Hour)
	it.State, it.PR = Active, 7
	it.Runs = []Run{
		{Kind: Code, Box: "q3-code-1", Started: t0, Launched: true, Ended: &end, Outcome: OutcomeDone},
		{Kind: CIFix, Box: "q3-ci-2", Started: end, Launched: true},
	}
	if err := Kill(it, "q3-ci-2", Box{}, end.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return q, it
}

func TestKill(t *testing.T) {
	_, it := killedCI(t)
	r := it.Last()
	if r.Outcome != OutcomeKilled || r.Ended == nil || it.Current() != nil {
		t.Errorf("run = %+v", r)
	}
	if it.State != NeedsYou || it.Note != "run killed (ci_fix on q3-ci-2)" {
		t.Errorf("item: %s %q", it.State, it.Note)
	}
	if it.Used(CIFix) != 1 {
		t.Errorf("Used(ci_fix) = %d, want the kill charged", it.Used(CIFix))
	}
	// Only the run going on that box can be killed.
	if err := Kill(it, "q3-ci-2", Box{}, t0); err == nil {
		t.Error("killed an ended run")
	}
	q := newQ(1)
	q.Items[0].Runs = []Run{{Kind: Code, Box: "q1-code-1", Started: t0}}
	if err := Kill(q.Items[0], "q1-code-9", Box{}, t0); err == nil || q.Items[0].Current() == nil {
		t.Errorf("wrong box: %v", err)
	}
}

func TestKilledItemWaitsForRetry(t *testing.T) {
	q, it := killedCI(t)
	q.Limits = Limits{Global: 10, PerRepo: 10}
	now := t0.Add(2 * time.Hour)
	pr := map[int]*PR{3: {State: "OPEN", Mergeable: "MERGEABLE", Head: "abc", Base: "main", Checks: "failure"}}
	// Its box gone: nothing ends as lost, nothing launches for #3.
	acts := Decide(q, Obs{Now: now, Boxes: map[string]Box{}, PRs: pr})
	for _, a := range acts {
		if a.Item == 3 {
			t.Errorf("box gone: %v", kinds(acts))
		}
	}
	if it.State != NeedsYou || it.Last().Outcome != OutcomeKilled {
		t.Errorf("after a pass: %s %+v", it.State, it.Last())
	}
	// Its box still there (deleting it failed): the pass deletes it.
	acts = Decide(q, Obs{Now: now, Boxes: map[string]Box{"q3-ci-2": {State: "running", Status: "running"}}, PRs: pr})
	var mine []string
	for _, a := range acts {
		if a.Item == 3 {
			mine = append(mine, string(a.Kind)+":"+a.Box)
		}
	}
	if strings.Join(mine, " ") != "remove_box:q3-ci-2" {
		t.Errorf("box still there: %v", mine)
	}
	// A retry gives the kind one more run, and the next pass starts it.
	it.State, it.Extra = Active, it.Extra+1
	acts = Decide(q, Obs{Now: now, Boxes: map[string]Box{}, PRs: pr})
	if got := kinds(acts); !strings.Contains(strings.Join(got, " "), "launch:q3-ci-3ci_fix") {
		t.Errorf("after retry: %v", got)
	}
}

// A kill links the pull request the box reported, as a pass would: the box
// is about to go, and a retry without it would open a second one.
func TestKillLinksTheBoxsPR(t *testing.T) {
	q := newQ(1)
	it := q.Items[0]
	it.State = Active
	it.Runs = []Run{{Kind: Code, Box: "q1-code-1", Started: t0, Launched: true}}
	seen := Box{State: "running", Status: "running", PRURL: "https://github.com/o/ra/pull/12", Branch: "boxctl/x"}
	if err := Kill(it, "q1-code-1", seen, t0); err != nil {
		t.Fatal(err)
	}
	if it.PR != 12 || it.Branch != "boxctl/x" {
		t.Errorf("PR %d, branch %q", it.PR, it.Branch)
	}
}

func TestRequestChange(t *testing.T) {
	q := newQ(1)
	it := q.Items[0]
	if err := RequestChange(it, "x", t0); err == nil || !strings.Contains(err.Error(), "no pull request") {
		t.Errorf("no PR: %v", err)
	}
	q, it, boxes := withPR(t)
	if err := RequestChange(it, "  ", t0); err == nil {
		t.Error("empty request taken")
	}
	ready := &PR{State: "OPEN", Mergeable: "MERGEABLE", Head: "h1", Base: "main", Checks: "success"}
	now := t0.Add(time.Hour)
	Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: ready}})
	if it.ReadySHA != "h1" {
		t.Fatalf("not ready: %q", it.Note)
	}
	// A request on an item that needs you makes it active again, and
	// isn't ready to merge any more.
	it.State = NeedsYou
	if err := RequestChange(it, "rename foo", now); err != nil {
		t.Fatal(err)
	}
	if err := RequestChange(it, "and add a test", now); err != nil {
		t.Fatal(err)
	}
	if it.State != Active || it.ReadySHA != "" {
		t.Fatalf("after request: %s %q", it.State, it.ReadySHA)
	}
	// It goes ahead of a conflict fix, on the PR's branch, told both requests.
	conflict := &PR{State: "OPEN", Mergeable: "CONFLICTING", Head: "h1", Base: "main"}
	acts := Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: conflict}})
	l := acts[len(acts)-1]
	if l.Kind != Launch || l.Run != Change || l.Box != "q1-change-2" || l.PR != 7 || !strings.Contains(l.Goal, "rename foo\n\nand add a test") || !strings.Contains(l.Goal, "goal a") {
		t.Fatalf("launch: %v %+v", kinds(acts), l)
	}
	if it.Change != "" || it.Current().Ask != "rename foo\n\nand add a test" {
		t.Fatalf("taken on: %q %q", it.Change, it.Current().Ask)
	}
	// Asked again while it runs: waits for the next run.
	later := now.Add(time.Minute)
	if err := RequestChange(it, "one more", later); err != nil || !strings.Contains(it.Note, "after the change run") {
		t.Fatalf("while running: %v %q", err, it.Note)
	}
	// It fails: asked for again, ahead of the newer request, and a new run
	// starts. Asking again was a new request, so that failure isn't
	// charged to it: two more failures are out of budget.
	boxes["q1-change-2"] = Box{State: "running", Status: "failed", Error: "boom"}
	Decide(q, Obs{Now: later, Boxes: boxes, PRs: map[int]*PR{1: ready}})
	if r := it.Runs[1]; r.Outcome != OutcomeFailed || it.Current() == nil || it.Current().Ask != "rename foo\n\nand add a test\n\none more" {
		t.Fatalf("after failure: %+v %+v", r, it.Current())
	}
	for i := 0; i < 2; i++ {
		if it.Current() == nil {
			t.Fatalf("run %d didn't start: %s %q", i, it.State, it.Note)
		}
		boxes[it.Current().Box] = Box{State: "running", Status: "failed", Error: "boom"}
		Decide(q, Obs{Now: later, Boxes: boxes, PRs: map[int]*PR{1: ready}})
	}
	if it.State != NeedsYou || !strings.Contains(it.Note, "failed 2 times") || it.Change == "" {
		t.Fatalf("out of budget: %s %q %q", it.State, it.Note, it.Change)
	}
	// A run that's done clears it; the PR is watched as before.
	q, it, boxes = withPR(t)
	_ = RequestChange(it, "x", now)
	Decide(q, Obs{Now: now, Boxes: boxes, PRs: map[int]*PR{1: ready}})
	boxes[it.Current().Box] = Box{State: "running", Status: "done"}
	Decide(q, Obs{Now: now.Add(time.Hour), Boxes: boxes, PRs: map[int]*PR{1: ready}})
	if it.Current() != nil || it.Change != "" || it.ReadySHA != "h1" {
		t.Errorf("done: %+v %q %q", it.Current(), it.Change, it.Note)
	}
}
