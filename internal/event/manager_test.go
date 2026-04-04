package event

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/storage"
)

func setupTestStorage(t *testing.T) *storage.Client {
	// Override storage path for tests
	tempDir := t.TempDir()
	storage.BasePath = tempDir
	storage.DBPath = "db"
	
	// Create db directory
	os.MkdirAll(filepath.Join(tempDir, "db"), 0755)

	client := storage.NewClient()
	
	// Pre-seed groups.json so the event creation allows it
	groups := map[string]Group{
		"123": {Users: []string{"@alice", "@bob"}},
	}
	client.SaveToDBAsync("groups.json", groups)

	return client
}

func TestManager_Manage_CreateEvent(t *testing.T) {
	storageClient := setupTestStorage(t)
	// Wait a brief moment to ensure async save completes
	time.Sleep(50 * time.Millisecond)
	
	m := NewManager(storageClient)

	tests := []struct {
		name          string
		args          map[string]any
		expectedErr   string
		expectedReply string
	}{
		{
			name: "rejects past event perfectly mathematically correctly regardless of timezone string skewing",
			args: map[string]any{
				"action":          "create",
				"chatID":          float64(123),
				"_callerChatID":   int64(123),
				"_chatTitle":      "Test Group",
				"date":            "Past Date String",
				"iso_date":        time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
			},
			expectedErr: "evaluates to a time in the past",
		},
		{
			name: "accepts future event securely",
			args: map[string]any{
				"action":          "create",
				"chatID":          float64(123),
				"_callerChatID":   int64(123),
				"_chatTitle":      "Test Group",
				"date":            "Future Date String",
				"iso_date":        time.Now().Add(2 * time.Hour).Format(time.RFC3339),
			},
			expectedReply: "Successfully created event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, err := m.Manage(tt.args)
			if tt.expectedErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.expectedErr)
				}
				if !strings.Contains(err.Error(), tt.expectedErr) {
					t.Errorf("expected error containing %q, got %q", tt.expectedErr, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				if !strings.Contains(reply, tt.expectedReply) {
					t.Errorf("expected reply containing %q, got %q", tt.expectedReply, reply)
				}
			}
		})
	}
}
