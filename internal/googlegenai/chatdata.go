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
	ChatDataFile = "chat-data-%d.json"
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
	actionShow   = "show"
	validActions = []string{actionGet, actionSet, actionAdd, actionRemove, actionDelete, actionShow}
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
						- show: Prints the current state of all characters and their properties

						Properties can represent any character attribute, statistic, or information.
						There are no restrictions on property names - any valid identifier can be used.
						New characters and properties are automatically created when setting values.

						Don't ever need to send the chat ID back to the user.
						`,
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
							- "show": will print the current state of all characters and their properties
							`,
							Enum: validActions,
						},
						"chatID": {
							Type:        "integer",
							Description: "Chat ID to associate with the data. It will always be available in the format at the end of the message.",
						},
						"path": {
							Type:        "string",
							Description: "Access path in format character.property using valid identifiers without spaces or special characters. Not required for show action.",
						},
						"value": {
							Type:        "string",
							Description: "New value to store when using set action (required for set, ignored for get). For add and remove actions, this is the value to append or remove from the property.",
						},
						"quantity": {
							Type:        "integer",
							Description: "Quantity of the item to add or remove when using add or remove actions (optional, defaults to 1)",
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

func needsValue(action string) bool {
	switch action {
	case actionSet, actionAdd, actionRemove:
		return true
	default:
		return false
	}
}

func needsPath(action string) bool {
	switch action {
	case actionShow:
		return false
	default:
		return true
	}
}

func formatChatData(data map[string]string) string {
	var sb strings.Builder
	sb.WriteString("Current chat data:\n")
	for key, value := range data {
		if strings.HasPrefix(value, "[") {
			var items []InventoryItem
			if err := json.Unmarshal([]byte(value), &items); err == nil {
				var itemStrings []string
				for _, item := range items {
					itemStrings = append(itemStrings, fmt.Sprintf("%s (x%d)", item.Value, item.Quantity))
				}
				value = strings.Join(itemStrings, ", ")
			}
		}
		sb.WriteString(fmt.Sprintf("- %s: %s\n", key, value))
	}
	return sb.String()
}

func getNumber[T ~float64 | ~int | ~int64](value any) (T, error) {
	var num T
	if x, ok := value.(float64); ok {
		num = T(x)
	} else if x, ok := value.(int64); ok {
		num = T(x)
	} else if x, ok := value.(int); ok {
		num = T(x)
	} else {
		return num, fmt.Errorf("failed to parse number")
	}
	return num, nil
}

func (c *Client) loadChatData(chatID int64) {
	chatData := make(map[string]string)
	c.storage.LoadFromDB(fmt.Sprintf(ChatDataFile, chatID), &chatData)
	c.lock.Lock()
	c.chatData[chatID] = chatData
	c.lock.Unlock()
}

func (c *Client) saveChatData(chatID int64, data map[string]string) {
	c.storage.SaveToDBAsync(fmt.Sprintf(ChatDataFile, chatID), data)
}

func (c *Client) ChatData(args map[string]any) (string, error) {
	spew.Dump(args)

	chatID, err := getNumber[int64](args["chatID"])
	if err != nil {
		return "", fmt.Errorf("invalid argument: chatID is missing or not a number")
	}

	action, ok := args["action"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: action is missing or not a string")
	}
	if isValidAction(action) == false {
		return "", fmt.Errorf("invalid action: %s, must be one of %v", action, validActions)
	}

	path, ok := args["path"].(string)
	if !ok && needsPath(action) {
		return "", fmt.Errorf("invalid argument: path is missing or not a string")
	}

	value, ok := args["value"].(string)
	if !ok && needsValue(action) {
		return "", fmt.Errorf("invalid argument: value is missing or not a string, but required when action is %q", action)
	}

	quantity, err := getNumber[int](args["quantity"])
	if err != nil {
		quantity = 1
	}

	if c.chatData[chatID] == nil {
		c.loadChatData(chatID)
	}
	chatData := c.chatData[chatID]

	fmt.Printf("Performing action: %q with path: %s and value: %s\n", action, path, value)
	switch action {
	case actionGet:
		return chatData[path], nil
	case actionSet:
		chatData[path] = value
		c.saveChatData(chatID, chatData)
		return fmt.Sprintf("Set %s to %s", path, value), nil
	case actionAdd:
		// TODO: debug why the quantity is not adding up correctly. I already updated some hardcoded 1's to quantity
		var msg string
		if existingValue, exists := chatData[path]; exists {
			var parsedValue []InventoryItem
			if err := json.Unmarshal([]byte(existingValue), &parsedValue); err != nil {
				return "", fmt.Errorf("failed to parse existing value for %s: %w", path, err)
			}
			added := false
			for i, v := range parsedValue {
				if v.Value == value {
					parsedValue[i].Quantity += quantity
					msg = fmt.Sprintf("Incremented quantity of %s to %d in %s", value, parsedValue[i].Quantity, path)
					added = true
				}
			}
			if !added {
				parsedValue = append(parsedValue, InventoryItem{Value: value, Quantity: quantity})
			}
			finalValue, err := json.Marshal(parsedValue)
			if err != nil {
				return "", fmt.Errorf("failed to marshal updated value for %s: %w", path, err)
			}
			chatData[path] = string(finalValue)
		} else {
			stringValue, err := json.Marshal([]InventoryItem{{Value: value, Quantity: quantity}})
			if err != nil {
				return "", fmt.Errorf("failed to marshal new value for %s: %w", path, err)
			}
			chatData[path] = string(stringValue)
		}
		msg = fmt.Sprintf("Added %s to %s with quantity %d", value, path, quantity)
		c.saveChatData(chatID, chatData)
		return msg, nil
	case actionRemove:
		if existingValue, exists := chatData[path]; exists {
			var parsedValue []InventoryItem
			if err := json.Unmarshal([]byte(existingValue), &parsedValue); err != nil {
				return "", fmt.Errorf("failed to parse existing value for %s: %w", path, err)
			}
			var msg string
			for i, v := range parsedValue {
				if v.Value == value {
					if v.Quantity > quantity {
						v.Quantity -= quantity
						parsedValue[i] = v
						msg = fmt.Sprintf("Decremented %d of %s. New total is %d", quantity, path, v.Quantity)
					} else {
						parsedValue = append(parsedValue[:i], parsedValue[i+1:]...)
						msg = fmt.Sprintf("Removed %s from %s", value, path)
					}

					finalValue, err := json.Marshal(parsedValue)
					if err != nil {
						return "", fmt.Errorf("failed to marshal updated value for %s: %w", path, err)
					}
					chatData[path] = string(finalValue)
					c.saveChatData(chatID, chatData)
					return msg, nil
				}
			}
			return fmt.Sprintf("%s not found in %s", value, path), nil
		}
		return fmt.Sprintf("%s is empty", path), nil
	case actionDelete:
		var deleted bool
		for key := range chatData {
			if strings.HasPrefix(key, path+".") || key == path {
				delete(chatData, key)
				deleted = true
			}
		}
		if !deleted {
			return fmt.Sprintf("%s does not exist", path), nil
		}
		c.saveChatData(chatID, chatData)
		return fmt.Sprintf("Deleted character %s and all its properties", path), nil
	case actionShow:
		if len(chatData) == 0 {
			return "No chat data available", nil
		}
		return formatChatData(chatData), nil
	default:
		return "", fmt.Errorf("unknown action: %s, must be one of %v", action, validActions)
	}
}
