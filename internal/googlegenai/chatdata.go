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
	tempChatData = map[string]any{
		"Hel": map[string]any{
			"HP": 25,
			"AC": 17,
		},
		"Thif": map[string]any{
			"HP": 30,
			"AC": 18,
		},
	}
)

var (
	actionGet    = "get"
	actionSet    = "set"
	validActions = []string{actionGet, actionSet}
	// ChatDataTool is the tool for managing chat data.
	ChatDataTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: ChatData,
				Description: `Manages game character data. Use this to check or update character statistics.
            Examples:
            - "What is Hel's current HP?" -> get Hel.HP
            - "Set Thif's AC to 19" -> set Thif.AC with value 19
            - "Update Hel's health to 20" -> set Hel.HP with value 20
            
            Sample available characters and their properties:
            - Hel: HP (health points), AC (armor class)
            - Thif: HP (health points), AC (armor class)
            
            The data is structured as: character_name.property`,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type: "string",
							Description: `The action to perform:
                        - "get" when asking about current values
                        - "set" when updating values`,
							Example: "What is Hel's current HP?",
							Enum:    validActions,
						},
						"path": {
							Type:        "string",
							Description: "The path to the chat data to get or set",
							Example:     "Hel.HP",
						},
						"value": {
							Type:        "string",
							Description: "The new value for given path",
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

	switch action {
	case actionGet:
		fmt.Printf("Performing action: %q", action)
		return fmt.Sprintf("Current chat data: %v", tempChatData), nil
	case actionSet:
		newData, ok := args["newData"].(string)
		if !ok && action == actionSet {
			return "", fmt.Errorf("invalid argument: newData is required when action is 'set'")
		}
		fmt.Printf("Performing action: %q with data: %q\n", action, newData)

		return fmt.Sprintf("Chat data updated successfully: %v", tempChatData), nil
	}

}
