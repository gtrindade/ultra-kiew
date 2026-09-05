package group

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

// usersArg is the one place every roster-taking action reads its list, so the
// shapes the model gets wrong only have to be handled here -- but they do all
// have to be handled here.
func TestUsersArgRejectsEveryUnusableShape(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing entirely", map[string]any{}},
		{"a bare string instead of a list", map[string]any{"users": "@alice"}},
		{"nil", map[string]any{"users": nil}},
		{"an empty list", map[string]any{"users": []any{}}},
		{"a list of non-strings", map[string]any{"users": []any{1, 2.5, true}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := usersArg(tc.args); err == nil {
				t.Fatalf("expected %v to be rejected", tc.args)
			}
		})
	}
}

func TestUsersArgDeduplicatesAndKeepsOrder(t *testing.T) {
	got, err := usersArg(map[string]any{
		"users": []any{"@alice", "@bob", "@alice", "@carol", "@bob"},
	})
	if err != nil {
		t.Fatalf("usersArg: %v", err)
	}

	want := []string{"@alice", "@bob", "@carol"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Mixed lists come back with the usable names rather than being rejected
// wholesale -- refusing the whole call because one entry was a number would
// leave the user with nothing.
func TestUsersArgKeepsTheUsableNamesFromAMixedList(t *testing.T) {
	got, err := usersArg(map[string]any{"users": []any{"@alice", 42, "@bob"}})
	if err != nil {
		t.Fatalf("usersArg: %v", err)
	}
	if len(got) != 2 || got[0] != "@alice" || got[1] != "@bob" {
		t.Fatalf("got %v", got)
	}
}

func TestAddUsersRefusesWithNoGroup(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "add_users",
		"users":  []any{"@alice"},
	}))
	if err != nil {
		t.Fatalf("add_users: %v", err)
	}
	if !strings.Contains(reply, "No group exists") {
		t.Errorf("expected a create-first reply, got %q", reply)
	}
}

func TestRemoveUsersRefusesWithNoGroup(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "remove_users",
		"users":  []any{"@alice"},
	}))
	if err != nil {
		t.Fatalf("remove_users: %v", err)
	}
	if !strings.Contains(reply, "nobody to remove") {
		t.Errorf("expected a nothing-to-do reply, got %q", reply)
	}
}

func TestRemoveUsersIgnoresPeopleWhoWereNeverInTheGroup(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	syncer := &fakeSyncer{}
	m.SetEventSyncer(syncer)
	createGroup(t, m, "@alice", "@bob")

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "remove_users",
		"users":  []any{"@ninguem"},
	}))
	if err != nil {
		t.Fatalf("remove_users: %v", err)
	}
	if !strings.Contains(reply, "Nothing to do") {
		t.Errorf("expected a no-op reply, got %q", reply)
	}
	if len(syncer.calls) != 0 {
		t.Errorf("nothing changed, so nothing should have been synced: %v", syncer.calls)
	}
}

func TestAddUsersReportsTheOnesAlreadyPresentAlongsideTheNewOnes(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	m.SetEventSyncer(&fakeSyncer{})
	createGroup(t, m, "@alice")

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "add_users",
		"users":  []any{"@alice", "@bob"},
	}))
	if err != nil {
		t.Fatalf("add_users: %v", err)
	}
	if !strings.Contains(reply, "@bob") {
		t.Errorf("expected the new member named, got %q", reply)
	}
	if !strings.Contains(reply, "Already there") {
		t.Errorf("expected the skipped member reported, got %q", reply)
	}
}

// A failing syncer must not roll back or hide the roster change: the group
// really did change, and the model needs to be told the event did not follow
// so it can say so rather than implying everything is in step.
func TestARosterChangeSurvivesAFailingEventSync(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	m.SetEventSyncer(&fakeSyncer{err: errors.New("events.json is unreadable")})
	createGroup(t, m, "@alice")

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "add_users",
		"users":  []any{"@bob"},
	}))
	if err != nil {
		t.Fatalf("add_users should not fail because the event sync did: %v", err)
	}
	if !strings.Contains(reply, "could not be updated") {
		t.Errorf("expected the failure to be surfaced, got %q", reply)
	}

	list, err := m.Manage(groupArgs(map[string]any{"action": "list"}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, "@bob") {
		t.Errorf("the roster change should have persisted, got %q", list)
	}
}

func TestUnknownActionIsRejected(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	_, err := m.Manage(groupArgs(map[string]any{"action": "demolish"}))
	if err == nil {
		t.Fatal("expected an unknown action to be rejected")
	}
	if !strings.Contains(err.Error(), "must be one of") {
		t.Errorf("expected the error to list the valid actions, got %v", err)
	}
}

// The chat is decided by the code from the Telegram update. A tool call that
// somehow arrives without it must fail rather than guess, since guessing means
// acting on the wrong group's roster.
func TestMissingCallerContextIsAnError(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	_, err := m.Manage(map[string]any{"action": "list"})
	if err == nil {
		t.Fatal("expected a missing caller chat ID to be rejected")
	}
	if !strings.Contains(err.Error(), "caller chat context") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMissingActionIsAnError(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	if _, err := m.Manage(groupArgs(nil)); err == nil {
		t.Fatal("expected a missing action to be rejected")
	}
}

// A group file that will not decode must stop the operation. Carrying on with
// an empty map would let the very next MustSave write that emptiness over
// every other chat's group.
func TestACorruptGroupsFileStopsTheOperation(t *testing.T) {
	client := setupTestStorage(t)
	m := NewManager(client)
	createGroup(t, m, "@alice")

	path := filepath.Join(storage.BasePath, storage.DBPath, groupsFileName)
	if err := os.WriteFile(path, []byte(`{"-100": {"users": [`), 0o600); err != nil {
		t.Fatalf("could not corrupt the file: %v", err)
	}

	_, err := m.Manage(groupArgs(map[string]any{
		"action": "add_users",
		"users":  []any{"@bob"},
	}))
	if err == nil {
		t.Fatal("expected the operation to be refused")
	}
	if !strings.Contains(err.Error(), "overwrite") {
		t.Errorf("expected the error to explain the risk, got %v", err)
	}

	// And the corrupt file must still be there to be recovered from, not
	// replaced with an empty one.
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the file should still exist: %v", readErr)
	}
	if !strings.Contains(string(raw), "-100") {
		t.Errorf("the original content was overwritten: %q", raw)
	}
}

func TestListIsAllowedInADM(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	createGroup(t, m, "@alice")

	// Only mutating actions are group-only; reading is not, so someone can ask
	// in a DM who is in the group.
	args := groupArgs(map[string]any{"action": "list"})
	args[googlegenai.ArgIsPrivate] = true

	if _, err := m.Manage(args); err != nil {
		t.Fatalf("list should be allowed in a DM: %v", err)
	}
}
