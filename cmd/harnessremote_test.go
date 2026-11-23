package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const fakeSHA = "0123456789abcdef0123456789abcdef01234567"

// prOnlyFiles is a pr-only harness as the repository holds it.
var prOnlyFiles = map[string]string{
	"README.md":                     "# harnesses",
	"pr-only/CLAUDE.md":             "never merge",
	"pr-only/managed-settings.json": `{"permissions":{"deny":["Bash(gh pr merge:*)"]}}`,
	"pr-only/commands/open-pr.md":   "open a PR",
	"other/CLAUDE.md":               "x",
}

// fakeGitHub serves the harness repository "o/h" at fakeSHA, files
// laid out as given, and points boxctl at it. It counts tarball
// downloads; down, while true, fails every request.
type fakeGitHub struct {
	downloads atomic.Int32
	down      atomic.Bool
}

func newFakeGitHub(t *testing.T, files map[string]string) *fakeGitHub {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": fakeSHA}})
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "o-h-0123456/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	tarball := buf.Bytes()

	f := &fakeGitHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/repos/o/h/commits/main", "/repos/o/h/commits/" + fakeSHA:
			_, _ = w.Write([]byte(fakeSHA))
		case "/o/h/tar.gz/" + fakeSHA:
			f.downloads.Add(1)
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	api, load := githubAPIBase, githubCodeloadBase
	githubAPIBase, githubCodeloadBase = srv.URL, srv.URL
	t.Cleanup(func() { githubAPIBase, githubCodeloadBase = api, load })
	t.Setenv(envHarnessRepo, "o/h")
	return f
}

func TestParseHarnessName(t *testing.T) {
	for _, tt := range []struct {
		arg, name, ref string
		ok             bool
	}{
		{"pr-only", "pr-only", "", true},
		{"pr-only@v1", "pr-only", "v1", true},
		{"pr-only@feature/x", "pr-only", "feature/x", true},
		{"pr-only@", "", "", false},
		{"./pr-only", "", "", false},
		{"a/b", "", "", false},
		{".hidden", "", "", false},
		{"x@a b", "", "", false},
	} {
		name, ref, ok := parseHarnessName(tt.arg)
		if name != tt.name || ref != tt.ref || ok != tt.ok {
			t.Errorf("%q: %q %q %v", tt.arg, name, ref, ok)
		}
	}
}

func TestFetchHarnessCachesByCommit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	gh := newFakeGitHub(t, prOnlyFiles)
	ctx := context.Background()

	dir, label, err := fetchHarness(ctx, "pr-only", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".boxctl", "harness-cache", "o", "h", fakeSHA, "pr-only"); dir != want {
		t.Errorf("dir = %s, want %s", dir, want)
	}
	if label != "pr-only (o/h@main 0123456)" {
		t.Errorf("label = %q", label)
	}
	h, err := loadHarness(dir)
	if err != nil || len(h.managed) != 2 || len(h.config) != 1 {
		t.Fatalf("loaded %+v, %v", h, err)
	}
	if _, _, err := fetchHarness(ctx, "pr-only", fakeSHA); err != nil {
		t.Fatal(err)
	}
	if n := gh.downloads.Load(); n != 1 {
		t.Errorf("%d downloads of one commit, want 1", n)
	}

	// GitHub down: the commit main was last at, from the cache.
	gh.down.Store(true)
	if d, _, err := fetchHarness(ctx, "pr-only", ""); err != nil || d != dir {
		t.Errorf("offline: %s, %v", d, err)
	}
	if _, _, err := fetchHarness(ctx, "pr-only", "v9"); err == nil {
		t.Error("a ref never fetched can't be used offline")
	}
	gh.down.Store(false)

	_, _, err = fetchHarness(ctx, "nope", "")
	if err == nil || !strings.Contains(err.Error(), "other, pr-only") {
		t.Errorf("an unknown name lists what there is: %v", err)
	}
	if _, _, err := fetchHarness(ctx, "pr-only", "v9"); err == nil {
		t.Error("an unknown ref should fail")
	}
}

func TestHarnessChoices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	newFakeGitHub(t, prOnlyFiles)
	writeHarnessFile(t, filepath.Join(home, ".boxctl", "harness", "mine"), "CLAUDE.md", "x")
	writeHarnessFile(t, filepath.Join(home, ".boxctl", "harness", "pr-only"), "CLAUDE.md", "x")
	got, err := harnessChoices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "mine pr-only other" {
		t.Errorf("choices = %v", got)
	}
}

func TestHarnessRepoSetting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(envHarnessRepo, "")
	if r := harnessRepo(); r != defaultHarnessRepo {
		t.Errorf("default = %s", r)
	}
	writeHarnessFile(t, filepath.Join(home, ".boxctl"), "config.json", `{"harness_repo":"me/rules"}`)
	if r := harnessRepo(); r != "me/rules" {
		t.Errorf("config = %s", r)
	}
	t.Setenv(envHarnessRepo, "env/rules")
	if r := harnessRepo(); r != "env/rules" {
		t.Errorf("env = %s", r)
	}
	_ = os.Remove(filepath.Join(home, ".boxctl", "config.json"))
}
