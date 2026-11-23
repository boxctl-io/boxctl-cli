package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListHarnesses(t *testing.T) {
	root := t.TempDir()
	harnessRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() {
		harnessRoot = func() (string, error) {
			home, err := os.UserHomeDir()
			return filepath.Join(home, ".boxctl", "harness"), err
		}
	})
	mk := func(name, file, body string) {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("docs", "CLAUDE.md", "# Docs only rules\n\nbody")
	mk("docs", "harness.json", `{"description":"Edits docs"}`)
	mk("default", "CLAUDE.md", "\n## Fallback rules\n")
	mk("empty", "notes.txt", "x")

	hs, err := listHarnesses()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]harnessEntry{}
	var order []string
	for _, h := range hs {
		got[h.Name] = h
		order = append(order, h.Name)
	}
	if want := []string{"pr-only", "default", "docs", "empty"}; len(order) != 4 || order[0] != want[0] || order[1] != want[1] || order[3] != want[3] {
		t.Fatalf("order %v, want %v", order, want)
	}
	if h := got["pr-only"]; h.Source != "built-in" || h.Description == "" {
		t.Errorf("built-in: %+v", h)
	}
	if h := got["docs"]; h.Description != "Edits docs" || h.Source != "yours" || h.Error != "" {
		t.Errorf("docs: %+v", h)
	}
	if h := got["default"]; !h.Default || h.Description != "Fallback rules" {
		t.Errorf("default: %+v", h)
	}
	if got["empty"].Error == "" {
		t.Errorf("a directory with no rules should be unusable: %+v", got["empty"])
	}
	// One of your own named pr-only replaces the built-in.
	mk("pr-only", "CLAUDE.md", "# Mine")
	hs, _ = listHarnesses()
	n := 0
	for _, h := range hs {
		if h.Name == "pr-only" {
			n++
			if h.Source != "yours" {
				t.Errorf("own pr-only should win: %+v", h)
			}
		}
	}
	if n != 1 {
		t.Errorf("pr-only listed %d times", n)
	}
}
