package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	"github.com/boxctl-io/boxctl-cli/internal/queue"
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

func TestQueueLaunchArgs(t *testing.T) {
	got := strings.Join(queueLaunchArgs(queue.Action{Repo: "o/r", Box: "q1-ci-2", Item: 1, Run: queue.CIFix, PR: 7, Goal: "fix", Model: "opus"}), " ")
	if got != "claude --repo o/r --box q1-ci-2 --queue-item 1 --goal fix --pr 7 --model opus" {
		t.Errorf("follow-up: %s", got)
	}
	if got := strings.Join(queueLaunchArgs(queue.Action{Repo: "o/r", Box: "q1-code-1", Item: 1, Run: queue.Code, Goal: "g"}), " "); strings.Contains(got, "--pr") {
		t.Errorf("code run: %s", got)
	}
}

// queueServer is a box API whose boxes the test changes between passes.
func queueServer(t *testing.T) (*client.Client, func([]client.VM), *[]string) {
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

func stubQueue(t *testing.T, launch func(args []string) (string, error), gh func(args []string) ([]byte, error)) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	oldL, oldG := launchCommand, ghRun
	launchCommand = func(_ string, args []string, _ string) (string, error) { return launch(args) }
	ghRun = func(_ context.Context, args ...string) ([]byte, error) { return gh(args) }
	t.Cleanup(func() { launchCommand, ghRun = oldL, oldG })
}

