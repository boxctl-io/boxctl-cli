package cmd

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// The harness catalog: which harnesses `--harness <name>` can name, with
// a description each -- the built-in pr-only plus every directory under
// ~/.boxctl/harness. It's built the way boxctl-images builds its image
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
	Source      string   `json:"source"` // "built-in" or "yours"
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

// listHarnesses is the catalog: the built-in pr-only (unless one of your
// own has that name, which wins as it does for --harness), then yours by
// name.
func listHarnesses() ([]harnessEntry, error) {
	var out []harnessEntry
	root, err := harnessRoot()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	hasOwnPROnly := false
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
		if d.Name() == harnessPROnly {
			hasOwnPROnly = true
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if !hasOwnPROnly {
		sub, err := fs.Sub(builtinPROnly, "harness-pr-only")
		if err != nil {
			return nil, err
		}
		out = append([]harnessEntry{describeHarnessDir(harnessPROnly, "built-in", sub)}, out...)
	}
	return out, nil
}

var harnessCmd = &cobra.Command{
	Use:     "harness",
	Aliases: []string{"harnesses"},
	Short:   "List the harnesses `claude --harness` can use",
	Long: `A harness is a directory of rules for the Claude on a box (CLAUDE.md,
managed-settings.json, agents/, skills/, commands/). Besides the built-in
pr-only one, every directory under ~/.boxctl/harness is listed by name.
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
	hs, err := listHarnesses()
	if err != nil {
		return err
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
	resource: "harnesses",
	columns:  []column{{"NAME", 'N'}, {"SOURCE", 'S'}, {"FILES", 'F'}, {"DESCRIPTION", 'D'}},
	stateCol: -1,
	empty:    "No harnesses.",
	hotkeys:  []hotkey{{"enter", "describe"}, {"y", "json"}, {"R", "refresh"}},
	rows: func(m *tuiModel) []tableRow {
		var rows []tableRow
		for _, h := range m.harnesses {
			name := h.Name
			if h.Default {
				name += " (default)"
			}
			rows = append(rows, tableRow{key: h.Name, cells: []string{name, h.Source, orDash(strings.Join(h.Files, ",")), harnessDesc(h)}})
		}
		return rows
	},
}

// listHarnessesCmd is list("harnesses"): a local read, so no loading flag.
func listHarnessesCmd() tea.Cmd {
	return func() tea.Msg {
		msg := tuiListMsg{resource: "harnesses"}
		msg.harnesses, msg.err = listHarnesses()
		return msg
	}
}

func (m *tuiModel) harness(name string) *harnessEntry {
	for i := range m.harnesses {
		if m.harnesses[i].Name == name {
			return &m.harnesses[i]
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
