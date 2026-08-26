package group

import (
	"context"
	"fmt"

	"github.com/go-telegram/bot"
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
	bot     *bot.Bot
}

func NewManager(storageClient *storage.Client) *Manager {
	return &Manager{
		storage: storageClient,
	}
}

func (m *Manager) SetBot(b *bot.Bot) {
	m.bot = b
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

		// This used to only ever check knownUsers (do we have a chat ID on
		// file at all) and return the result as a string for the model to
		// relay -- and in production, the model sometimes just didn't: a
		// clean "Feito! Registrei o grupo..." with the missing-user warning
		// silently dropped, even though this tool had already detected and
		// returned it. event_manage never had that failure mode, because it
		// posts its own "could not reach these users" warning directly to the
		// chat rather than trusting the model to repeat it -- this does the
		// same. It also tests deliverability for real, by actually attempting
		// a DM, rather than only trusting that a stored chat ID still works.
		chatTitle, _ := args[googlegenai.ArgChatTitle].(string)
		missingUsers := m.notifyNewMembers(chatID, chatTitle, users)

		if len(missingUsers) > 0 {
			return fmt.Sprintf("Successfully created the group with users: %v. The following users have not started the bot yet, and a warning about them has ALREADY been posted to the chat directly -- do not claim you are the one telling the user this, just briefly confirm: %v.", users, missingUsers), nil
		}
		return fmt.Sprintf("Successfully created the group with users: %v. Everyone could be reached by DM.", users), nil

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

// notifyNewMembers actually attempts a "you've been added" DM to every new
// member, and reports back who could not be reached. If anyone couldn't, it
// posts that warning to the group chat itself -- directly, not through the
// model -- so it cannot be silently dropped from a conversational reply.
func (m *Manager) notifyNewMembers(chatID int64, chatTitle string, users []string) []string {
	if m.bot == nil {
		return nil
	}

	knownUsers := make(map[string]int64)
	m.storage.LoadFromDB(usersFileName, &knownUsers)

	groupName := chatTitle
	if groupName == "" {
		groupName = "o grupo"
	}

	var missingUsers []string
	for _, u := range users {
		uid, exists := knownUsers[u]
		if !exists {
			missingUsers = append(missingUsers, u)
			continue
		}
		_, err := m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
			ChatID: uid,
			Text:   fmt.Sprintf("Você foi adicionado ao grupo %q! Quando tiver um evento marcado, eu te chamo por aqui para confirmar presença.", groupName),
		})
		if err != nil {
			missingUsers = append(missingUsers, u)
		}
	}

	if len(missingUsers) > 0 {
		m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
			ChatID: chatID,
			Text:   fmt.Sprintf("Aviso: Não foi possível enviar uma mensagem direta para os seguintes usuários porque eles ainda não iniciaram este bot: %v. Por favor, peça a eles para me enviarem uma DM para iniciar o bot!", missingUsers),
		})
	}

	return missingUsers
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
Never claim a group was created or removed unless this tool returned success.
On 'create', the system itself already DMs every new member and, if any could not be reached, posts that warning directly to the group chat -- you do not need to (and should not) repeat that warning yourself, just confirm briefly.`,
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