func TestQueuePass(t *testing.T) {
	var mu sync.Mutex
	var launched, ghCalls []string
	stubQueue(t, func(args []string) (string, error) {
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
	c, setVMs, calls := queueServer(t)
	if _, err := addQueueItem("o/r", "fix the bug", "", 0); err != nil {
		t.Fatal(err)
	}
	r, err := newQueueRunner(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Pass 1: a code run starts.
	if _, err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	if len(launched) != 1 || !strings.Contains(launched[0], "--box q1-code-1 --queue-item 1 --goal fix the bug") {
		t.Fatalf("launched = %v", launched)
	}
	q, _ := loadQueue()
	if it := q.Item(1); it.State != queue.Active || it.Current() == nil || !it.Current().Launched {
		t.Fatalf("after launch: %+v", it)
	}
	// A second loop is refused while this one holds the queue.
	other, _ := newQueueRunner(c)
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
	q, _ = loadQueue()
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
	if q, _ = loadQueue(); q.Loop != "" {
		t.Errorf("loop not released: %q", q.Loop)
	}
}

func TestQueueLaunchRefusedAndFailed(t *testing.T) {
	answer := "Error: You have 2 active boxes (a, b). Delete one with `boxctl rm <box>` or keep working in an existing one.\n"
	stubQueue(t, func(args []string) (string, error) { return answer, errors.New("exit status 1") }, func([]string) ([]byte, error) { return nil, nil })
	c, _, _ := queueServer(t)
	_, _ = addQueueItem("o/r", "g", "", 0)
	r, _ := newQueueRunner(c)
	if _, err := r.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	q, _ := loadQueue()
	it := q.Item(1)
	if it.State != queue.Queued || len(it.Runs) != 0 || time.Until(q.Backoff) <= 0 || !strings.Contains(it.Note, "active boxes") {
		t.Fatalf("refused: %+v, backoff %v", it, q.Backoff)
	}

	answer = "Error: cloning o/r on q2-code-1: exit status 128\n"
	_ = queue.Update(mustQueuePath(), func(q *queue.Queue) error { q.Backoff = time.Time{}; return nil })
	if _, err := r.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	q, _ = loadQueue()
	if run := q.Item(1).Last(); run == nil || run.Outcome != queue.OutcomeFailed || !strings.Contains(run.Error, "cloning") {
		t.Fatalf("failed launch: %+v", q.Item(1))
	}
}

func TestQueueCommands(t *testing.T) {
	stubQueue(t, nil, func(args []string) ([]byte, error) { return nil, nil })
	it, _ := addQueueItem("o/r", "g", "", 0)
	if err := retryQueueItem(it.ID); err == nil {
		t.Error("retry of a queued item")
	}
	_ = changeQueueItem(it.ID, func(it *queue.Item) error { it.State = queue.NeedsYou; return nil })
	if err := retryQueueItem(it.ID); err != nil {
		t.Fatal(err)
	}
	q, _ := loadQueue()
	if got := q.Item(it.ID); got.State != queue.Active || got.Budget() != queue.DefaultBudget+1 {
		t.Errorf("retried: %+v", got)
	}
	if err := cancelQueueItem(it.ID); err != nil {
		t.Fatal(err)
	}
	if err := cancelQueueItem(it.ID); err == nil {
		t.Error("cancelled twice")
	}
	if err := mergeQueueItem(context.Background(), &queue.Item{ID: 1, PR: 3, Note: "waiting for CI"}, "squash"); err == nil || !strings.Contains(err.Error(), "waiting for CI") {
		t.Errorf("merge not ready: %v", err)
	}
	var merged []string
	ghRun = func(_ context.Context, args ...string) ([]byte, error) { merged = args; return nil, nil }
	if err := mergeQueueItem(context.Background(), &queue.Item{ID: 1, Repo: "o/r", PR: 3, ReadySHA: "abc"}, "squash"); err != nil || strings.Join(merged, " ") != "pr merge 3 --repo o/r --squash --match-head-commit abc" {
		t.Errorf("merge: %v %v", err, merged)
	}
}

func TestTUIQueue(t *testing.T) {
	stubQueue(t, nil, func([]string) ([]byte, error) { return nil, nil })
	tuiFlashFor = time.Millisecond
	c, _ := fakeBoxServer(t, tuiFixture())
	m := newTUI(c)
	feed := func(cmd tea.Cmd) {
		for _, msg := range runCmd(cmd) {
			if msg == nil {
				continue
			}
			next, more := m.Update(msg)
			m = next.(tuiModel)
			if _, ok := msg.(tuiQueueDoneMsg); ok {
				for _, msg := range runCmd(more) {
					if lm, ok := msg.(tuiListMsg); ok {
						next, _ := m.Update(lm)
						m = next.(tuiModel)
					}
				}
			}
		}
	}
	next, cmd := m.runCommand("queue")
	m = next.(tuiModel)
	feed(cmd)
	if m.top().view != queueView {
		t.Fatalf("view = %v", m.top().view.name)
	}
	// a: repository, then goal.
	m, _ = press(t, m, "a")
	m = typeIn(t, m, "o/r")
	m, _ = press(t, m, "enter")
	if m.dialog == nil || !strings.Contains(m.dialog.title, "o/r") {
		t.Fatalf("goal prompt: %+v", m.dialog)
	}
	m = typeIn(t, m, "do the thing")
	m, cmd = press(t, m, "enter")
	feed(cmd)
	if got := keys(m.rows()); got != "1" {
		t.Fatalf("rows = %q", got)
	}
	if lines := describeQueueItem(m.queueItem()); !strings.Contains(strings.Join(lines, "\n"), "do the thing") {
		t.Errorf("describe: %v", lines)
	}
	if !strings.Contains(m.queueSummary(), "1 queued") {
		t.Errorf("summary = %q", m.queueSummary())
	}
	// m on an item that isn't ready only says so.
	if m2, _ := press(t, m, "m"); m2.dialog != nil || !strings.Contains(m2.flash, "isn't ready") {
		t.Errorf("merge not ready: %q", m2.flash)
	}
	// ctrl-d asks, y cancels.
	m, _ = press(t, m, "ctrl+d")
	m, cmd = press(t, m, "y")
	feed(cmd)
	q, _ := loadQueue()
	if q.Item(1).State != queue.Cancelled {
		t.Errorf("state = %s", q.Item(1).State)
	}

	// a in :repos queues a goal on the row's repository.
	m.repos = []ghRepo{{FullName: "o/other"}}
	m.loaded["repos"] = true
	m.pages = []page{{kind: pageTable, view: reposView, sortCol: -1}}
	m, _ = press(t, m, "a")
	m = typeIn(t, m, "x")
	m, cmd = press(t, m, "enter")
	feed(cmd)
	if q, _ = loadQueue(); len(q.Items) != 2 || q.Items[1].Repo != "o/other" {
		t.Errorf("from :repos: %+v", q.Items)
	}
}

// A goal longer than the terminal is wide wraps on the describe page,
// so all of it can be read, and scrolling reaches its last line.
func TestTUIQueueDescribeWraps(t *testing.T) {
	stubQueue(t, nil, func([]string) ([]byte, error) { return nil, nil })
	c, _ := fakeBoxServer(t, tuiFixture())
	m := newTUI(c)
	m.width, m.height = 60, 20
	goal := strings.Repeat("make the thing better ", 30) + "THE-END"
	if _, err := addQueueItem("o/r", goal, "", 0); err != nil {
		t.Fatal(err)
	}
	q, err := loadQueue()
	if err != nil {
		t.Fatal(err)
	}
	m.queue = q
	m.loaded["queue"] = true
	m.pages = []page{{kind: pageTable, view: queueView, sortCol: -1}}
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

// TestTUIQueueRepoChoices: the queue's `a` lists repositories -- the
// current item's first, then the other queued ones, then :repos' -- that
// typing narrows, up/down selects and tab completes.
func TestTUIQueueRepoChoices(t *testing.T) {
	stubQueue(t, nil, func([]string) ([]byte, error) { return nil, nil })
	c, _ := fakeBoxServer(t, tuiFixture())
	m := newTUI(c)
	m.repos = []ghRepo{{FullName: "acme/boxctl-cli"}, {FullName: "acme/web"}, {FullName: "o/r"}}
	m.loaded["repos"] = true
	m.reposAt = time.Now()
	m.queue = &queue.Queue{Items: []*queue.Item{{ID: 1, Repo: "o/r"}, {ID: 2, Repo: "zed/app"}}}
	m.pages = []page{{kind: pageTable, view: queueView, sortCol: -1}}
	m.moveTo(true) // item 2, zed/app
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
	if m.dialog == nil || m.dialog.title != "Queue a goal on acme/web" {
		t.Fatalf("goal prompt: %+v", m.dialog)
	}
	// A typed owner/name nobody listed is taken as typed.
	m, _ = press(t, m, "esc")
	m, _ = press(t, m, "a")
	m = typeIn(t, m, "acme/boxctl")
	m, _ = press(t, m, "enter")
	if m.dialog == nil || m.dialog.title != "Queue a goal on acme/boxctl" {
		t.Fatalf("typed: %+v", m.dialog)
	}
}
