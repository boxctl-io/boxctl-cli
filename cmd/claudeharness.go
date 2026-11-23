package cmd

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/boxctl-io/boxctl-cli/internal/client"
)

// --harness: rules for Claude on the box that don't belong in your own
// ~/.claude, which also steers the Claude on this machine. A harness is
// a directory:
//
//	CLAUDE.md             -> /etc/claude-code/CLAUDE.md (managed memory:
//	                         loaded in every session, can't be excluded)
//	managed-settings.json -> /etc/claude-code/managed-settings.json (the
//	                         highest-precedence settings: permission
//	                         rules the user and project settings can't undo)
//	agents/ skills/ commands/ -> ~/.claude, on top of your own
//
// Everything is checked before any box is created.

const harnessManagedDir = "/etc/claude-code"

// harnessManaged are the harness files that go to harnessManagedDir.
var harnessManaged = []string{"CLAUDE.md", "managed-settings.json"}

// harnessConfigEntries are the harness entries that go to ~/.claude.
var harnessConfigEntries = []string{"agents", "skills", "commands"}

type harness struct {
	dir     string   // where it's read from
	label   string   // how it's named in output: dir, or "built-in pr-only"
	managed []string // files for harnessManagedDir, relative to dir
	config  []string // files for ~/.claude, relative to dir
	size    int64
	model   *harnessModel // its model.json (harnessModelFile), if any
	// modelFrom names the harness model came from when it isn't this
	// one: a model-only harness under the built-in (withRules).
	modelFrom string
}

// rules says whether the harness has anything for the box -- rules for
// Claude -- rather than only boxctl's model.json.
func (h *harness) rules() bool { return len(h.managed) > 0 || len(h.config) > 0 }

// errNoHarness is resolveHarness's "nothing by that name".
var errNoHarness = errors.New("no such directory, nor a harness by that name in ~/.boxctl/harness")

// resolveHarness is the directory --harness names, and how to name it:
// the path itself; else name[@ref] -- without a ref, yours under
// ~/.boxctl/harness first -- from the harness repository (harnessremote.go).
func resolveHarness(ctx context.Context, arg string) (dir, label string, err error) {
	if st, err := os.Stat(arg); err == nil {
		if !st.IsDir() {
			return "", "", fmt.Errorf("--harness %s: not a directory", arg)
		}
		return arg, arg, nil
	}
	name, ref, ok := parseHarnessName(arg)
	if !ok {
		return "", "", fmt.Errorf("--harness %s: %w", arg, errNoHarness)
	}
	if ref == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".boxctl", "harness", name)
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return p, p, nil
			}
		}
	}
	return fetchHarness(ctx, name, ref)
}

// loadHarness reads and checks the harness in dir.
func loadHarness(dir string) (*harness, error) {
	h := &harness{dir: dir, label: dir}
	for _, name := range harnessManaged {
		st, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("harness %s: %s is not a regular file", dir, name)
		}
		if name == "managed-settings.json" {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			var v map[string]json.RawMessage
			if err := json.Unmarshal(data, &v); err != nil {
				return nil, fmt.Errorf("harness %s: %s: %w", dir, name, err)
			}
		}
		h.managed = append(h.managed, name)
		h.size += st.Size()
	}
	files, size, err := configFilesIn(dir, harnessConfigEntries)
	if err != nil {
		return nil, err
	}
	h.config, h.size = files, h.size+size
	if h.model, err = loadHarnessModel(dir); err != nil {
		return nil, err
	}
	if len(h.managed) == 0 && len(h.config) == 0 && h.model == nil {
		return nil, fmt.Errorf("harness %s has none of %s, %s, %s", dir,
			strings.Join(harnessManaged, ", "), strings.Join(harnessConfigEntries, ", "), harnessModelFile)
	}
	return h, nil
}

