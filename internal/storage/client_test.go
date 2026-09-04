package storage

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withTempStorage points the package's globals at a scratch directory for the
// duration of one test, and puts them back afterwards.
func withTempStorage(t *testing.T) *Client {
	t.Helper()
	oldBase, oldDB := BasePath, DBPath
	BasePath, DBPath = t.TempDir(), "db"
	t.Cleanup(func() { BasePath, DBPath = oldBase, oldDB })
	return NewClient()
}

func dbFile(name string) string {
	return filepath.Join(BasePath, DBPath, name)
}

type record struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestSaveToDBRoundTrips(t *testing.T) {
	c := withTempStorage(t)

	want := map[string]record{"a": {Name: "alice", Count: 2}}
	if err := c.SaveToDB("things.json", want); err != nil {
		t.Fatalf("SaveToDB failed: %v", err)
	}

	got := make(map[string]record)
	if err := c.LoadFromDB("things.json", &got); err != nil {
		t.Fatalf("LoadFromDB failed: %v", err)
	}
	if len(got) != 1 || got["a"].Name != "alice" || got["a"].Count != 2 {
		t.Fatalf("round trip lost data: %v", got)
	}
}

// A missing file is the normal first-run case and must not read as an error,
// or every fresh install would refuse to do anything.
func TestLoadOfAMissingFileIsNotAnError(t *testing.T) {
	c := withTempStorage(t)

	got := make(map[string]record)
	if err := c.LoadFromDB("never-written.json", &got); err != nil {
		t.Fatalf("a missing file should load as empty, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty map, got %v", got)
	}
	if err := c.LoadForUpdate("never-written.json", &got); err != nil {
		t.Fatalf("LoadForUpdate should also accept a missing file, got: %v", err)
	}
}

// This is the case LoadForUpdate exists for. A file that is present but will
// not decode used to be indistinguishable from an absent one: the caller got
// an empty map and its next save wrote that emptiness back over real data.
func TestLoadForUpdateRefusesToProceedOnACorruptFile(t *testing.T) {
	c := withTempStorage(t)

	if err := c.SaveToDB("events.json", map[string]record{"a": {Name: "alice"}}); err != nil {
		t.Fatalf("setup save failed: %v", err)
	}
	if err := os.WriteFile(dbFile("events.json"), []byte(`{"a": {"name": "ali`), 0o600); err != nil {
		t.Fatalf("could not truncate the file: %v", err)
	}

	got := make(map[string]record)
	err := c.LoadForUpdate("events.json", &got)
	if err == nil {
		t.Fatal("expected LoadForUpdate to refuse a corrupt file")
	}
	if !strings.Contains(err.Error(), "events.json") {
		t.Errorf("expected the error to name the file, got %v", err)
	}
	if !strings.Contains(err.Error(), "overwrite") {
		t.Errorf("expected the error to explain the risk, got %v", err)
	}
}

// The read-only counterpart deliberately does NOT stop the caller: a users.json
// that will not parse costs a DM, not a schedule.
func TestLoadOrLogCarriesOnPastACorruptFile(t *testing.T) {
	c := withTempStorage(t)

	if err := os.MkdirAll(filepath.Join(BasePath, DBPath), 0o755); err != nil {
		t.Fatalf("could not create the db dir: %v", err)
	}
	if err := os.WriteFile(dbFile("users.json"), []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("could not write the file: %v", err)
	}

	got := make(map[string]int64)
	c.LoadOrLog("users.json", &got) // must not panic, must return
	if len(got) != 0 {
		t.Fatalf("expected nothing to be loaded, got %v", got)
	}
}

func TestSaveCreatesMissingDirectories(t *testing.T) {
	c := withTempStorage(t)

	if err := c.Save(filepath.Join("nested", "deeper", "x.json"), record{Name: "x"}); err != nil {
		t.Fatalf("Save should create parent directories: %v", err)
	}
	if _, err := os.Stat(filepath.Join(BasePath, "nested", "deeper", "x.json")); err != nil {
		t.Fatalf("expected the file to exist: %v", err)
	}
}

func TestSaveOverwritesRatherThanAppending(t *testing.T) {
	c := withTempStorage(t)

	if err := c.SaveToDB("g.json", map[string]record{"a": {}, "b": {}, "c": {}}); err != nil {
		t.Fatalf("first save failed: %v", err)
	}
	if err := c.SaveToDB("g.json", map[string]record{"a": {}}); err != nil {
		t.Fatalf("second save failed: %v", err)
	}

	got := make(map[string]record)
	if err := c.LoadFromDB("g.json", &got); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	// A shorter document written over a longer one must not leave the tail of
	// the old one behind -- that would decode as trailing garbage, or worse,
	// as valid-looking stale data.
	if len(got) != 1 {
		t.Fatalf("expected the file to be replaced, got %v", got)
	}
}

func TestDeleteRemovesTheFile(t *testing.T) {
	c := withTempStorage(t)

	if err := c.SaveToDB("temp.json", record{Name: "x"}); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if err := c.Delete(filepath.Join(DBPath, "temp.json")); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := os.Stat(dbFile("temp.json")); !os.IsNotExist(err) {
		t.Fatalf("expected the file to be gone, got %v", err)
	}
	if err := c.Delete(filepath.Join(DBPath, "temp.json")); err == nil {
		t.Error("deleting a missing file should report an error")
	}
}

// The event manager and the Telegram handler share one Client across
// goroutines, so the embedded RWMutex has to actually cover concurrent access.
// Run with -race for this to be worth anything.
func TestConcurrentSavesAndLoadsDoNotRace(t *testing.T) {
	c := withTempStorage(t)

	if err := c.SaveToDB("shared.json", map[string]record{"seed": {Name: "seed"}}); err != nil {
		t.Fatalf("seed save failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = c.SaveToDB("shared.json", map[string]record{"a": {Count: i}})
		}(i)
		go func() {
			defer wg.Done()
			got := make(map[string]record)
			_ = c.LoadFromDB("shared.json", &got)
		}()
	}
	wg.Wait()
}
