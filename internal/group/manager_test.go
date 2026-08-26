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
