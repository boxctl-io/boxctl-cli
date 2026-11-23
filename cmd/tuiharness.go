package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The harness a goal run gets, asked before its goal: in :backlog's and
// :repos' a (a backlog item, every run of it) and :repos' g.

// listHarnesses is list("harnesses"): yours and the harness
// repository's, once a session (harnessChoices asks GitHub for main's
// commit, and downloads only a new one).
func (m *tuiModel) listHarnesses() tea.Cmd {
	if m.loading["harnesses"] || m.loaded["harnesses"] {
		return nil
	}
	m.loading["harnesses"] = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		msg := tuiListMsg{resource: "harnesses"}
		msg.harnesses, msg.err = harnessChoices(ctx)
		return msg
	}
}

// harnessPickChoices is what the harness prompt offers: what a goal run
// gets without --harness first (your default, else pr-only), so enter
// alone keeps it; then the others; none last.
func harnessPickChoices(m *tuiModel) []string {
	first := harnessPROnly
	if home, err := os.UserHomeDir(); err == nil {
		if st, err := os.Stat(filepath.Join(home, ".boxctl", "harness", "default")); err == nil && st.IsDir() {
			first = "default"
		}
	}
	out := []string{first}
	for _, h := range m.harnesses {
		if h != first {
			out = append(out, h)
		}
	}
	return append(out, harnessNone)
}

// askHarness asks which harness a goal on repo runs with, then calls
// next with it: "" for the default a goal run gets anyway, so a launch
// (or a backlog item) only names a harness that was chosen.
func (m tuiModel) askHarness(repo string, next func(m tuiModel, harness string) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	cmd := m.prompt("Harness for the goal on "+repo, []string{
		"Rules for Claude on the box (boxctl claude --harness). Enter keeps the first:",
		"what a goal gets anyway. Yours under ~/.boxctl/harness, then " + harnessRepo() + "'s;",
		"name@ref for a branch or tag of it. Up/down to select, tab to complete.",
	}, "name", func(m tuiModel, value string) (tea.Model, tea.Cmd) {
		if value == harnessPickChoices(&m)[0] {
			value = ""
		}
		return next(m, value)
	})
	if m.dialog != nil && m.dialog.input != nil {
		m.dialog.choices = harnessPickChoices
		m.dialog.typed = func(v string) bool { return strings.ContainsAny(v, `@/\`) }
	}
	return m, tea.Batch(cmd, m.listHarnesses(), m.spinIfBusy())
}

// harnessSuffix names a chosen harness in the goal prompt's title.
func harnessSuffix(harness string) string {
	if harness == "" {
		return ""
	}
	return " (harness " + harness + ")"
}
