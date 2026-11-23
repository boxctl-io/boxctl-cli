package cmd

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/config"
)

// Harnesses by name come from a public GitHub repository
// (defaultHarnessRepo): each top-level directory is one. A run asks
// GitHub which commit the ref (main by default) is at, and downloads
// the repository's tarball only for a commit it hasn't seen, into
// ~/.boxctl/harness-cache/<owner>/<repo>/<sha>. When GitHub can't be
// asked, the commit the ref was last at is used, if it's cached.

const (
	defaultHarnessRepo = "boxctl-io/harnesses"
	defaultHarnessRef  = "main"
	envHarnessRepo     = "BOXCTL_HARNESS_REPO"
	// harnessTarballMax caps a harness repository's download.
	harnessTarballMax = 20 << 20
)

// Where GitHub is; variables so tests point them at a fake.
var (
	githubAPIBase      = "https://api.github.com"
	githubCodeloadBase = "https://codeload.github.com"
)

var (
	harnessNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	commitPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// parseHarnessName splits name[@ref]; ok is false when arg isn't one
// (a path, say).
func parseHarnessName(arg string) (name, ref string, ok bool) {
	name, ref, hasRef := strings.Cut(arg, "@")
	if !harnessNamePattern.MatchString(name) || (hasRef && (ref == "" || strings.ContainsAny(ref, " \t\n:~^?*[\\"))) {
		return "", "", false
	}
	return name, ref, true
}

// harnessRepo is the repository harness names come from:
// BOXCTL_HARNESS_REPO, harness_repo in the config, else the default.
func harnessRepo() string {
	if r := strings.TrimSpace(os.Getenv(envHarnessRepo)); r != "" {
		return r
	}
	if cfg, err := config.Load(); err == nil && cfg.HarnessRepo != "" {
		return cfg.HarnessRepo
	}
	return defaultHarnessRepo
}

// harnessCacheDir is where repo's commits are kept.
func harnessCacheDir(repo string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".boxctl", "harness-cache", filepath.FromSlash(repo)), nil
}

// remoteCheckout is one commit of a harness repository, on disk.
type remoteCheckout struct {
	repo, ref, sha string
	dir            string // the repository's root at sha
	stale          bool   // GitHub couldn't be asked: the commit ref was last at
}

// short is how a checkout is named in output: repo@ref (sha7).
func (c *remoteCheckout) short() string {
	sha := c.sha
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return fmt.Sprintf("%s@%s %s", c.repo, c.ref, sha)
}

// fetchHarnessRepo makes sure the commit repo's ref is at is in the
// cache and returns it.
func fetchHarnessRepo(ctx context.Context, repo, ref string) (*remoteCheckout, error) {
	if _, _, err := parseRepo(repo); err != nil {
		return nil, fmt.Errorf("harness repository %q: use owner/name", repo)
	}
	if ref == "" {
		ref = defaultHarnessRef
	}
	cache, err := harnessCacheDir(repo)
	if err != nil {
		return nil, err
	}
	refFile := filepath.Join(cache, "refs", url.PathEscape(ref))
	co := &remoteCheckout{repo: repo, ref: ref}
	sha, err := githubCommit(ctx, repo, ref)
	if err != nil {
		last, rerr := os.ReadFile(refFile)
		if rerr == nil {
			co.sha = strings.TrimSpace(string(last))
			co.dir = filepath.Join(cache, co.sha)
			if st, serr := os.Stat(co.dir); serr == nil && st.IsDir() {
				co.stale = true
				return co, nil
			}
		}
		return nil, fmt.Errorf("harnesses from %s@%s: %w", repo, ref, err)
	}
	co.sha, co.dir = sha, filepath.Join(cache, sha)
	if st, err := os.Stat(co.dir); err != nil || !st.IsDir() {
		if err := downloadHarnessRepo(ctx, repo, sha, co.dir); err != nil {
			return nil, fmt.Errorf("harnesses from %s@%s: %w", repo, ref, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(refFile), 0o755); err == nil {
		_ = os.WriteFile(refFile, []byte(sha+"\n"), 0o644)
	}
	return co, nil
}

// githubGet is a GET to GitHub with the token from BOXCTL_GITHUB_TOKEN,
// if set (only for the higher rate limit: the repository is public).
func githubGet(ctx context.Context, u, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if t := strings.TrimSpace(os.Getenv("BOXCTL_GITHUB_TOKEN")); t != "" && strings.HasPrefix(u, githubAPIBase) {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
			return nil, errHarnessNotFound
		}
		return nil, fmt.Errorf("GitHub answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// errHarnessNotFound: GitHub has no such repository, ref or commit.
var errHarnessNotFound = errors.New("no such repository or ref on GitHub")

// githubCommit is the commit ref is at in repo.
func githubCommit(ctx context.Context, repo, ref string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := githubGet(ctx, githubAPIBase+"/repos/"+repo+"/commits/"+url.PathEscape(ref), "application/vnd.github.sha")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(b))
	if !commitPattern.MatchString(sha) {
		return "", fmt.Errorf("GitHub's answer for %s isn't a commit: %q", ref, sha)
	}
	return sha, nil
}

// downloadHarnessRepo puts repo at sha in dst, by way of a temp
// directory beside it, so dst exists only once it's complete.
func downloadHarnessRepo(ctx context.Context, repo, sha, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := githubGet(ctx, githubCodeloadBase+"/"+repo+"/tar.gz/"+sha, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), ".download-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, harnessTarballMax))
	if err != nil {
		return err
	}
	if _, _, err := extractTar(gz, tmp); err != nil {
		return err
	}
	// GitHub's tarball holds one directory, <owner>-<repo>-<sha7>.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	var root string
	for _, e := range entries {
		if e.IsDir() {
			if root != "" {
				return errors.New("the tarball has more than one top-level directory")
			}
			root = filepath.Join(tmp, e.Name())
		}
	}
	if root == "" {
		return errors.New("the tarball is empty")
	}
	if err := os.Rename(root, dst); err != nil {
		if st, serr := os.Stat(dst); serr == nil && st.IsDir() {
			return nil // another run got there first
		}
		return err
	}
	return nil
}

// fetchHarness is the directory of harness name in the harness
// repository at ref, and how to name it.
func fetchHarness(ctx context.Context, name, ref string) (dir, label string, err error) {
	co, err := fetchHarnessRepo(ctx, harnessRepo(), ref)
	if err != nil {
		return "", "", err
	}
	dir = filepath.Join(co.dir, name)
	if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
		return "", "", fmt.Errorf("--harness %s: %w (nor is it in %s, which has %s)", name, errNoHarness, co.short(), strings.Join(harnessesIn(co.dir), ", "))
	}
	if co.stale {
		sayWarn("Couldn't ask GitHub for %s@%s's latest commit: using %s, the one last fetched", co.repo, co.ref, co.short())
	}
	return dir, name + " (" + co.short() + ")", nil
}

// harnessesIn are the harness directories in a harness repository's
// root: every top-level directory not starting with a dot.
func harnessesIn(root string) []string {
	entries, _ := os.ReadDir(root)
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// harnessChoices are the harnesses a run can name: yours under
// ~/.boxctl/harness, then the harness repository's main ones, sorted
// within each, without duplicates. A repository that can't be reached
// only leaves its own out (err says why).
func harnessChoices(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(names []string) {
		sort.Strings(names)
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(harnessesIn(filepath.Join(home, ".boxctl", "harness")))
	}
	co, err := fetchHarnessRepo(ctx, harnessRepo(), "")
	if err == nil {
		add(harnessesIn(co.dir))
	}
	return out, err
}
