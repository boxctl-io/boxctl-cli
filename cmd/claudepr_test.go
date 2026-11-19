package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPRFlags(t *testing.T) {
	for _, c := range []struct {
		pr, issue int
		repos     []string
		want      string
	}{
		{0, 0, nil, ""},
		{7, 0, []string{"o/r"}, ""},
		{-1, 0, []string{"o/r"}, "use a pull request number"},
		{7, 3, []string{"o/r"}, "can't be combined"},
		{7, 0, []string{"o/a", "o/b"}, "single --repo"},
	} {
		err := checkPRFlags(c.pr, c.repos, c.issue)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("checkPRFlags(%d, %v, %d) = %v, want %q", c.pr, c.repos, c.issue, err, c.want)
		}
	}
}

func TestPRBranch(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/repos/o/r/pulls/1":
			_, _ = w.Write([]byte(`{"number":1,"state":"open","head":{"ref":"fix/x","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}`))
		case "/repos/o/r/pulls/2":
			_, _ = w.Write([]byte(`{"number":2,"state":"closed","head":{"ref":"y","repo":{"full_name":"o/r"}}}`))
		case "/repos/o/r/pulls/3":
			_, _ = w.Write([]byte(`{"number":3,"state":"open","head":{"ref":"main","repo":{"full_name":"fork/r"}}}`))
		case "/repos/o/r/pulls/4":
			_, _ = w.Write([]byte(`{"number":4,"state":"open","head":{"ref":"z","repo":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	old := githubAPI
	githubAPI = gh.URL
	defer func() { githubAPI = old }()

	ctx := context.Background()
	if b, err := prBranch(ctx, "o/r", 1, "tok"); err != nil || b != "fix/x" {
		t.Errorf("open PR: %q, %v", b, err)
	}
	for n, want := range map[int]string{2: "is closed", 3: "fork", 4: "fork", 5: "no pull request #5"} {
		if _, err := prBranch(ctx, "o/r", n, "tok"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("#%d: %v, want %q", n, err, want)
		}
	}
}

func TestCloneBranchScript(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	git := []string{"-c", "user.name=t", "-c", "user.email=t@t"}
	run(t, root, "git", "init", "-q", "-b", "main", origin)
	write(t, filepath.Join(origin, "a.txt"), "a")
	run(t, origin, append([]string{"git"}, append(git, "add", ".")...)...)
	run(t, origin, append([]string{"git"}, append(git, "commit", "-q", "-m", "init")...)...)
	run(t, origin, "git", "checkout", "-q", "-b", "fix/x")
	write(t, filepath.Join(origin, "b.txt"), "b")
	run(t, origin, append([]string{"git"}, append(git, "add", ".")...)...)
	run(t, origin, append([]string{"git"}, append(git, "commit", "-q", "-m", "fix")...)...)

	dir := filepath.Join(root, "box", "n")
	script := cloneBranchScript(origin, dir, "fix/x")
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if head := strings.TrimSpace(sh(t, "git -C "+dir+" rev-parse --abbrev-ref HEAD")); head != "fix/x" {
		t.Errorf("HEAD = %q", head)
	}
	// The default branch is there to merge, and plain git push has an upstream.
	if err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", "origin/main").Run(); err != nil {
		t.Errorf("origin/main wasn't fetched: %v", err)
	}
	if up := strings.TrimSpace(sh(t, "git -C "+dir+" rev-parse --abbrev-ref @{u}")); up != "origin/fix/x" {
		t.Errorf("upstream = %q", up)
	}
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err == nil || !strings.Contains(string(out), "already exists") {
		t.Errorf("second clone: %v %s", err, out)
	}
}

func TestPRGoalPrompt(t *testing.T) {
	p := prGoalPrompt("make CI pass", "o/r", 7, "fix/x")
	for _, want := range []string{"make CI pass\n", "o/r", "fix/x", "#7", "never force-push", "don't open a new pull request", "gh pr comment 7"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}
