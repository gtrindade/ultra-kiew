package googlegenai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"google.golang.org/genai"
)

const (
	// ChatData is the name of the tool that updates chat data.
	ChatDataToolName = "chat_data"

	// ChatDataFile is the name of the file where chat data is stored.
	ChatDataFile = "chat-data.json"
)

type InventoryItem struct {
	Value    string `json:"value"`
	Quantity int    `json:"quantity"`
}

var (
	actionGet    = "get"
	actionSet    = "set"
	actionAdd    = "add"
	actionRemove = "remove"
	actionDelete = "delete"
	validActions = []string{actionGet, actionSet, actionAdd, actionRemove, actionDelete}
	// ChatDataTool is the tool for managing chat data.
	ChatDataTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: ChatDataToolName,
				Description: `Universal character data management system that stores and retrieves any property for any character.
						
						Structure:
						- Data points are accessed using character.property notation
						- Both character and property are case-sensitive identifiers
						- No spaces or special characters are allowed in either part
						- The dot (.) is the only separator between character and property
						- All properties should always be lower case
						
						Operations:
						- get: Retrieves the current value of any property for any character
						- set: Updates or creates any property for any character with a new value
						- add: Appends a value to an existing property for a character
						- remove: Removes a value from a property for a character
						- delete: Deletes a property or character completely, removing all associated data

						Properties can represent any character attribute, statistic, or information.
						There are no restrictions on property names - any valid identifier can be used.
						New characters and properties are automatically created when setting values.`,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type: "string",
							Description: `Action to perform:
							- "get": retrieve a value
							- "set": store a value
							- "add": will append a value to a property
							- "remove": will remove a value from a property
							- "delete": will remove the property or character completely
							`,
							Enum: validActions,
						},
						"path": {
							Type:        "string",
							Description: "Access path in format character.property using valid identifiers without spaces or special characters",
						},
						"value": {
							Type:        "string",
							Description: "New value to store when using set action (required for set, ignored for get)",
						},
					},
					Required: []string{"action", "path"},
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
	spew.Dump(args)

	action, ok := args["action"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: action is required")
	}
	if isValidAction(action) == false {
		return "", fmt.Errorf("invalid action: %s, must be one of %v", action, validActions)
	}

	path, ok := args["path"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: path is required")
	}

	value, ok := args["value"].(string)
	if !ok && action != actionGet && action != actionRemove && action != actionDelete {
		return "", fmt.Errorf("invalid argument: value is required when action is 'add'")
	}

	fmt.Printf("Performing action: %q with path: %s and value: %s\n", action, path, value)
	switch action {
	case actionGet:
		return c.chatData[path], nil
	case actionSet:
		c.chatData[path] = value
		c.storage.SaveToDBAsync(ChatDataFile, c.chatData)
		return fmt.Sprintf("Set %s to %s", path, value), nil
	case actionAdd:
		var msg string
		if existingValue, exists := c.chatData[path]; exists {
			var parsedValue []InventoryItem
			if err := json.Unmarshal([]byte(existingValue), &parsedValue); err != nil {
				return "", fmt.Errorf("failed to parse existing value for %s: %w", path, err)
			}
			added := false
			for i, v := range parsedValue {
				if v.Value == value {
					parsedValue[i].Quantity++
					msg = fmt.Sprintf("Incremented quantity of %s to %d in %s", value, parsedValue[i].Quantity, path)
					added = true
				}
			}
			if !added {
				parsedValue = append(parsedValue, InventoryItem{Value: value, Quantity: 1})
				msg = fmt.Sprintf("Added %s to %s with quantity 1", value, path)
			}
			finalValue, err := json.Marshal(parsedValue)
			if err != nil {
				return "", fmt.Errorf("failed to marshal updated value for %s: %w", path, err)
			}
			c.chatData[path] = string(finalValue)
		} else {
			stringValue, err := json.Marshal([]InventoryItem{{Value: value, Quantity: 1}})
			if err != nil {
				return "", fmt.Errorf("failed to marshal new value for %s: %w", path, err)
			}
			c.chatData[path] = string(stringValue)
			msg = fmt.Sprintf("Added %s to %s with quantity 1", value, path)
		}
		c.storage.SaveToDBAsync(ChatDataFile, c.chatData)
		return msg, nil
	case actionRemove:
		if existingValue, exists := c.chatData[path]; exists {
			var parsedValue []InventoryItem
			if err := json.Unmarshal([]byte(existingValue), &parsedValue); err != nil {
				return "", fmt.Errorf("failed to parse existing value for %s: %w", path, err)
			}
			var msg string
			for i, v := range parsedValue {
				if v.Value == value {
					if v.Quantity > 1 {
						v.Quantity--
						parsedValue[i] = v
						c.chatData[path] = string(existingValue)
						msg = fmt.Sprintf("Decremented quantity of %s to %d in %s", value, v.Quantity, path)
					} else {
						parsedValue = append(parsedValue[:i], parsedValue[i+1:]...)
						c.chatData[path] = string(existingValue)
						msg = fmt.Sprintf("Removed %s from %s", value, path)
					}
					c.storage.SaveToDBAsync(ChatDataFile, c.chatData)
					return msg, nil
				}
			}
			return fmt.Sprintf("%s not found in %s", value, path), nil
		}
		return fmt.Sprintf("%s is empty", path), nil
	case actionDelete:
		var deleted bool
		for key := range c.chatData {
			if strings.HasPrefix(key, path+".") || key == path {
				delete(c.chatData, key)
				deleted = true
			}
		}
		c.storage.SaveToDBAsync(ChatDataFile, c.chatData)
		if !deleted {
			return fmt.Sprintf("%s does not exist", path), nil
		}
		return fmt.Sprintf("Deleted character %s and all its properties", path), nil
	default:
		return "", fmt.Errorf("unknown action: %s, must be one of %v", action, validActions)
	}
}
