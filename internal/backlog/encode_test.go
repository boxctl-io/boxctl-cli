package backlog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// backlog.json has each item on a line of its own, and reads back the
// same -- a goal with a newline, or text that looks like the items key,
// included.
func TestSaveOneLinePerItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backlog.json")
	now := time.Now().UTC().Truncate(time.Second)
	want := &Backlog{Next: 3, Limits: DefaultLimits, BackoffWhy: `"items":null`, Items: []*Item{
		{ID: 1, Repo: "o/r", Goal: "two\nlines", State: Queued, Created: now, Updated: now},
		{ID: 2, Repo: "o/r", Goal: `say "items":null`, State: Active, Created: now, Updated: now,
			Runs: []Run{{Kind: Code, Box: "q2-code-1", Started: now}}},
	}}
	if err := save(path, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := string(raw)
	lines := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], `{"id":1,`) || !strings.HasPrefix(lines[2], `{"id":2,`) {
		t.Fatalf("file:\n%s", data)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v, want %+v", got, want)
	}

	// No items: still a backlog Load reads.
	if err := save(path, &Backlog{Next: 1, Limits: DefaultLimits}); err != nil {
		t.Fatal(err)
	}
	if q, err := Load(path); err != nil || len(q.Items) != 0 || q.Next != 1 {
		t.Errorf("empty: %+v, %v", q, err)
	}
}
