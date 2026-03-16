package main

import (
	"context"
	"log"

	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/diceroller"
	"github.com/gtrindade/ultra-kiew/internal/event"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/group"
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

	botClient.Start(ctx)
}
