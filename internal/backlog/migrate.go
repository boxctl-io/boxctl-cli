package backlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// migrate moves an older boxctl's state -- ~/.boxctl/queue.json and its
// run logs in ~/.boxctl/queue/ -- to backlog.json and backlog/ beside
// path, the first time a backlog file is wanted and there's none yet. A
// rename keeps everything: items, runs in progress, limits, the loop's
// claim. It does nothing for a path not named backlog.json (tests), or
// when backlog.json already exists.
//
// It refuses while an older boxctl may still be using queue.json (its tui
// or `boxctl queue run` holding the loop, or the file's lock): that one
// would go on writing queue.json and launching from it, beside this one.
func migrate(path string) error {
	if filepath.Base(path) != "backlog.json" {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	dir := filepath.Dir(path)
	old := filepath.Join(dir, "queue.json")
	data, err := os.ReadFile(old)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var claim struct {
		Loop     string    `json:"loop"`
		LoopSeen time.Time `json:"loop_seen"`
	}
	if json.Unmarshal(data, &claim) == nil && claim.Loop != "" && time.Since(claim.LoopSeen) < LoopStale {
		return fmt.Errorf("%s is in use by an older boxctl (%s, its tui or `boxctl queue run`): stop it, then run this again", old, claim.Loop)
	}
	lock := old + ".lock"
	if st, err := os.Stat(lock); err == nil {
		if time.Since(st.ModTime()) < lockStale {
			return fmt.Errorf("%s is locked by an older boxctl: stop it, then run this again", old)
		}
		_ = os.Remove(lock)
	}
	if err := os.Rename(old, path); err != nil {
		if _, serr := os.Stat(path); serr == nil {
			return nil // another boxctl moved it first
		}
		return err
	}
	logs, newLogs := filepath.Join(dir, "queue"), filepath.Join(dir, "backlog")
	if _, err := os.Stat(newLogs); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(logs, newLogs); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
