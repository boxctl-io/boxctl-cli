package backlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oldQueue writes an older boxctl's queue.json into dir: item #5 with a
// code run going on q5-code-1, held by loop (seen at seen), plus its log.
func oldQueue(t *testing.T, dir, loop string, seen time.Time) {
	t.Helper()
	q := &Backlog{Limits: DefaultLimits, Loop: loop, LoopSeen: seen}
	for i := 0; i < 5; i++ {
		q.Add("o/r", "goal", "", 0, t0)
	}
	it := q.Item(5)
	it.State = Active
	it.Runs = []Run{{Kind: Code, Box: "q5-code-1", Started: t0, Launched: true}}
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "queue", "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue", "logs", "q5-code-1.log"), []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateCarriesARunningRunOver(t *testing.T) {
	dir := t.TempDir()
	oldQueue(t, dir, "", time.Time{})
	path := filepath.Join(dir, "backlog.json")
	q, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// queue.json is left as a note an older boxctl can't read as its queue,
	// and that isn't moved again.
	data, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatalf("no note left: %v", err)
	}
	if err := json.Unmarshal(data, &Backlog{}); err == nil {
		t.Errorf("queue.json still reads as a queue: %s", data)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if q, err := Load(path); err != nil || len(q.Items) != 0 {
		t.Errorf("the note was moved again: %v, %v", q, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backlog", "logs", "q5-code-1.log")); err != nil {
		t.Errorf("log not moved: %v", err)
	}
	it := q.Item(5)
	if r := it.Current(); r == nil || r.Box != "q5-code-1" {
		t.Fatalf("current run = %+v", it.Current())
	}
	// The next pass follows it: still running, not lost.
	Decide(q, Obs{Now: t0.Add(time.Hour), Boxes: map[string]Box{"q5-code-1": {State: "running", Status: "running"}}})
	if r := it.Current(); r == nil || r.Outcome != "" {
		t.Errorf("after a pass: %+v", it.Last())
	}
}

func TestMigrateLeavesAnExistingBacklog(t *testing.T) {
	dir := t.TempDir()
	oldQueue(t, dir, "", time.Time{})
	path := filepath.Join(dir, "backlog.json")
	if err := os.WriteFile(path, []byte(`{"next":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	q, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Items) != 0 {
		t.Errorf("backlog.json didn't win: %d items", len(q.Items))
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.json")); err != nil {
		t.Errorf("queue.json was touched: %v", err)
	}
}

func TestMigrateWithNothingToMove(t *testing.T) {
	q, err := Load(filepath.Join(t.TempDir(), "backlog.json"))
	if err != nil || len(q.Items) != 0 {
		t.Fatalf("Load = %v, %v", q, err)
	}
}

func TestMigrateRefusesWhileAnOlderBoxctlRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backlog.json")
	oldQueue(t, dir, "laptop:123", time.Now().Add(-30*time.Second))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "laptop:123") {
		t.Fatalf("held loop: err = %v", err)
	}
	if err := Update(path, func(*Backlog) error { return nil }); err == nil {
		t.Fatal("Update migrated past a held loop")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("backlog.json written")
	}

	// A claim gone stale (the old tui quit without releasing it) moves.
	dir = t.TempDir()
	path = filepath.Join(dir, "backlog.json")
	oldQueue(t, dir, "laptop:123", time.Now().Add(-5*time.Minute))
	if _, err := Load(path); err != nil {
		t.Fatalf("stale loop: %v", err)
	}

	// A fresh lock on queue.json is left alone; a stale one is removed.
	dir = t.TempDir()
	path = filepath.Join(dir, "backlog.json")
	oldQueue(t, dir, "", time.Time{})
	lock := filepath.Join(dir, "queue.json.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("fresh lock: migrated")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("fresh lock removed: %v", err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("stale lock left behind")
	}
}
