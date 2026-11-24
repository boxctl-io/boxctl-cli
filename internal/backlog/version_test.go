package backlog

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// versionFields is the JSON field set each Version stands for. Changing a
// field of Backlog, Item or Run without bumping Version fails here: add
// the new set under the new Version.
var versionFields = map[int]string{
	1: "Backlog{backoff backoff_why items limits loop loop_seen next version} " +
		"Item{asked branch change created extra_budget goal harness id model note pr priority ready_sha repo reruns runs state updated updates} " +
		"Limits{global per_repo} " +
		"Run{ask box ended error kind launched outcome started}",
}

func TestVersionPinsFields(t *testing.T) {
	seen := map[string]string{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || t.PkgPath() != reflect.TypeOf(Backlog{}).PkgPath() || seen[t.Name()] != "" {
			return
		}
		seen[t.Name()] = "-"
		var names []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			names = append(names, name)
			walk(f.Type)
		}
		sort.Strings(names)
		seen[t.Name()] = t.Name() + "{" + strings.Join(names, " ") + "}"
	}
	walk(reflect.TypeOf(Backlog{}))
	var types []string
	for _, s := range seen {
		types = append(types, s)
	}
	sort.Strings(types)
	if got := strings.Join(types, " "); got != versionFields[Version] {
		t.Errorf("backlog.json's fields changed: bump Version (now %d) and add\n\t%d: %q,\nto versionFields", Version, Version+1, got)
	}
}

// A file a newer boxctl wrote is refused, by Load and Update alike, and
// left as it was; save stamps the current Version.
func TestNewerVersionRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backlog.json")
	if err := Update(path, func(q *Backlog) error { q.Add("o/r", "x", "", 0, time.Now()); return nil }); err != nil {
		t.Fatal(err)
	}
	q, err := Load(path)
	if err != nil || q.Version != Version {
		t.Fatalf("version %d, %v", q.Version, err)
	}
	newer := `{"version":` + strconv.Itoa(Version+1) + `,"next":2,"items":[{"id":1,"change":"keep me"}]}` + "\n"
	if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, ErrNewer) {
		t.Errorf("Load: %v", err)
	}
	if err := Update(path, func(*Backlog) error { return nil }); !errors.Is(err, ErrNewer) {
		t.Errorf("Update: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != newer {
		t.Errorf("file changed: %s", data)
	}
}
