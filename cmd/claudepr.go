package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// boxctl claude --repo owner/name --pr N [--task "..."]: a run on the
// branch of an open pull request instead of a new one -- a follow-up:
// fix its CI, resolve a conflict with the default branch, address a
// review. The box clones that branch (and fetches the others, so
// origin/<default> is there to merge), Claude pushes to it, and no new
// pull request is opened: the runner's `gh pr list --head <branch>`
// finds the one that's there, so a goal still ends "done" with its URL.
//
// Only a pull request whose branch is in the repository itself: a fork's
// branch can't be pushed with the token the box has.

// claudePR is --pr.
var claudePR int

// pullRequest is what --pr needs of one, from GitHub.
type pullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// lookupPR asks GitHub for pull request n of repo, with token when there
// is one. A variable so tests stand in for GitHub.
var lookupPR = func(ctx context.Context, repo string, n int, token string) (*pullRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/repos/"+repo+"/pulls/"+strconv.Itoa(n), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking GitHub for %s#%d: %w", repo, n, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s has no pull request #%d (or the token can't see it)", repo, n)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("asking GitHub for %s#%d: %s", repo, n, resp.Status)
	}
	var pr pullRequest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&pr); err != nil {
		return nil, fmt.Errorf("reading GitHub's answer for %s#%d: %w", repo, n, err)
	}
	return &pr, nil
}

// prBranch is the branch a --pr run works on: pull request n's, checked
// to be open and in repo itself.
func prBranch(ctx context.Context, repo string, n int, token string) (string, error) {
	pr, err := lookupPR(ctx, repo, n, token)
	if err != nil {
		return "", err
	}
	if pr.State != "open" {
		return "", fmt.Errorf("%s#%d is %s: --pr works on an open pull request", repo, n, pr.State)
	}
	if pr.Head.Repo == nil || pr.Head.Repo.FullName != repo || pr.Head.Ref == "" {
		return "", fmt.Errorf("%s#%d's branch is in a fork: --pr can only push to a branch of %s itself", repo, n, repo)
	}
	return pr.Head.Ref, nil
}

// checkPRFlags is what --pr can't go with.
func checkPRFlags(pr int, repos []string, issue int) error {
	switch {
	case pr == 0:
		return nil
	case pr < 0:
		return fmt.Errorf("--pr %d: use a pull request number", pr)
	case issue != 0:
		return errors.New("--pr and --issue can't be combined: --pr works on a pull request that's there, --issue opens one")
	case len(repos) != 1:
		return errors.New("--pr is a pull request of one repository: pass a single --repo")
	}
	return nil
}

// cloneBranchScript clones url's branch into dir, for a --pr run. Like
// cloneScript it refuses a directory that's there and maps every branch,
// and it then fetches them all: a conflict fix merges origin/<default>.
func cloneBranchScript(url, dir, branch string) string {
	return `set -e
[ ! -e ` + shellQuote(dir) + ` ] || { echo "` + dir + ` already exists on this box" >&2; exit 1; }
mkdir -p "$(dirname ` + shellQuote(dir) + `)"
GIT_TERMINAL_PROMPT=0 git clone -q --single-branch --branch ` + shellQuote(branch) + ` ` + shellQuote(url) + ` ` + shellQuote(dir) + `
git -C ` + shellQuote(dir) + ` config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*'
GIT_TERMINAL_PROMPT=0 git -C ` + shellQuote(dir) + ` fetch -q origin`
}

// prGoalPrompt is goalPrompt for a --pr run: the same goal, ending in a
// push to the pull request's branch rather than a new pull request.
func prGoalPrompt(goal, repo string, n int, branch string) string {
	return goal + `

---
You're running unattended on a box, in a clone of ` + repo + ` on the branch ` + branch + ` of its open pull request #` + strconv.Itoa(n) + ` (see it with gh pr view ` + strconv.Itoa(n) + `; every other branch is fetched too, as origin/<name>). Nobody can answer questions, so make reasonable decisions and say what you assumed. Work on this branch: commit, then push it with a plain git push -- never force-push, never rebase what's already pushed, and don't open a new pull request. If you merge another branch in to resolve a conflict, merge (git merge origin/<branch>), don't rebase. When you're done, add a comment to the pull request (gh pr comment ` + strconv.Itoa(n) + `) saying what you changed and anything you assumed. If you can't get anywhere, push nothing: say why in your final answer.`
}
