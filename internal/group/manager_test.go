package group

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

const testGroupChatID = int64(-1001234567890)

func setupTestStorage(t *testing.T) *storage.Client {
	t.Helper()
	tempDir := t.TempDir()
	storage.BasePath = tempDir
	storage.DBPath = "db"
	if err := os.MkdirAll(filepath.Join(tempDir, "db"), 0755); err != nil {
		t.Fatalf("failed to create db dir: %v", err)
	}
	return storage.NewClient()
}

func groupArgs(extra map[string]any) map[string]any {
	args := map[string]any{
		googlegenai.ArgCallerChatID: testGroupChatID,
		googlegenai.ArgChatTitle:    "Shadowrun",
		googlegenai.ArgIsPrivate:    false,
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func TestCreateSucceedsWithNoBotConfigured(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "create",
		"users":  []any{"@alice", "@bob"},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !strings.Contains(reply, "Successfully created") {
		t.Fatalf("expected a success message, got %q", reply)
	}
}

func TestCreateRefusesASecondGroup(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	if _, err := m.Manage(groupArgs(map[string]any{
		"action": "create",
		"users":  []any{"@alice"},
	})); err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "create",
		"users":  []any{"@carol"},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(reply, "NOT changed") {
		t.Fatalf("expected a refusal naming the existing group, got %q", reply)
	}
}

func TestListReflectsCurrentUsers(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	if _, err := m.Manage(groupArgs(map[string]any{
		"action": "create",
		"users":  []any{"@alice", "@bob"},
	})); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	reply, err := m.Manage(groupArgs(map[string]any{"action": "list"}))
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(reply, "@alice") || !strings.Contains(reply, "@bob") {
		t.Fatalf("expected both users listed, got %q", reply)
	}
}

// notifyNewMembers is the fix for a real production bug: the missing-user
// warning used to be returned only as a string for the model to relay, and
// the model sometimes just didn't. With no bot configured (as in every test
// here), it must degrade to doing nothing rather than panic on a nil bot.
func TestNotifyNewMembersIsSafeWithNoBotConfigured(t *testing.T) {
	m := NewManager(setupTestStorage(t))

	got := m.notifyNewMembers(testGroupChatID, "Shadowrun", []string{"@alice", "@bob"})
	if got != nil {
		t.Fatalf("expected nil (no bot to attempt delivery with), got %v", got)
	}
}

// fakeSyncer records what roster the event side was told about, so the tests
// can assert the group and the event are actually kept in step.
type fakeSyncer struct {
	calls [][]string
	note  string
	err   error
}

func (f *fakeSyncer) SyncGroupMembers(chatIDStr string, users []string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), users...))
	return f.note, f.err
}

func createGroup(t *testing.T, m *Manager, users ...string) {
	t.Helper()
	list := make([]any, 0, len(users))
	for _, u := range users {
		list = append(list, u)
	}
	if _, err := m.Manage(groupArgs(map[string]any{"action": "create", "users": list})); err != nil {
		t.Fatalf("create failed: %v", err)
	}
}

func TestAddUsersExtendsTheRosterAndSyncsTheEvent(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	syncer := &fakeSyncer{note: "The upcoming event was updated."}
	m.SetEventSyncer(syncer)
	createGroup(t, m, "@alice")

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "add_users",
		"users":  []any{"@bob"},
	}))
	if err != nil {
		t.Fatalf("add_users failed: %v", err)
	}
	if !strings.Contains(reply, "@bob") {
		t.Fatalf("expected the reply to name the added user, got %q", reply)
	}
	if !strings.Contains(reply, "The upcoming event was updated.") {
		t.Fatalf("expected the event sync note to be passed along, got %q", reply)
	}

	// The syncer must be handed the FULL new roster, not just the delta --
	// it reconciles the event card against who is in the group now.
	if len(syncer.calls) != 1 {
		t.Fatalf("expected exactly one sync call, got %d", len(syncer.calls))
	}
	if got := syncer.calls[0]; len(got) != 2 || got[0] != "@alice" || got[1] != "@bob" {
		t.Fatalf("expected the full roster [@alice @bob] to be synced, got %v", got)
	}
}

func TestAddUsersIgnoresPeopleAlreadyInTheGroup(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	syncer := &fakeSyncer{}
	m.SetEventSyncer(syncer)
	createGroup(t, m, "@alice")

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "add_users",
		"users":  []any{"@alice"},
	}))
	if err != nil {
		t.Fatalf("add_users failed: %v", err)
	}
	if !strings.Contains(reply, "already in the group") {
		t.Fatalf("expected a nothing-to-do reply, got %q", reply)
	}
	// Nothing changed, so nothing should have been synced either.
	if len(syncer.calls) != 0 {
		t.Fatalf("expected no sync call for a no-op, got %v", syncer.calls)
	}
}

func TestRemoveUsersShrinksTheRosterAndSyncsTheEvent(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	syncer := &fakeSyncer{}
	m.SetEventSyncer(syncer)
	createGroup(t, m, "@alice", "@bob")

	if _, err := m.Manage(groupArgs(map[string]any{
		"action": "remove_users",
		"users":  []any{"@bob"},
	})); err != nil {
		t.Fatalf("remove_users failed: %v", err)
	}

	reply, err := m.Manage(groupArgs(map[string]any{"action": "list"}))
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if strings.Contains(reply, "@bob") {
		t.Fatalf("expected @bob to be gone from the group, got %q", reply)
	}
	if len(syncer.calls) != 1 || len(syncer.calls[0]) != 1 || syncer.calls[0][0] != "@alice" {
		t.Fatalf("expected the remaining roster [@alice] to be synced, got %v", syncer.calls)
	}
}

// Draining the roster one member at a time must not become a way around
// 'remove', which deliberately refuses while an event is still active.
func TestRemoveUsersRefusesToEmptyTheGroup(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	syncer := &fakeSyncer{}
	m.SetEventSyncer(syncer)
	createGroup(t, m, "@alice", "@bob")

	reply, err := m.Manage(groupArgs(map[string]any{
		"action": "remove_users",
		"users":  []any{"@alice", "@bob"},
	}))
	if err != nil {
		t.Fatalf("remove_users failed: %v", err)
	}
	if !strings.Contains(reply, "leaving it empty") {
		t.Fatalf("expected a refusal to empty the group, got %q", reply)
	}
	if len(syncer.calls) != 0 {
		t.Fatalf("expected nothing to be synced when nothing changed, got %v", syncer.calls)
	}

	list, err := m.Manage(groupArgs(map[string]any{"action": "list"}))
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(list, "@alice") || !strings.Contains(list, "@bob") {
		t.Fatalf("expected the group to be untouched, got %q", list)
	}
}

func TestRosterChangesAreRefusedInADM(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	createGroup(t, m, "@alice")

	for _, action := range []string{"add_users", "remove_users"} {
		args := groupArgs(map[string]any{"action": action, "users": []any{"@bob"}})
		args[googlegenai.ArgIsPrivate] = true
		if _, err := m.Manage(args); err == nil || !strings.Contains(err.Error(), "private DM") {
			t.Errorf("action %q should be refused in a DM, got err=%v", action, err)
		}
	}
}

// With no syncer wired in, roster changes must still work -- they just do not
// touch any event.
func TestRosterChangesWorkWithNoEventSyncer(t *testing.T) {
	m := NewManager(setupTestStorage(t))
	createGroup(t, m, "@alice")

	if _, err := m.Manage(groupArgs(map[string]any{
		"action": "add_users",
		"users":  []any{"@bob"},
	})); err != nil {
		t.Fatalf("add_users failed with no syncer: %v", err)
	}
}
