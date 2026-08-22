package group

import (
	"fmt"

	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	GroupManageToolName = "group_manage"
	groupsFileName      = "groups.json"
	usersFileName       = "users.json"
)

type Group struct {
	Users []string `json:"users"`
	// Timezone is the IANA zone the group schedules in, learned once and reused
	// so nobody is asked "qual o fuso horário?" on every single event.
	Timezone string `json:"timezone,omitempty"`
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
	// The chat is decided by the code from the Telegram update, never by the
	// model. It used to be a tool parameter the model had to carry through the
	// conversation, which failed constantly: it asked users to type the ID in
	// by hand, and it "registered" a group against an ID that was not this one.
	chatID, ok := args[googlegenai.ArgCallerChatID].(int64)
	if !ok {
		return "", fmt.Errorf("internal error: caller chat context is missing")
	}
	isPrivate, _ := args[googlegenai.ArgIsPrivate].(bool)

	action, ok := args["action"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: action is required")
	}

	if isPrivate && (action == "create" || action == "remove") {
		return "", fmt.Errorf("this action is only allowed inside the group chat itself, and this is a private DM. Tell the user to go to the group chat to do it")
	}

	groups := make(map[string]Group)
	m.storage.LoadFromDB(groupsFileName, &groups)

	chatIDStr := fmt.Sprintf("%d", chatID)

	switch action {
	case "create":
		usersRaw, ok := args["users"].([]any)
		if !ok {
			return "", fmt.Errorf("invalid argument: users is required for create action and must be a list of strings")
		}

		var users []string
		seen := make(map[string]bool)
		for _, u := range usersRaw {
			if str, ok := u.(string); ok {
				if !seen[str] {
					seen[str] = true
					users = append(users, str)
				}
			}
		}

		if len(users) == 0 {
			return "", fmt.Errorf("no valid usernames were given. Ask the user which people should be in the group, as @usernames")
		}

		// One group per chat, enforced here rather than only in the prompt.
		// The tool description asked the model to refuse a second create; the
		// model is not a reliable place to keep an invariant.
		if existing, exists := groups[chatIDStr]; exists && len(existing.Users) > 0 {
			return fmt.Sprintf("A group already exists for this chat with users: %v. It was NOT changed. Tell the user the group already exists and that they must remove it first if they want a different one.", existing.Users), nil
		}

		group := groups[chatIDStr]
		group.Users = users
		groups[chatIDStr] = group

		m.storage.MustSave(groupsFileName, groups)

		var missingUsers []string
		knownUsers := make(map[string]int64)
		// We only need to check this internally. If it fails, we just assume tracking isn't populated.
		m.storage.LoadFromDB(usersFileName, &knownUsers)
		for _, u := range users {
			if _, exists := knownUsers[u]; !exists {
				missingUsers = append(missingUsers, u)
			}
		}

		if len(missingUsers) > 0 {
			return fmt.Sprintf("Successfully created the group with users: %v.\n\nHowever, the following users have not started the bot yet: %v. Please make sure to ask them to direct message the bot to start it, so it can mention them later. If they already started the bot, tell them to stop and start again.", users, missingUsers), nil
		}
		return fmt.Sprintf("Successfully created the group with users: %v", users), nil

	case "remove":
		if _, exists := groups[chatIDStr]; !exists {
			return "No group exists for this chat, so there was nothing to remove.", nil
		}

		events := make(map[string]any)
		m.storage.LoadFromDB("events.json", &events)
		if _, hasEvent := events[chatIDStr]; hasEvent {
			return "Cannot remove the group because there is an active event associated with it. Tell the user to remove the event first.", nil
		}

		delete(groups, chatIDStr)
		m.storage.MustSave(groupsFileName, groups)
		return "Successfully removed the group for this chat.", nil

	case "list":
		group, exists := groups[chatIDStr]
		if !exists || len(group.Users) == 0 {
			return "No group exists for this chat yet.", nil
		}
		return fmt.Sprintf("The group for this chat has users: %v", group.Users), nil

	default:
		return "", fmt.Errorf("invalid action: %s, must be one of [create, remove, list]", action)
	}
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: GroupManageToolName,
				Description: `Manages the group of users for the current chat. Can create a group with a list of users, remove the group, or list its users.

The chat this applies to is determined automatically by the system. You do not have a chat ID and must never ask the user for one.

There is at most one group per chat; the system enforces this and will tell you if one already exists.
If the user provides duplicate usernames, do not reject the request; they are deduplicated for you.
Never claim a group was created or removed unless this tool returned success.`,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type:        "string",
							Description: "Action to perform: 'create', 'remove', or 'list'",
							Enum:        []string{"create", "remove", "list"},
						},
						"users": {
							Type:        "array",
							Description: "List of user names (e.g. ['@alice', '@bob']) to include in the group. Required for 'create' action. If the user says 'everyone in this chat', use the @usernames you can see in the conversation context.",
							Items: &genai.Schema{
								Type: "string",
							},
						},
					},
					Required: []string{"action"},
				},
			},
		},
	}
}
