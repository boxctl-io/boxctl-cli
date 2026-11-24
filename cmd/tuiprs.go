package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// `:prs`: the open pull requests of one GitHub repository, as `gh pr
// list` shows them. The repository is the current directory's GitHub
// origin, `:prs owner/name`, or the one `p` picked in `:repos`. enter
// describes a PR, `g` asks what Claude should do about it and runs
// `boxctl claude --repo R --pr N --goal "..."` (an unattended run on the
// PR's branch, claudepr.go), `o` opens it in the browser. Like :repos it
// asks GitHub only when opened and on R, never on the 5s tick.

// ghPR is what this keeps of a pull request from `gh pr list --json`.
type ghPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	IsDraft        bool      `json:"isDraft"`
	HeadRefName    string    `json:"headRefName"`
	BaseRefName    string    `json:"baseRefName"`
	ReviewDecision string    `json:"reviewDecision"`
	Mergeable      string    `json:"mergeable"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Body           string    `json:"body"`
	Author         struct {
		Login string `json:"login"`
	} `json:"author"`
}

const prFields = "number,title,url,isDraft,headRefName,baseRefName,reviewDecision,mergeable,updatedAt,body,author"

// listGitHubPRs lists repo's open pull requests, newest update first. A
// variable so tests stand in for gh.
var listGitHubPRs = func(ctx context.Context, repo string) ([]ghPR, error) {
	var stderr strings.Builder
	c := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "100", "--json", prFields)
	c.Stderr = &stderr
	out, err := c.Output()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, errors.New("gh isn't installed")
	case ctx.Err() != nil:
		return nil, errors.New("GitHub didn't answer in time")
	case err != nil:
		if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
			return nil, errors.New(line)
		}
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("reading gh's answer: %w", err)
	}
	return prs, nil
}

// listPRs is list("prs"): the repository's open pull requests, in the
// background.
func (m *tuiModel) listPRs() tea.Cmd {
	if m.loading["prs"] {
		return nil
	}
	if m.prsRepo == "" {
		m.prsRepo = githubRepo(".")
	}
	if m.prsRepo == "" {
		return m.fail("no GitHub repository here: `:prs owner/name`, or p on a repo in :repos")
	}
	m.loading["prs"] = true
	repo := m.prsRepo
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		msg := tuiListMsg{resource: "prs"}
		msg.prs, msg.err = listGitHubPRs(ctx, repo)
		msg.prsRepo = repo
		return msg
	}
}

// openPRs shows repo's pull requests.
func (m *tuiModel) openPRs(repo string) tea.Cmd {
	if repo != m.prsRepo {
		m.prsRepo = repo
		m.prs = nil
		delete(m.loaded, "prs")
	}
	return m.openView(prsView)
}

var prsView = &tableView{
	name:     "prs",
	aliases:  []string{"pr", "pulls"},
	title:    "Open PRs",
	resource: "prs",
	onDemand: true,
	columns:  []column{{"PR", 'N'}, {"TITLE", 'T'}, {"BRANCH", 'B'}, {"AUTHOR", 'A'}, {"REVIEW", 'V'}, {"UPDATED", 'U'}},
	stateCol: 4,
	empty:    "No open pull requests (or no repository: `:prs owner/name`).",
	hotkeys:  []hotkey{{"enter", "describe"}, {"g", "ask Claude"}, {"o", "open"}, {"y", "json"}, {"R", "refresh"}},
	rows: func(m *tuiModel) []tableRow {
		var rows []tableRow
		for _, p := range m.prs {
			state := prReview(p)
			rows = append(rows, tableRow{
				key:      strconv.Itoa(p.Number),
				state:    state,
				cells:    []string{"#" + strconv.Itoa(p.Number), p.Title, p.HeadRefName, orDash(p.Author.Login), state, age(p.UpdatedAt)},
				sortKeys: []string{fmt.Sprintf("%09d", p.Number), "", "", "", "", fmt.Sprintf("%020d", -p.UpdatedAt.Unix()+1<<62)},
			})
		}
		return rows
	},
}

// prReview is the REVIEW cell: draft, else GitHub's review decision.
func prReview(p ghPR) string {
	switch {
	case p.IsDraft:
		return "draft"
	case p.ReviewDecision == "":
		return "open"
	}
	return strings.ToLower(strings.ReplaceAll(p.ReviewDecision, "_", " "))
}

func (m *tuiModel) pr(key string) *ghPR {
	n, _ := strconv.Atoi(key)
	for i := range m.prs {
		if m.prs[i].Number == n {
			return &m.prs[i]
		}
	}
	return nil
}

func describePR(repo string, p ghPR) []string {
	kv := func(k, v string) string { return fmt.Sprintf("%-16s%s", k+":", v) }
	lines := []string{
		kv("PR", repo+"#"+strconv.Itoa(p.Number)),
		kv("Title", p.Title),
		kv("URL", orDash(p.URL)),
		kv("Author", orDash(p.Author.Login)),
		kv("Branch", p.HeadRefName+" -> "+orDash(p.BaseRefName)),
		kv("Review", prReview(p)),
		kv("Mergeable", orDash(strings.ToLower(p.Mergeable))),
	}
	if !p.UpdatedAt.IsZero() {
		lines = append(lines, kv("Updated", stamp(p.UpdatedAt)+" ("+age(p.UpdatedAt)+" ago)"))
	}
	if b := strings.TrimSpace(p.Body); b != "" {
		lines = append(lines, "")
		lines = append(lines, markdownLines(strings.Split(b, "\n"), 60)...)
	}
	lines = append(lines, "", "g asks Claude to do something on this PR's branch, unattended on a new box",
		"(`boxctl claude --repo "+repo+" --pr "+strconv.Itoa(p.Number)+" --goal ...`): a status summary, a fix, a review.")
	return lines
}

// claudePRArgs is the command line `g` runs in :prs, less the binary.
func claudePRArgs(repo string, pr int, goal, harness string) []string {
	args := claudeRepoArgs(repo, goal, harness)
	return append(args[:3:3], append([]string{"--pr", strconv.Itoa(pr)}, args[3:]...)...)
}

// askPRGoal asks what Claude should do on pull request p, then runs it
// with harness ("" for the default).
func (m tuiModel) askPRGoal(p ghPR, harness string) (tea.Model, tea.Cmd) {
	repo := m.prsRepo
	title := fmt.Sprintf("Ask Claude about %s#%d", repo, p.Number) + harnessSuffix(harness)
	cmd := m.promptText(title, []string{
		"Claude works on the PR's branch unattended on a new box (boxctl claude --pr --goal);",
		"anything it changes is pushed to that branch. :boxes' RUN column and l follow it.",
	}, "e.g. summarize where this stands and what blocks merging", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		if value == "" {
			return m, m.fail("a goal is needed")
		}
		return m.claudeRun(fmt.Sprintf("%s#%d", repo, p.Number), claudePRArgs(repo, p.Number, value, harness), true)
	})
	return m, cmd
}

// currentPR is the PR under the cursor in :prs.
func (m *tuiModel) currentPR() *ghPR {
	if r := m.currentRow(); r != nil {
		return m.pr(r.key)
	}
	return nil
}

// handlePRsKey is :prs' own keys; ok is false for the common ones.
func (m tuiModel) handlePRsKey(key string) (tea.Model, tea.Cmd, bool) {
	switch key {
	case "g":
		if p := m.currentPR(); p != nil {
			pr := *p
			md, cmd := m.askHarness(fmt.Sprintf("%s#%d", m.prsRepo, pr.Number), func(m tuiModel, harness string) (tea.Model, tea.Cmd) {
				return m.askPRGoal(pr, harness)
			})
			return md, cmd, true
		}
		return m, nil, true
	case "o":
		if p := m.currentPR(); p != nil {
			repo, n := m.prsRepo, p.Number
			return m, func() tea.Msg {
				return tuiActionMsg{name: "pr", verb: "open", err: openPR(repo, n)}
			}, true
		}
		return m, nil, true
	case "R", "ctrl+r", "f5":
		return m, tea.Batch(m.refresh(), m.listPRs(), m.spinIfBusy()), true
	}
	return m, nil, false
}
