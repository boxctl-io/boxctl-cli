package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/backlog"
	"github.com/boxctl-io/boxctl-cli/internal/client"
)

// A box's run kind comes from its boxctl.run label: an unattended run's
// box carries the kind itself, and every box an attended run made is one
// kind, interactive -- so `boxctl runs` and ls name runs the same way the
// backlog does.
func TestRunKind(t *testing.T) {
	for run, want := range map[string]string{
		"code": "code", "ci_fix": "ci_fix", "conflict_fix": "conflict_fix", "change": "change",
		"session": runInteractive, "box": runInteractive, "repo": runInteractive,
		"": "",
	} {
		vm := client.VM{Labels: map[string]string{"boxctl.run": run}}
		if got := runKind(vm); got != want {
			t.Errorf("runKind(boxctl.run=%q) = %q, want %q", run, got, want)
		}
		if unattendedBox(vm) != (want != runInteractive && want != "") {
			t.Errorf("unattendedBox(boxctl.run=%q) = %v", run, unattendedBox(vm))
		}
	}
}

// `boxctl runs` lists every run, not just the backlog's: a backlog run
// once (from the backlog file, not again from its labeled box), each box
// a run outside the backlog made, and nothing for a box no run made. Runs
// going come before ended ones, newest first within each.
func TestAllRuns(t *testing.T) {
	now := time.Now()
	ended := now.Add(-time.Minute)
	q := &backlog.Backlog{Items: []*backlog.Item{{ID: 4, Repo: "o/r", Runs: []backlog.Run{
		{Kind: backlog.Code, Box: "q4-code-1", Started: now.Add(-3 * time.Hour), Ended: &ended, Outcome: backlog.OutcomeFailed},
		{Kind: backlog.CIFix, Box: "q4-ci-2", Started: now.Add(-2 * time.Hour), Launched: true},
	}}}}
	vms := []client.VM{
		{Name: "q4-ci-2", State: "running", CreatedAt: now.Add(-2 * time.Hour), Labels: map[string]string{"boxctl.run": "ci_fix", "boxctl.backlog": "4", "boxctl.repo": "o/r"}, RunStatus: json.RawMessage(`{"status":"working"}`)},
		{Name: "goal-r-fix-a1b2", State: "running", CreatedAt: now.Add(-time.Hour), Labels: map[string]string{"boxctl.run": "code", "boxctl.repo": "o/r"}, RunStatus: json.RawMessage(`{"status":"done"}`)},
		{Name: "claude-app", State: "paused", CreatedAt: now.Add(-30 * time.Minute), Labels: map[string]string{"boxctl.run": "box", "boxctl.project": "/w/app"}},
		{Name: "web", State: "running", CreatedAt: now},
	}
	var got []string
	for _, r := range allRuns(q, vms) {
		got = append(got, strings.Join(r.cells()[:5], " "))
	}
	want := []string{
		"- app interactive claude-app paused",
		"#4 o/r ci_fix q4-ci-2 working",
		"- o/r code goal-r-fix-a1b2 done",
		"#4 o/r code q4-code-1 failed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("runs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Without the box listing, the backlog's runs are still there.
	if rows := allRuns(q, nil); len(rows) != 2 || rows[0].state != "?" {
		t.Errorf("no listing: %+v", rows)
	}
}
