package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The harness catalog: which harnesses `--harness <name>` can name, with
// a description each -- yours (every directory under ~/.boxctl/harness)
// plus the harness repository's. It's built the way boxctl-images builds its image
// catalog: each harness declares itself in a small metadata file
// (harness.json here, images/<name>/image.json there: label,
// description) and the index is derived from what's on disk, never kept
// as a second list that could drift. A directory without harness.json
// still lists, its description taken from the first line of its
// CLAUDE.md. harness.json isn't copied to the box.

const harnessMetaFile = "harness.json"

// harnessMeta is a harness's harness.json.
type harnessMeta struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// harnessEntry is one row of the catalog.
type harnessEntry struct {
	Name        string   `json:"name"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description"`
	Source      string   `json:"source"` // "yours", or the harness repository's owner/name
	Dir         string   `json:"dir,omitempty"`
	Files       []string `json:"files"`           // what it has: CLAUDE.md, managed-settings.json, agents, ...
	Model       string   `json:"model,omitempty"` // its model.json, as "model/effort"
	Default     bool     `json:"default"`         // ~/.boxctl/harness/default
	Error       string   `json:"error,omitempty"` // why it can't be used
}

// harnessRoot is ~/.boxctl/harness. A variable for tests.
var harnessRoot = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".boxctl", "harness"), nil
}

// describeHarnessDir builds the entry for the harness in dir, in fsys
// terms of plain files so the built-in (embedded) and yours share it.
func describeHarnessDir(name, source string, fsys fs.FS) harnessEntry {
	e := harnessEntry{Name: name, Source: source, Files: []string{}}
	for _, f := range append(append([]string{}, harnessManaged...), harnessConfigEntries...) {
		if _, err := fs.Stat(fsys, f); err == nil {
			e.Files = append(e.Files, f)
		}
	}
	var meta harnessMeta
	if data, err := fs.ReadFile(fsys, harnessMetaFile); err == nil {
		if err := json.Unmarshal(data, &meta); err != nil {
			e.Error = harnessMetaFile + ": " + err.Error()
		}
	}
	e.Label, e.Description = meta.Label, meta.Description
	if e.Description == "" {
		e.Description = mdTitle(fsys, "CLAUDE.md")
	}
	if data, err := fs.ReadFile(fsys, harnessModelFile); err == nil {
		var hm harnessModel
		if json.Unmarshal(data, &hm) == nil {
			e.Model = strings.Trim(hm.Model+"/"+hm.Effort, "/")
		}
	}
	return e
}

// mdTitle is the first non-blank line of a markdown file, less its
// leading #s; "" when unreadable.
func mdTitle(fsys fs.FS, name string) string {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#")); l != "" {
			return l
		}
	}
	return ""
}

