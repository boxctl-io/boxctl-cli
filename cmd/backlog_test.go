package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
	"github.com/boxctl-io/boxctl-cli/internal/client"
	tea "github.com/charmbracelet/bubbletea"
)

const prAnswer = `{"data":{"repository":{"pullRequest":{"state":"OPEN","mergeable":"MERGEABLE","headRefOid":"abc123","baseRefName":"main",
"baseRef":{"compare":{"behindBy":0}},
"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"%s","contexts":{"nodes":[
{"name":"Lint","conclusion":"FAILURE","checkSuite":{"workflowRun":{"databaseId":11}}},
{"name":"Test","conclusion":"FAILURE","checkSuite":{"workflowRun":{"databaseId":11}}},
{"name":"Build","conclusion":"SUCCESS","checkSuite":{"workflowRun":{"databaseId":12}}},
{"context":"ci/ext","state":"ERROR"}
]}}}}]}}}}}`

func TestParsePR(t *testing.T) {
	pr, err := parsePR([]byte(strings.Replace(prAnswer, "%s", "FAILURE", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != "OPEN" || pr.Head != "abc123" || pr.Base != "main" || pr.Checks != "failure" {
		t.Errorf("pr = %+v", pr)
	}
	if strings.Join(pr.Failed, ",") != "Lint,Test,ci/ext" || len(pr.FailedRunIDs) != 1 || pr.FailedRunIDs[0] != 11 {
		t.Errorf("failed = %v %v", pr.Failed, pr.FailedRunIDs)
	}
	for state, want := range map[string]string{"SUCCESS": "success", "PENDING": "pending", "EXPECTED": "pending", "ERROR": "failure"} {
		if pr, _ := parsePR([]byte(strings.Replace(prAnswer, "%s", state, 1))); pr.Checks != want {
			t.Errorf("%s: %s", state, pr.Checks)
		}
	}
	none, _ := parsePR([]byte(`{"data":{"repository":{"pullRequest":{"state":"MERGED","commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}}`))
	if none.Checks != "none" || none.State != "MERGED" {
		t.Errorf("no rollup: %+v", none)
	}
	if _, err := parsePR([]byte(`{"data":{"repository":null},"errors":[{"message":"Could not resolve"}]}`)); err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Errorf("error answer: %v", err)
	}
}

func TestBacklogLaunchArgs(t *testing.T) {
	got := strings.Join(backlogLaunchArgs(backlog.Action{Repo: "o/r", Box: "q1-ci-2", Item: 1, Run: backlog.CIFix, PR: 7, Goal: "fix", Model: "opus", Harness: "visual"}), " ")
	// A one-word task: the backlog's runs skip --task's word count
	// (runRepo), which is there for a description typed by hand.
	if got != "claude --repo o/r --box q1-ci-2 --backlog-item 1 --run-kind ci_fix --task fix --pr 7 --model opus --harness visual" {
		t.Errorf("follow-up: %s", got)
	}
	if got := strings.Join(backlogLaunchArgs(backlog.Action{Repo: "o/r", Box: "q1-code-1", Item: 1, Run: backlog.Code, Goal: "g"}), " "); strings.Contains(got, "--pr") {
		t.Errorf("code run: %s", got)
	}
}

// backlogServer is a box API whose boxes the test changes between passes.
func backlogServer(t *testing.T) (*client.Client, func([]client.VM), *[]string) {
	t.Helper()
	var mu sync.Mutex
	var vms []client.VM
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/api/vms" {
			_ = json.NewEncoder(w).Encode(vms)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "tok"), func(v []client.VM) { mu.Lock(); vms = v; mu.Unlock() }, &calls
}

func stubBacklog(t *testing.T, launch func(args []string) (string, error), gh func(args []string) ([]byte, error)) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	oldL, oldG := launchCommand, ghRun
	launchCommand = func(_ string, args []string, _ string) (string, error) { return launch(args) }
	ghRun = func(_ context.Context, args ...string) ([]byte, error) { return gh(args) }
	t.Cleanup(func() { launchCommand, ghRun = oldL, oldG })
}

