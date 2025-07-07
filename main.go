package main

import (
	"context"
	"log"

	"github.com/gtrindade/ultra-kiew/internal/diceroller"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/telegram"
)

func main() {
	ctx := context.Background()

	toolConfigs := map[string]*googlegenai.ToolConfig{
		diceroller.RollDice: {
			Function: diceroller.RollWithArgs,
			Tool:     diceroller.GetToolConfig(),
		},
	}

	aiClient, err := googlegenai.NewClient(ctx, toolConfigs)
	if err != nil {
		log.Fatalf("failed to create Google GenAI client: %v", err)
	}

	botClient, err := telegram.NewBot(aiClient)
	if err != nil {
		log.Fatalf("failed to create Telegram bot: %v", err)
	}

	botClient.Start(ctx)
}
