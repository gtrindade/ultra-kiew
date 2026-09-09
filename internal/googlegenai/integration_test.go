package googlegenai_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/event"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/group"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

// TestIntegration_EventAI drives real Gemini calls, so it is opt-in rather
// than opt-out.
//
// It used to run on any plain `go test ./...` that happened to find a
// config.yaml with a key in it -- which meant the default local test run spent
// real quota and took five seconds, and its assertions ("the model asks about
// the timezone", "the model says the date is in the past") are about model
// behaviour, so it can fail for reasons that have nothing to do with the
// commit under test. Gate it on an explicit env var so CI and everyday runs
// stay hermetic:
//
//	ULTRA_KIEW_INTEGRATION=1 go test ./internal/googlegenai/ -run Integration -v
func TestIntegration_EventAI(t *testing.T) {
	if os.Getenv("ULTRA_KIEW_INTEGRATION") == "" {
		t.Skip("set ULTRA_KIEW_INTEGRATION=1 to run integration tests against the live Gemini API")
	}

	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("could not read the working directory: %v", err)
	}
	if err := os.Chdir("../../"); err != nil {
		t.Fatalf("could not change to the repo root: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWd); err != nil {
			t.Errorf("could not restore the working directory: %v", err)
		}
	})

	cfg, err := config.LoadFromFile()
	if err != nil || cfg.GeminiAPIKey == "" {
		t.Skip("Skipping integration test: Gemini API key not found in config.yaml")
	}

	// Override storage path for tests
	tempDir := t.TempDir()
	storage.BasePath = tempDir
	storage.DBPath = "db"

	if err := os.MkdirAll(filepath.Join(tempDir, "db"), 0o755); err != nil {
		t.Fatalf("could not create the db dir: %v", err)
	}

	storageClient := storage.NewClient()

	// Pre-seed groups.json so the event creation allows it
	groups := map[string]event.Group{
		"-12345": {Users: []string{"@alice", "@bob", "@guilhermetmg"}},
	}
	storageClient.SaveToDBAsync("groups.json", groups)
	time.Sleep(50 * time.Millisecond)

	eventManager := event.NewManager(storageClient)
	groupManager := group.NewManager(storageClient)

	toolConfigs := map[string]*googlegenai.ToolConfig{
		event.EventManageToolName: {
			Function: eventManager.Manage,
			Tool:     event.GetToolConfig(),
		},
		group.GroupManageToolName: {
			Function: groupManager.Manage,
			Tool:     group.GetToolConfig(),
		},
	}

	ctx := context.Background()
	aiClient, err := googlegenai.NewClient(ctx, toolConfigs, storageClient, nil, cfg)
	if err != nil {
		t.Fatalf("Failed to create AI client: %v", err)
	}

	// 1. Ask AI to schedule an event for 21:00 (without timezone)
	// It should respond asking for the timezone!
	chatID := int64(-12345)

	resp1, err := aiClient.SendMessage(ctx, chatID, "Test Group", "cria um evento as 21:00")
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}
	t.Logf("AI Response 1 (Asking for timezone): %s", resp1)

	if !strings.Contains(strings.ToLower(resp1), "fuso") && !strings.Contains(strings.ToLower(resp1), "horário") && !strings.Contains(strings.ToLower(resp1), "horario") {
		t.Errorf("Expected AI to ask for timezone, but got: %s", resp1)
	}

	// 2. We answer "BRT" and the AI should create it in the PAST and fail!
	// To reliably test past-rejection, we can just ask for "ontem às 21:00" (yesterday at 21:00) or an explicit past absolute date!
	resp2, err := aiClient.SendMessage(ctx, chatID, "Test Group", "quero o evento para 1 de Janeiro de 2020 às 21:00 BRT. Crie agora mesmo.")
	if err != nil {
		t.Fatalf("Failed to send message 2: %v", err)
	}
	t.Logf("AI Response 2 (Rejection): %s", resp2)

	// The AI must apologize that the event is in the past.
	if !strings.Contains(strings.ToLower(resp2), "passado") && !strings.Contains(strings.ToLower(resp2), "já passou") && !strings.Contains(strings.ToLower(resp2), "passou") {
		t.Errorf("Expected AI to apologize about past event, got: %s", resp2)
	}

	// 3. Ask to schedule for future properly!
	resp3, err := aiClient.SendMessage(ctx, chatID, "Test Group", "blz, então quero o evento para 1 de Janeiro de 2030 às 21:00 BRT")
	if err != nil {
		t.Fatalf("Failed to send message 3: %v", err)
	}
	t.Logf("AI Response 3 (Success): %s", resp3)

	// The AI generates __SILENT__ or empty or confirms briefly because we told it to output __SILENT__ and message.go skips it.
	if resp3 != "" {
		t.Errorf("Expected AI to output empty string due to __SILENT__, got: %s", resp3)
	}
}
