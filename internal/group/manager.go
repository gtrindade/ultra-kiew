package group

import (
	"fmt"

	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	GroupManageToolName = "group_manage"
	groupsFileName      = "groups.json"
)

type Group struct {
	Users []string `json:"users"`
}

type Manager struct {
	storage *storage.Client
}

func NewManager(storageClient *storage.Client) *Manager {
	return &Manager{
		storage: storageClient,
	}
}

func (m *Manager) Manage(args map[string]any) (string, error) {
	chatIDFloat, ok := args["chatID"].(float64)
	if !ok {
		return "", fmt.Errorf("invalid argument: chatID is required and must be a number")
	}
	chatID := int64(chatIDFloat)

	action, ok := args["action"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: action is required")
	}

	groups := make(map[string]Group)
	// Try to load existing groups file
	m.storage.LoadFromDB(groupsFileName, &groups)

	chatIDStr := fmt.Sprintf("%d", chatID)

	switch action {
	case "create":
		usersRaw, ok := args["users"].([]any)
		if !ok {
			return "", fmt.Errorf("invalid argument: users is required for create action and must be a list of strings")
		}
		
		var users []string
		for _, u := range usersRaw {
			if str, ok := u.(string); ok {
				users = append(users, str)
			}
		}

		group := groups[chatIDStr]
		group.Users = users
		groups[chatIDStr] = group

		m.storage.SaveToDBAsync(groupsFileName, groups)
		return fmt.Sprintf("Successfully created group for chat %d with users: %v", chatID, users), nil

	case "remove":
		if _, exists := groups[chatIDStr]; !exists {
			return fmt.Sprintf("No group exists for chat %d", chatID), nil
		}
		delete(groups, chatIDStr)
		m.storage.SaveToDBAsync(groupsFileName, groups)
		return fmt.Sprintf("Successfully removed group for chat %d", chatID), nil

	case "list":
		group, exists := groups[chatIDStr]
		if !exists || len(group.Users) == 0 {
			return fmt.Sprintf("No group exists for chat %d", chatID), nil
		}
		return fmt.Sprintf("Group for chat %d has users: %v", chatID, group.Users), nil

	default:
		return "", fmt.Errorf("invalid action: %s, must be one of [create, remove, list]", action)
	}
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        GroupManageToolName,
				Description: "Manages a group of users for the current chat. Can create a group with a list of users, remove the group, or list the users. Don't ever need to send the chat ID back to the user. ALWAYS enforce a strict limit of 1 group per chat. If the user tries to create another group when one already exists, refuse the request and list the existing group instead.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type:        "string",
							Description: "Action to perform: 'create', 'remove', or 'list'",
							Enum:        []string{"create", "remove", "list"},
						},
						"chatID": {
							Type:        "integer",
							Description: "Chat ID to associate with the group. It will always be available in the format at the end of the context message. You can only take the chatID from the end of the message, if there are multiple chatIDs, take the last one.",
						},
						"users": {
							Type:        "array",
							Description: "List of user names (e.g. ['@alice', '@bob']) to include in the group. Required for 'create' action.",
							Items: &genai.Schema{
								Type: "string",
							},
						},
					},
					Required: []string{"action", "chatID"},
				},
			},
		},
	}
}