// listHarnesses is the catalog: yours under ~/.boxctl/harness by name,
// then the harness repository's (harnessRepo, main) that you haven't
// shadowed with a directory of your own, as --harness resolves a name.
// A repository that can't be reached only leaves its own out: err says
// why, and yours are still returned.
func listHarnesses(ctx context.Context) ([]harnessEntry, error) {
	var out []harnessEntry
	root, err := harnessRoot()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	have := map[string]bool{}
	for _, d := range ents {
		// Follow symlinks: a harness may be linked in from a checkout.
		dir := filepath.Join(root, d.Name())
		if st, err := os.Stat(dir); err != nil || !st.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		e := describeHarnessDir(d.Name(), "yours", os.DirFS(dir))
		e.Dir = dir
		e.Default = d.Name() == "default"
		if _, err := loadHarness(dir); err != nil && e.Error == "" {
			e.Error = strings.TrimPrefix(err.Error(), "harness "+dir+": ")
		}
		have[d.Name()] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	co, err := fetchHarnessRepo(ctx, harnessRepo(), "")
	if err != nil {
		return out, err
	}
	names := harnessesIn(co.dir)
	sort.Strings(names)
	for _, n := range names {
		if have[n] {
			continue
		}
		dir := filepath.Join(co.dir, n)
		e := describeHarnessDir(n, harnessRepo(), os.DirFS(dir))
		e.Dir = dir
		if _, err := loadHarness(dir); err != nil && e.Error == "" {
			e.Error = strings.TrimPrefix(err.Error(), "harness "+dir+": ")
		}
		out = append(out, e)
	}
	return out, nil
}

var harnessCmd = &cobra.Command{
	Use:     "harness",
	Aliases: []string{"harnesses"},
	Short:   "List the harnesses `claude --harness` can use",
	Long: `A harness is a directory of rules for the Claude on a box (CLAUDE.md,
managed-settings.json, agents/, skills/, commands/). Every directory under
~/.boxctl/harness is listed by name, then those of the harness repository
(boxctl-io/harnesses, pr-only among them) that yours don't shadow.
Describe one with a harness.json beside its files:

  {"label": "Docs only", "description": "Edits under docs/, nothing else"}

Without it the description is the first line of the harness's CLAUDE.md.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error { return runHarnessLs(cmd) },
}

var harnessLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List the available harnesses",
	Args:    cobra.NoArgs,
	RunE:    func(cmd *cobra.Command, args []string) error { return runHarnessLs(cmd) },
}

func runHarnessLs(cmd *cobra.Command) error {
	asJSON, err := wantJSON(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	hs, err := listHarnesses(ctx)
	if err != nil {
		if len(hs) == 0 {
			return err
		}
		_, _ = fmt.Fprintln(os.Stderr, "warning:", err)
	}
	if asJSON {
		return printJSON(hs)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tSOURCE\tFILES\tDESCRIPTION"); err != nil {
		return err
	}
	for _, h := range hs {
		name := h.Name
		if h.Default {
			name += " (default)"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, h.Source, orDash(strings.Join(h.Files, ",")), harnessDesc(h)); err != nil {
			return err
		}
	}
	return w.Flush()
}

// harnessDesc is the description cell, or why the harness can't be used.
func harnessDesc(h harnessEntry) string {
	if h.Error != "" {
		return "(unusable: " + h.Error + ")"
	}
	return orDash(h.Description)
}

func init() {
	addOutputFlag(harnessCmd)
	addOutputFlag(harnessLsCmd)
	harnessCmd.AddCommand(harnessLsCmd)
	rootCmd.AddCommand(harnessCmd)
}

// harnessesView is `:harnesses` on the board: the same catalog.
var harnessesView = &tableView{
	name:     "harnesses",
	aliases:  []string{"harness", "hn"},
	title:    "Harnesses",
	resource: "harnesscat",
	columns:  []column{{"NAME", 'N'}, {"SOURCE", 'S'}, {"FILES", 'F'}, {"DESCRIPTION", 'D'}},
	stateCol: -1,
	empty:    "No harnesses.",
	onDemand: true,
	hotkeys:  []hotkey{{"enter", "describe"}, {"y", "json"}, {"R", "refresh"}},
	rows: func(m *tuiModel) []tableRow {
		var rows []tableRow
		for _, h := range m.catalog {
			name := h.Name
			if h.Default {
				name += " (default)"
			}
			rows = append(rows, tableRow{key: h.Name, cells: []string{name, h.Source, orDash(strings.Join(h.Files, ",")), harnessDesc(h)}})
		}
		return rows
	},
}

// listHarnessesCmd is list("harnesscat"): yours, then the repository's
// (a download only for a commit not cached).
func listHarnessesCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		msg := tuiListMsg{resource: "harnesscat"}
		msg.catalog, msg.err = listHarnesses(ctx)
		return msg
	}
}

func (m *tuiModel) harness(name string) *harnessEntry {
	for i := range m.catalog {
		if m.catalog[i].Name == name {
			return &m.catalog[i]
		}
	}
	return nil
}

func describeHarness(h harnessEntry) []string {
	kv := func(k, v string) string { return fmt.Sprintf("%-16s%s", k+":", v) }
	lines := []string{kv("Name", h.Name)}
	if h.Label != "" {
		lines = append(lines, kv("Label", h.Label))
	}
	lines = append(lines, kv("Source", h.Source), kv("Description", orDash(h.Description)))
	if h.Dir != "" {
		lines = append(lines, kv("Directory", h.Dir))
	}
	lines = append(lines, kv("Files", orDash(strings.Join(h.Files, ", "))))
	if h.Model != "" {
		lines = append(lines, kv("Model", h.Model))
	}
	if h.Default {
		lines = append(lines, kv("Default", "yes: used when --harness isn't given"))
	}
	if h.Error != "" {
		lines = append(lines, kv("Problem", h.Error))
	}
	return append(lines, "", "boxctl claude --harness "+h.Name+" ...")
}