func TestBacklogPass(t *testing.T) {
	var mu sync.Mutex
	var launched, ghCalls []string
	stubBacklog(t, func(args []string) (string, error) {
		mu.Lock()
		launched = append(launched, strings.Join(args, " "))
		mu.Unlock()
		return "", nil
	}, func(args []string) ([]byte, error) {
		mu.Lock()
		ghCalls = append(ghCalls, strings.Join(args, " "))
		mu.Unlock()
		if args[0] == "api" && args[1] == "graphql" {
			return []byte(strings.Replace(prAnswer, "%s", "FAILURE", 1)), nil
		}
		return nil, nil
	})
	c, setVMs, calls := backlogServer(t)
	if _, err := addBacklogItem("o/r", "fix the bug", "", "", 0); err != nil {
		t.Fatal(err)
	}
	r, err := newBacklogRunner(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Pass 1: a code run starts.
	if _, err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	if len(launched) != 1 || !strings.Contains(launched[0], "--box q1-code-1 --backlog-item 1 --run-kind code --task fix the bug") {
		t.Fatalf("launched = %v", launched)
	}
	q, _ := loadBacklog()
	if it := q.Item(1); it.State != backlog.Active || it.Current() == nil || !it.Current().Launched {
		t.Fatalf("after launch: %+v", it)
	}
	// A second loop is refused while this one holds the backlog.
	other, _ := newBacklogRunner(c)
	other.owner = "elsewhere"
	if _, err := other.pass(ctx); !errors.As(err, new(errLoopHeld)) {
		t.Errorf("second loop: %v", err)
	}

	// Pass 2: the run is done with a PR. Pass 3: its CI fails -> re-run.
	setVMs([]client.VM{{Name: "q1-code-1", State: "running", RunStatus: json.RawMessage(`{"status":"done","branch":"boxctl/fix-1","pr_url":"https://github.com/o/r/pull/5"}`)}})
	if _, err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	q, _ = loadBacklog()
	it := q.Item(1)
	if it.PR != 5 || it.Branch != "boxctl/fix-1" || !slices.Contains(ghCalls, "run rerun 11 --failed -R o/r") {
		t.Fatalf("after CI failed: PR %d, branch %q, gh %v", it.PR, it.Branch, ghCalls)
	}
	// Pass 4: it fails again -> a ci_fix run on #5, and the code run's box goes.
	if _, err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	if len(launched) != 2 || !strings.Contains(launched[1], "--box q1-ci-2") || !strings.Contains(launched[1], "--pr 5") {
		t.Fatalf("follow-up launch = %v", launched)
	}
	if !slices.Contains(*calls, "DELETE /api/vms/q1-code-1") {
		t.Errorf("calls = %v", *calls)
	}
	r.release()
	if q, _ = loadBacklog(); q.Loop != "" {
		t.Errorf("loop not released: %q", q.Loop)
	}
}

func TestBacklogLaunchRefusedAndFailed(t *testing.T) {
	answer := "Error: You have 2 active boxes (a, b). Delete one with `boxctl rm <box>` or keep working in an existing one.\n"
	stubBacklog(t, func(args []string) (string, error) { return answer, errors.New("exit status 1") }, func([]string) ([]byte, error) { return nil, nil })
	c, _, _ := backlogServer(t)
	_, _ = addBacklogItem("o/r", "g", "", "", 0)
	r, _ := newBacklogRunner(c)
	if _, err := r.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	q, _ := loadBacklog()
	it := q.Item(1)
	if it.State != backlog.Queued || len(it.Runs) != 0 || time.Until(q.Backoff) <= 0 || !strings.Contains(it.Note, "active boxes") {
		t.Fatalf("refused: %+v, backoff %v", it, q.Backoff)
	}

	answer = "Error: cloning o/r on q2-code-1: exit status 128\n"
	_ = backlog.Update(mustBacklogPath(), func(q *backlog.Backlog) error { q.Backoff = time.Time{}; return nil })
	if _, err := r.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	q, _ = loadBacklog()
	if run := q.Item(1).Last(); run == nil || run.Outcome != backlog.OutcomeFailed || !strings.Contains(run.Error, "cloning") {
		t.Fatalf("failed launch: %+v", q.Item(1))
	}
}

func TestBacklogCommands(t *testing.T) {
	stubBacklog(t, nil, func(args []string) ([]byte, error) { return nil, nil })
	it, _ := addBacklogItem("o/r", "g", "", "", 0)
	if err := retryBacklogItem(it.ID); err == nil {
		t.Error("retry of a queued item")
	}
	_ = changeBacklogItem(it.ID, func(it *backlog.Item) error { it.State = backlog.NeedsYou; return nil })
	if err := retryBacklogItem(it.ID); err != nil {
		t.Fatal(err)
	}
	q, _ := loadBacklog()
	if got := q.Item(it.ID); got.State != backlog.Active || got.Budget() != backlog.DefaultBudget+1 {
		t.Errorf("retried: %+v", got)
	}
	if err := cancelBacklogItem(it.ID); err != nil {
		t.Fatal(err)
	}
	if err := cancelBacklogItem(it.ID); err == nil {
		t.Error("cancelled twice")
	}
	if err := mergeBacklogItem(context.Background(), &backlog.Item{ID: 1, PR: 3, Note: "waiting for CI"}, "squash"); err == nil || !strings.Contains(err.Error(), "waiting for CI") {
		t.Errorf("merge not ready: %v", err)
	}
	var merged []string
	ghRun = func(_ context.Context, args ...string) ([]byte, error) { merged = args; return nil, nil }
	if err := mergeBacklogItem(context.Background(), &backlog.Item{ID: 1, Repo: "o/r", PR: 3, ReadySHA: "abc"}, "squash"); err != nil || strings.Join(merged, " ") != "pr merge 3 --repo o/r --squash --match-head-commit abc" {
		t.Errorf("merge: %v %v", err, merged)
	}
}

func TestTUIBacklog(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	c, _ := fakeBoxServer(t, tuiFixture())
	m := boardTUI(c)
	feed := func(cmd tea.Cmd) {
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
	}
	next, cmd := m.runCommand("backlog")
	m = next.(tuiModel)
	feed(cmd)
	if m.top().view != backlogView {
		t.Fatalf("view = %v", m.top().view.name)
	}
	// a: repository, then harness, then goal.
	m, _ = press(t, m, "a")
	m = typeIn(t, m, "o/r")
	m, _ = press(t, m, "enter")
	if m.dialog == nil || m.dialog.title != "Harness for the task on o/r" {
		t.Fatalf("harness prompt: %+v", m.dialog)
	}
	m.harnesses = []string{"mine", "pr-only", "visual"}
	if got := strings.Join(m.dialog.matches(&m), " "); got != "pr-only mine visual none" {
		t.Errorf("harness choices = %q", got)
	}
	m = typeIn(t, m, "vis")
	m, _ = press(t, m, "enter")
	if m.dialog == nil || m.dialog.title != "Add a task on o/r (harness visual)" {
		t.Fatalf("goal prompt: %+v", m.dialog)
	}
	m = typeIn(t, m, "do the thing")
	m, cmd = press(t, m, "enter")
	feed(cmd)
	if got := keys(m.rows()); got != "1" {
		t.Fatalf("rows = %q", got)
	}
	if lines := describeBacklogItem(m.backlogItem()); !strings.Contains(strings.Join(lines, "\n"), "do the thing") {
		t.Errorf("describe: %v", lines)
	}
	if it := m.backlogItem(); it.Harness != "visual" {
		t.Errorf("harness = %q", it.Harness)
	}
	if !strings.Contains(m.backlogSummary(), "1 queued") {
		t.Errorf("summary = %q", m.backlogSummary())
	}
	// m on an item that isn't ready only says so.
	if m2, _ := press(t, m, "m"); m2.dialog != nil || !strings.Contains(m2.flash, "isn't ready") {
		t.Errorf("merge not ready: %q", m2.flash)
	}
	// ctrl-d asks, y cancels.
	m, _ = press(t, m, "ctrl+d")
	m, cmd = press(t, m, "y")
	feed(cmd)
	q, _ := loadBacklog()
	if q.Item(1).State != backlog.Cancelled {
		t.Errorf("state = %s", q.Item(1).State)
	}

	// m on a ready item asks, y merges, and the row is done at once --
	// not only once a pass sees the PR merged.
	_ = changeBacklogItem(1, func(it *backlog.Item) error {
		it.State, it.PR, it.ReadySHA, it.Note = backlog.Active, 7, "abcdef0123", "ready to merge"
		return nil
	})
	feed(loadBacklogMsg)
	m, _ = press(t, m, "m")
	m, cmd = press(t, m, "y")
	if it := m.backlog.Item(1); it.Note != "merging" {
		t.Errorf("while merging: %q", it.Note)
	}
	feed(cmd)
	if it := m.backlog.Item(1); it.State != backlog.Done || it.Note != "merged" || it.ReadySHA != "" {
		t.Errorf("merged: %s %q %q", it.State, it.Note, it.ReadySHA)
	}

	// a in :repos queues a goal on the row's repository.
	m.repos = []ghRepo{{FullName: "o/other"}}
	m.loaded["repos"] = true
	m.pages = []page{{kind: pageTable, view: reposView, sortCol: -1}}
	m, _ = press(t, m, "a")
	m, _ = press(t, m, "enter") // the default harness
	m = typeIn(t, m, "x")
	m, cmd = press(t, m, "enter")
	feed(cmd)
	if q, _ = loadBacklog(); len(q.Items) != 2 || q.Items[1].Repo != "o/other" {
		t.Errorf("from :repos: %+v", q.Items)
	}
}

// c asks what to change on the item's pull request and records it for a
// follow-up run; without a pull request it only says so.
func TestTUIBacklogRequestChanges(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	c, _ := fakeBoxServer(t, tuiFixture())
	m := boardTUI(c)
	feed := func(cmd tea.Cmd) {
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
	}
	if _, err := addBacklogItem("o/r", "do the thing", "", "", 0); err != nil {
		t.Fatal(err)
	}
	next, cmd := m.runCommand("backlog")
	m = next.(tuiModel)
	feed(cmd)
	if m2, _ := press(t, m, "c"); m2.dialog != nil || !strings.Contains(m2.flash, "no pull request") {
		t.Fatalf("no PR: %q", m2.flash)
	}
	_ = changeBacklogItem(1, func(it *backlog.Item) error {
		it.State, it.PR, it.ReadySHA, it.Note = backlog.NeedsYou, 7, "abc", "x"
		return nil
	})
	feed(loadBacklogMsg)
	m, _ = press(t, m, "c")
	if m.dialog == nil || !strings.Contains(m.dialog.title, "o/r#7") {
		t.Fatalf("prompt: %+v", m.dialog)
	}
	m = typeIn(t, m, "rename it")
	m, cmd = press(t, m, "enter")
	feed(cmd)
	it := m.backlog.Item(1)
	if it.Change != "rename it" || it.State != backlog.Active || it.ReadySHA != "" {
		t.Errorf("after c: %q %s %q", it.Change, it.State, it.ReadySHA)
	}
	if lines := strings.Join(describeBacklogItem(it), "\n"); !strings.Contains(lines, "Changes requested") || !strings.Contains(lines, "rename it") {
		t.Errorf("describe: %s", lines)
	}
}

// A pass asked for while one is going runs as soon as that one ends: the
// one going may have read GitHub before the change that asked.
func TestTUIBacklogPassAgain(t *testing.T) {
	m := tuiModel{qr: &backlogRunner{}, backlogPassing: true}
	if cmd := m.backlogPass(); cmd != nil || !m.backlogPassAgain {
		t.Fatalf("pass while passing: %v %v", cmd != nil, m.backlogPassAgain)
	}
	m.backlogPassed(tuiBacklogPassMsg{})
	if !m.backlogPassing || m.backlogPassAgain {
		t.Errorf("after the pass: passing %v, again %v", m.backlogPassing, m.backlogPassAgain)
	}
}

// A goal longer than the terminal is wide wraps on the describe page,
// so all of it can be read, and scrolling reaches its last line.
func TestTUIBacklogDescribeWraps(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	c, _ := fakeBoxServer(t, tuiFixture())
	m := boardTUI(c)
	m.width, m.height = 60, 20
	goal := strings.Repeat("make the thing better ", 30) + "THE-END"
	if _, err := addBacklogItem("o/r", goal, "", "", 0); err != nil {
		t.Fatal(err)
	}
	q, err := loadBacklog()
	if err != nil {
		t.Fatal(err)
	}
	m.backlog = q
	m.loaded["backlog"] = true
	m.pages = []page{{kind: pageTable, view: backlogView, sortCol: -1}}
	m, _ = press(t, m, "d")
	if m.top().kind != pageText {
		t.Fatalf("d didn't describe: %+v", m.top())
	}
	lines := m.textLines()
	var text strings.Builder
	for _, l := range lines {
		if w := len(l); w > m.width-3 {
			t.Errorf("line wider than the box (%d): %q", w, l)
		}
		text.WriteString(strings.TrimSpace(l) + " ")
	}
	if !strings.Contains(text.String(), strings.TrimSpace(goal)) {
		t.Errorf("goal not all there:\n%s", strings.Join(lines, "\n"))
	}
	m, _ = press(t, m, "G")
	if !strings.Contains(m.View(), "THE-END") {
		t.Errorf("end of the goal not shown after G:\n%s", m.View())
	}
}

func TestWrapLines(t *testing.T) {
	got := wrapLines([]string{"Note:       one two three four five", "short", "  abcdefghijkl mnop"}, 26)
	want := []string{"Note:       one two three", "            four five", "short", "  abcdefghijkl mnop"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	got = wrapLines([]string{"  aaaa bbbb cccc dddd", "xxxxxxxxxxxx"}, 10)
	want = []string{"  aaaa", "  bbbb", "  cccc", "  dddd", "xxxxxxxxxx", "xx"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestTUIBacklogRepoChoices: the backlog's `a` lists repositories -- the
// current item's first, then the other queued ones, then :repos' -- that
// typing narrows, up/down selects and tab completes.
func TestTUIBacklogRepoChoices(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	c, _ := fakeBoxServer(t, tuiFixture())
	m := boardTUI(c)
	m.repos = []ghRepo{{FullName: "acme/boxctl-cli"}, {FullName: "acme/web"}, {FullName: "o/r"}}
	m.loaded["repos"] = true
	m.reposAt = time.Now()
	m.backlog = &backlog.Backlog{Items: []*backlog.Item{{ID: 1, Repo: "o/r"}, {ID: 2, Repo: "zed/app"}}}
	m.pages = []page{{kind: pageTable, view: backlogView, sortCol: -1}}
	m, _ = press(t, m, "a")
	if m.dialog == nil {
		t.Fatal("no dialog")
	}
	if got := strings.Join(m.dialog.matches(&m), " "); got != "zed/app o/r acme/boxctl-cli acme/web" {
		t.Fatalf("choices = %q", got)
	}
	// Enter alone takes the selected one, the current item's.
	if got := m.dialog.value(&m); got != "zed/app" {
		t.Errorf("empty value = %q", got)
	}
	// The name after the owner matches too; tab completes.
	m = typeIn(t, m, "box")
	if got := strings.Join(m.dialog.matches(&m), " "); got != "acme/boxctl-cli" {
		t.Errorf("box matches = %q", got)
	}
	m, _ = press(t, m, "tab")
	if got := m.dialog.input.Value(); got != "acme/boxctl-cli" {
		t.Errorf("tab = %q", got)
	}
	// Narrow to acme's, down selects the second, enter takes it.
	m.dialog.input.SetValue("")
	m = typeIn(t, m, "acme")
	m, _ = press(t, m, "down")
	if lines := strings.Join(m.dialog.choiceLines(&m), "\n"); !strings.Contains(lines, "▸ acme/web") {
		t.Errorf("lines = %q", lines)
	}
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "enter")
	if m.dialog == nil || m.dialog.title != "Harness for the task on acme/web" {
		t.Fatalf("harness prompt: %+v", m.dialog)
	}
	// Enter alone keeps the default, which the item then doesn't name.
	m, _ = press(t, m, "enter")
	if m.dialog == nil || m.dialog.title != "Add a task on acme/web" {
		t.Fatalf("goal prompt: %+v", m.dialog)
	}
	// A typed owner/name nobody listed is taken as typed.
	m, _ = press(t, m, "esc")
	m, _ = press(t, m, "a")
	m = typeIn(t, m, "acme/boxctl")
	m, _ = press(t, m, "enter")
	// A typed name@ref is taken as typed too.
	m = typeIn(t, m, "pr-only@v2")
	m, _ = press(t, m, "enter")
	if m.dialog == nil || m.dialog.title != "Add a task on acme/boxctl (harness pr-only@v2)" {
		t.Fatalf("typed: %+v", m.dialog)
	}
}

func TestParseBacklogLimit(t *testing.T) {
	for _, c := range []struct {
		in      string
		global  int
		perRepo int // 0: not given
		bad     bool
	}{
		{in: "3", global: 3},
		{in: " 4 2 ", global: 4, perRepo: 2},
		{in: "4/1", global: 4, perRepo: 1},
		{in: "", bad: true},
		{in: "0", bad: true},
		{in: "x", bad: true},
		{in: "1 2 3", bad: true},
		{in: "2 -1", bad: true},
	} {
		g, p, err := parseBacklogLimit(c.in)
		if (err != nil) != c.bad {
			t.Errorf("%q: err = %v", c.in, err)
			continue
		}
		if c.bad {
			continue
		}
		got := 0
		if p != nil {
			got = *p
		}
		if g != c.global || got != c.perRepo {
			t.Errorf("%q = %d, %d", c.in, g, got)
		}
	}
}

// TestTUIBacklogLimit: L in :backlog asks for the slot limit and saves it.
func TestTUIBacklogLimit(t *testing.T) {
	stubBacklog(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	c, _ := fakeBoxServer(t, tuiFixture())
	m := boardTUI(c)
	m.backlog = &backlog.Backlog{Limits: backlog.DefaultLimits}
	m.pages = []page{{kind: pageTable, view: backlogView, sortCol: -1}}
	m, _ = press(t, m, "L")
	if m.dialog == nil || m.dialog.title != "Slot limit" {
		t.Fatalf("dialog: %+v", m.dialog)
	}
	m = typeIn(t, m, "5 3")
	m, cmd := press(t, m, "enter")
	runCmd(cmd)
	q, _ := loadBacklog()
	if q.Limits != (backlog.Limits{Global: 5, PerRepo: 3}) {
		t.Errorf("limits = %+v", q.Limits)
	}
	// One number leaves the per-repository limit.
	m, _ = press(t, m, "L")
	m = typeIn(t, m, "1")
	_, cmd = press(t, m, "enter")
	runCmd(cmd)
	if q, _ = loadBacklog(); q.Limits != (backlog.Limits{Global: 1, PerRepo: 3}) {
		t.Errorf("limits = %+v", q.Limits)
	}
}

// The backlog's commands replaced queue's, with no alias for the old ones.
func TestBacklogCommandNames(t *testing.T) {
	if c, _, _ := rootCmd.Find([]string{"queue"}); c != rootCmd {
		t.Errorf("queue found: %s", c.CommandPath())
	}
	if c, _, _ := rootCmd.Find([]string{"backlog", "run"}); c == backlogDispatchCmd {
		t.Error("backlog run found")
	}
	if c, _, _ := rootCmd.Find([]string{"backlog", "dispatch"}); c != backlogDispatchCmd {
		t.Errorf("backlog dispatch = %s", c.CommandPath())
	}
	if c, _, _ := rootCmd.Find([]string{"runs"}); c != runsCmd {
		t.Errorf("runs = %s", c.CommandPath())
	}
	if boardApp.findView("queue") != nil || boardApp.findView("q") != nil {
		t.Error(":queue still opens a view")
	}
}

// TestBacklogOrdered: :backlog shows newest first by default and 1/2/3 pick
// the order; dispatch's own order (Sorted) is untouched.
func TestBacklogOrdered(t *testing.T) {
	t0 := time.Now()
	q := &backlog.Backlog{Items: []*backlog.Item{
		{ID: 1, Created: t0, Priority: 0, State: backlog.Queued},
		{ID: 2, Created: t0.Add(time.Hour), Priority: 0, State: backlog.Done},
		{ID: 3, Created: t0.Add(2 * time.Hour), Priority: 5, State: backlog.Queued},
	}}
	ids := func(order int) string {
		var s []string
		for _, it := range backlogOrdered(q, order) {
			s = append(s, strconv.Itoa(it.ID))
		}
		return strings.Join(s, "")
	}
	if got := ids(backlogNewest); got != "321" {
		t.Errorf("newest = %s", got)
	}
	if got := ids(backlogOldest); got != "123" {
		t.Errorf("oldest = %s", got)
	}
	if got := ids(backlogByPriority); got != "312" {
		t.Errorf("priority = %s", got)
	}
}
