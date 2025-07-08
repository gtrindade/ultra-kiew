package googlegenai

import (
	"fmt"

	"google.golang.org/genai"
)

const (
	// ChatData is the name of the tool that updates chat data.
	ChatData = "chat_data"
)

var (
	actionGet    = "get"
	actionSet    = "set"
	validActions = []string{actionGet, actionSet}
	// ChatDataTool is the tool for managing chat data.
	ChatDataTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        ChatData,
				Description: "Manages chat data",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type:        "string",
							Description: "The action to perform on chat data",
							Example:     "What is Hel's current HP?",
						},
						"newData": {
							Type:        "string",
							Description: "New data to update the chat with",
							Example:     "Hel's current HP is 25",
						},
					},
					Required: []string{"action", "newData"},
				},
			},
		},
	}
)

func isValidAction(action string) bool {
	for _, validAction := range validActions {
		if action == validAction {
			return true
		}
	}
	return false
}

func (c *Client) ChatData(args map[string]any) (string, error) {
	action, ok := args["action"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: action is required")
	}
	if isValidAction(action) == false {
		return "", fmt.Errorf("invalid action: %s, must be one of %v", action, validActions)
	}

	newData, ok := args["newData"].(string)
	if !ok && action == actionSet {
		return "", fmt.Errorf("invalid argument: newData is required when action is 'set'")
	}

	fmt.Printf("Performing action: %q with data: %q\n", action, newData)
}
