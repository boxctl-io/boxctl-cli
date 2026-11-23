package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHarnessFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadHarness(t *testing.T) {
	dir := t.TempDir()
	writeHarnessFile(t, dir, "CLAUDE.md", "never merge")
	writeHarnessFile(t, dir, "managed-settings.json", `{"permissions":{"deny":["Bash(gh pr merge:*)"]}}`)
	writeHarnessFile(t, dir, "commands/open-pr.md", "open a PR")
	writeHarnessFile(t, dir, "skills/x/node_modules/junk.js", "skipped")
	writeHarnessFile(t, dir, "README.txt", "not part of a harness")

	h, err := loadHarness(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.managed) != 2 {
		t.Errorf("managed = %v, want CLAUDE.md and managed-settings.json", h.managed)
	}
	if len(h.config) != 1 || h.config[0] != "commands/open-pr.md" {
		t.Errorf("config = %v, want only commands/open-pr.md", h.config)
	}
}

func TestLoadHarnessRejects(t *testing.T) {
	empty := t.TempDir()
	if _, err := loadHarness(empty); err == nil {
		t.Error("an empty directory is not a harness")
	}
	bad := t.TempDir()
	writeHarnessFile(t, bad, "managed-settings.json", `{not json`)
	if _, err := loadHarness(bad); err == nil {
		t.Error("invalid managed-settings.json should be refused before a box exists")
	}
}

func TestResolveHarnessName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	newFakeGitHub(t, prOnlyFiles)
	ctx := context.Background()
	mine := filepath.Join(home, ".boxctl", "harness", "pr-only")
	writeHarnessFile(t, mine, "CLAUDE.md", "x")
	if got, _, err := resolveHarness(ctx, "pr-only"); err != nil || got != mine {
		t.Errorf("yours wins: %q, %v", got, err)
	}
	if got, label, err := resolveHarness(ctx, "pr-only@main"); err != nil || got == mine || !strings.HasPrefix(label, "pr-only (o/h@main ") {
		t.Errorf("a ref names the repository's: %q %q, %v", got, label, err)
	}
	if got, _, err := resolveHarness(ctx, "other"); err != nil || filepath.Base(got) != "other" {
		t.Errorf("the repository's: %q, %v", got, err)
	}
	if _, _, err := resolveHarness(ctx, "nope"); err == nil {
		t.Error("an unknown name should fail")
	}
	if _, _, err := resolveHarness(ctx, "./nope"); err == nil {
		t.Error("a missing path should fail")
	}
}

func isRemotePROnly(h *harness) bool {
	return h != nil && strings.HasPrefix(h.label, "pr-only (o/h@main ")
}

func TestPrepareHarnessChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	gh := newFakeGitHub(t, prOnlyFiles)
	t.Cleanup(func() { claudeHarnessArg, claudeGoal, claudeIssue, claudeHarness = "", "", 0, nil })
	pick := func(arg, goal string) *harness {
		t.Helper()
		claudeHarnessArg, claudeGoal, claudeIssue = arg, goal, 0
		if err := prepareHarness(context.Background()); err != nil {
			t.Fatal(err)
		}
		return claudeHarness
	}

	if pick("", "") != nil {
		t.Error("a plain run with no default gets no harness")
	}
	if h := pick("", "fix it"); !isRemotePROnly(h) {
		t.Errorf("--goal gets the repository's pr-only, got %+v", h)
	}
	if pick("none", "fix it") != nil {
		t.Error("--harness none turns the goal default off")
	}
	// --issue is a goal that only names an issue: unattended all the same.
	claudeHarnessArg, claudeGoal, claudeIssue = "", "", 7
	if err := prepareHarness(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !isRemotePROnly(claudeHarness) {
		t.Errorf("--issue gets pr-only, got %+v", claudeHarness)
	}
	if h := pick(harnessPROnly, ""); !isRemotePROnly(h) {
		t.Errorf("--harness pr-only is the repository's, got %+v", h)
	}
	mine := filepath.Join(home, ".boxctl", "harness", harnessPROnly)
	writeHarnessFile(t, mine, "CLAUDE.md", "mine")
	if h := pick(harnessPROnly, ""); h == nil || h.dir != mine {
		t.Errorf("your own pr-only beats the repository's, got %+v", h)
	}
	if h := pick("", "fix it"); h == nil || h.dir != mine {
		t.Errorf("your own pr-only is a goal's too, got %+v", h)
	}
	_ = os.RemoveAll(mine)

	// Nothing cached and GitHub down: an unattended run fails rather
	// than run without its guardrail.
	gh.down.Store(true)
	_ = os.RemoveAll(filepath.Join(home, ".boxctl", "harness-cache"))
	claudeHarnessArg, claudeGoal = "", "fix it"
	if err := prepareHarness(context.Background()); err == nil {
		t.Error("a goal without pr-only should fail")
	}
	gh.down.Store(false)

	def := filepath.Join(home, ".boxctl", "harness", "default")
	writeHarnessFile(t, def, "CLAUDE.md", "mine")
	if h := pick("", ""); h == nil || h.dir != def {
		t.Errorf("a default directory applies to every run, got %+v", h)
	}
	if h := pick("", "fix it"); h == nil || h.dir != def {
		t.Errorf("your default beats pr-only for --goal, got %+v", h)
	}
	if pick("none", "") != nil {
		t.Error("--harness none turns your default off")
	}
	other := t.TempDir()
	writeHarnessFile(t, other, "CLAUDE.md", "x")
	if h := pick(other, "fix it"); h == nil || h.dir != other {
		t.Errorf("an explicit --harness wins, got %+v", h)
	}
}
