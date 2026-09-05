package main

import (
	"context"
	"log"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/diceroller"
	"github.com/gtrindade/ultra-kiew/internal/event"
	"github.com/gtrindade/ultra-kiew/internal/google"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/group"
	"github.com/gtrindade/ultra-kiew/internal/meet"
	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"github.com/gtrindade/ultra-kiew/internal/telegram"
)

func main() {
	ctx := context.Background()

	config, err := config.LoadFromFile()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	dbClient, err := mysql.NewMySQLClient(config)
	if err != nil {
		log.Fatalf("Failed to create MySQL client: %v", err)
	}
	defer dbClient.Close()

	storageClient := storage.NewClient()
	groupManager := group.NewManager(storageClient)
	eventManager := event.NewManager(storageClient)

	toolConfigs := map[string]*googlegenai.ToolConfig{
		diceroller.RollDice: {
			Function: diceroller.RollWithArgs,
			Tool:     diceroller.GetToolConfig(),
		},
		group.GroupManageToolName: {
			Function: groupManager.Manage,
			Tool:     group.GetToolConfig(),
		},
		event.EventManageToolName: {
			Function: eventManager.Manage,
			Tool:     event.GetToolConfig(),
		},
	}

	aiClient, err := googlegenai.NewClient(ctx, toolConfigs, storageClient, dbClient, config)
	if err != nil {
		log.Fatalf("failed to create Google GenAI client: %v", err)
	}

	botClient, err := telegram.NewBot(config, aiClient, storageClient)
	if err != nil {
		log.Fatalf("failed to create Telegram bot: %v", err)
	}

	eventManager.SetBot(botClient.Bot())
	eventManager.SetAI(aiClient)
	groupManager.SetBot(botClient.Bot())
	// Roster changes go through the event manager rather than writing
	// events.json directly, so they stay behind the same mutex as everything
	// else that touches it.
	groupManager.SetEventSyncer(eventManager)

	setUpMeet(ctx, config, eventManager)

	go eventManager.StartEventMonitor(ctx, 1*time.Minute)

	botClient.Start(ctx)
}

// setUpMeet wires Google Meet integration into the event manager, or logs
// exactly why it could not and leaves the bot running without it.
//
// This is deliberately loud on every failure path. The alternative --
// integration silently absent -- is what actually happened before this
// existed: the wiring that calls eventManager.SetMeet was written and tested
// but never reached main(), so nothing about Meet ever worked in production
// and nothing said so. A missing config block, a bad credentials file, and a
// missing or revoked token now each produce one clear line at startup instead
// of that same silence.
func setUpMeet(ctx context.Context, cfg *config.Config, eventManager *event.Manager) {
	if cfg.Google == nil {
		log.Println("Google Meet integration: disabled (no [google] block in config.yaml)")
		return
	}

	authenticator, err := google.NewAuthenticator(cfg)
	if err != nil {
		log.Printf("Google Meet integration: disabled (%v)", err)
		return
	}

	// The bot's own calls to AccessToken happen from the event monitor's
	// ticker goroutine. Without this, a missing or expired token would send
	// that goroutine into the interactive browser-consent flow -- which can
	// never complete headlessly, but still blocks for up to 5 minutes per
	// attempt, stalling event reminders for every chat once a minute, not just
	// the one that wanted a Meet link.
	authenticator.NonInteractive = true

	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := authenticator.CheckToken(checkCtx); err != nil {
		log.Printf("Google Meet integration: credentials look configured, but no usable token yet -- Meet links will keep failing until this is fixed: %v", err)
		// Wired in anyway: CheckToken can fail on a startup network hiccup as
		// easily as on a real problem, and the event monitor already retries
		// space creation every tick. Refusing to wire it in here would need a
		// full restart to recover from the transient case.
	} else {
		log.Println("Google Meet integration: enabled")
	}

	eventManager.SetMeet(meet.NewClient(authenticator))
}