// apply puts the harness on the box. Its ~/.claude entries overwrite
// your configuration's, so call it after copyClaudeConfig, not alongside.
func (h *harness) apply(ctx context.Context, box *boxSSH) error {
	if !h.rules() {
		return nil // only a model.json, which is boxctl's, not the box's
	}
	progress := newUploadProgress("Copying the harness "+bold(h.label), h.size)
	if len(h.managed) > 0 {
		err := streamTar(ctx, box, "mkdir -p "+harnessManagedDir+" && tar -xzf - -C "+harnessManagedDir+" && chmod 0644 "+harnessManagedDir+"/*", progress,
			func(tw *tar.Writer) error { return addFiles(tw, h.dir, h.managed) })
		if err != nil {
			return fmt.Errorf("harness: %w", err)
		}
	}
	if len(h.config) > 0 {
		err := streamTar(ctx, box, "mkdir -p ~/.claude && tar -xzf - -C ~/.claude", progress,
			func(tw *tar.Writer) error { return addFiles(tw, h.dir, h.config) })
		if err != nil {
			return fmt.Errorf("harness: %w", err)
		}
	}
	sayDone("Copied the harness %s %s", bold(h.label), dim("("+client.FormatBytes(h.size)+")"))
	return nil
}

// Which harness a run gets, first match:
//
//	--harness none            none
//	--harness <dir|name[@ref]> that one (resolveHarness)
//	~/.boxctl/harness/default your default, for every run
//	--goal / --issue          pr-only, by name: it runs unattended with
//	                          --dangerously-skip-permissions, so it is the run
//	                          that most needs a guardrail you didn't have to ask for
//
// A harness with nothing but a model.json has no rules, so for --goal and
// --issue it doesn't stand in for pr-only: its model and pr-only's rules
// both apply (withRules). Whatever is picked besides an explicit
// --harness is announced, and so is pr-only under one.

const (
	harnessNone   = "none"
	harnessPROnly = "pr-only" // an unattended run's, by name
)

var (
	claudeHarnessArg string
	claudeHarness    *harness
)

// prepareHarness picks and loads the run's harness into claudeHarness.
func prepareHarness(ctx context.Context) error {
	claudeHarness = nil
	switch arg := claudeHarnessArg; {
	case arg == harnessNone:
		return nil
	case arg != "":
		if err := useHarness(ctx, arg); err != nil {
			return err
		}
		return withRules(ctx, "--harness "+claudeHarness.label)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".boxctl", "harness", "default")
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			var lerr error
			if claudeHarness, lerr = loadHarness(dir); lerr != nil {
				return lerr
			}
			if claudeHarness.rules() || !unattendedRun() {
				sayNote("Using your default harness %s (--harness %s to skip it)", dir, harnessNone)
			}
			return withRules(ctx, "your default harness "+dir)
		}
	}
	if !unattendedRun() {
		return nil
	}
	if err := useHarness(ctx, harnessPROnly); err != nil {
		return fmt.Errorf("an unattended run gets the pr-only harness (--harness %s to skip it): %w", harnessNone, err)
	}
	sayNote("An unattended run gets the %s harness: pull request, never merge (--harness %s to skip it, or --harness <dir|name> for another)", claudeHarness.label, harnessNone)
	return nil
}

// useHarness resolves and loads arg into claudeHarness.
func useHarness(ctx context.Context, arg string) error {
	dir, label, err := resolveHarness(ctx, arg)
	if err != nil {
		return err
	}
	h, err := loadHarness(dir)
	if err != nil {
		return err
	}
	h.label = label
	claudeHarness = h
	return nil
}

// unattendedRun: --goal or --issue, which get pr-only by default.
func unattendedRun() bool { return claudeGoal != "" || claudeIssue != 0 }

// withRules keeps a model-only claudeHarness (named by what) from
// standing in for an unattended run's rules: pr-only takes its place,
// carrying its model. Anything else is left as it is.
func withRules(ctx context.Context, what string) error {
	own := claudeHarness
	if own == nil || own.rules() || !unattendedRun() {
		return nil
	}
	if err := useHarness(ctx, harnessPROnly); err != nil {
		return fmt.Errorf("%s has no rules, so this unattended run needs the pr-only harness too: %w", what, err)
	}
	claudeHarness.model, claudeHarness.modelFrom = own.model, own.label
	sayNote("Using the %s of %s, which has no rules, so this unattended run also gets the %s harness: pull request, never merge (--harness %s to skip both)", harnessModelFile, what, claudeHarness.label, harnessNone)
	return nil
}

// applyHarness is the harness step of starting a box: a no-op without
// --harness.
func applyHarness(ctx context.Context, box *boxSSH) error {
	if claudeHarness == nil {
		return nil
	}
	return claudeHarness.apply(ctx, box)
}
