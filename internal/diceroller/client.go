package diceroller

import (
	"fmt"

	"github.com/justinian/dice"
	"google.golang.org/genai"
)

const (
	// RollDice is the name of the function that rolls a dice.
	RollDice = "roll_dice"
)

func Roll(prompt string) (string, error) {
	result, _, err := dice.Roll(prompt)
	if err != nil {
		return "", fmt.Errorf("failed to roll dice: %w", err)
	}
	return result.String(), nil
}

func RollWithArgs(args map[string]any) (string, error) {
	prompt, ok := args["prompt"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: prompt is required")
	}

	return Roll(prompt)
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        RollDice,
				Description: "Rolls a dice",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"prompt": {
							Type:        "string",
							Description: "The prompt to roll the dice",
							Example:     "1d20+4",
						},
					},
					Required: []string{"prompt"},
				},
			},
		},
	}
}
