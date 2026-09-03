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

// EventSyncer brings a chat's upcoming event in line with a roster change.
//
// Declared here, and satisfied by *event.Manager, specifically so this package
// never writes events.json itself: that file is guarded by the event manager's
// mutex, and a second writer reaching into it directly would reintroduce the
// lost-update race that mutex exists to close.
type EventSyncer interface {
	SyncGroupMembers(chatIDStr string, users []string) (string, error)
}

type Manager struct {
	storage *storage.Client
	bot     *bot.Bot
	events  EventSyncer
}

func NewManager(storageClient *storage.Client) *Manager {
	return &Manager{
		storage: storageClient,
	}
}

func (m *Manager) SetBot(b *bot.Bot) {
	m.bot = b
}

// SetEventSyncer wires in whatever keeps events in step with the roster.
// Leaving it unset is supported: roster changes then simply do not touch any
// event, rather than failing.
func (m *Manager) SetEventSyncer(events EventSyncer) {
	m.events = events
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

	switch action {
	case "create", "remove", "add_users", "remove_users":
		if isPrivate {
			return "", fmt.Errorf("this action is only allowed inside the group chat itself, and this is a private DM. Tell the user to go to the group chat to do it")
		}
	}

	groups := make(map[string]Group)
	m.storage.LoadFromDB(groupsFileName, &groups)

	chatIDStr := fmt.Sprintf("%d", chatID)

	switch action {
	case "create":
		users, err := usersArg(args)
		if err != nil {
			return "", err
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

	case "add_users":
		group, exists := groups[chatIDStr]
		if !exists || len(group.Users) == 0 {
			return "No group exists for this chat yet, so there is nobody to add to. Create the group first.", nil
		}

		users, err := usersArg(args)
		if err != nil {
			return "", err
		}

		existing := make(map[string]bool, len(group.Users))
		for _, u := range group.Users {
			existing[u] = true
		}

		var added, alreadyThere []string
		for _, u := range users {
			if existing[u] {
				alreadyThere = append(alreadyThere, u)
				continue
			}
			existing[u] = true
			group.Users = append(group.Users, u)
			added = append(added, u)
		}

		if len(added) == 0 {
			return fmt.Sprintf("Nothing to do: %v are already in the group. The group is unchanged: %v.", alreadyThere, group.Users), nil
		}

		groups[chatIDStr] = group
		m.storage.MustSave(groupsFileName, groups)

		chatTitle, _ := args[googlegenai.ArgChatTitle].(string)
		missingUsers := m.notifyNewMembers(chatID, chatTitle, added)
		eventNote := m.syncEvent(chatIDStr, group.Users)

		reply := fmt.Sprintf("Added %v to the group. It now has: %v.", added, group.Users)
		if len(alreadyThere) > 0 {
			reply += fmt.Sprintf(" Already there, so skipped: %v.", alreadyThere)
		}
		if len(missingUsers) > 0 {
			reply += fmt.Sprintf(" These have not started the bot, and a warning about them has ALREADY been posted to the chat directly -- do not repeat it yourself: %v.", missingUsers)
		}
		return reply + eventNote, nil

	case "remove_users":
		group, exists := groups[chatIDStr]
		if !exists || len(group.Users) == 0 {
			return "No group exists for this chat yet, so there is nobody to remove.", nil
		}

		users, err := usersArg(args)
		if err != nil {
			return "", err
		}

		toRemove := make(map[string]bool, len(users))
		for _, u := range users {
			toRemove[u] = true
		}

		var kept, removed []string
		for _, u := range group.Users {
			if toRemove[u] {
				removed = append(removed, u)
				continue
			}
			kept = append(kept, u)
		}

		if len(removed) == 0 {
			return fmt.Sprintf("Nothing to do: none of %v are in the group. It still has: %v.", users, group.Users), nil
		}

		// Emptying the roster this way would leave a group that no event can
		// ever be scheduled against, which is what 'remove' is for -- and
		// 'remove' deliberately refuses while an event is still active.
		// Draining it member by member must not become a way around that.
		if len(kept) == 0 {
			return fmt.Sprintf("That would remove everyone from the group, leaving it empty. Nothing was changed. Tell the user to use the remove action to delete the group instead: %v.", group.Users), nil
		}

		group.Users = kept
		groups[chatIDStr] = group
		m.storage.MustSave(groupsFileName, groups)

		eventNote := m.syncEvent(chatIDStr, group.Users)

		return fmt.Sprintf("Removed %v from the group. It now has: %v.%s", removed, kept, eventNote), nil

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
		return "", fmt.Errorf("invalid action: %s, must be one of [create, remove, list, add_users, remove_users]", action)
	}
}

// usersArg pulls the deduplicated @usernames out of a tool call. Shared by
// every action that takes a roster, so "the model sent the same name twice"
// is handled identically everywhere rather than only where someone
// remembered to.
func usersArg(args map[string]any) ([]string, error) {
	usersRaw, ok := args["users"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid argument: users is required for this action and must be a list of strings")
	}

	var users []string
	seen := make(map[string]bool)
	for _, u := range usersRaw {
		if s, ok := u.(string); ok && !seen[s] {
			seen[s] = true
			users = append(users, s)
		}
	}

	if len(users) == 0 {
		return nil, fmt.Errorf("no valid usernames were given. Ask the user which people this should apply to, as @usernames")
	}
	return users, nil
}

// syncEvent asks whoever owns events.json to bring the chat's upcoming event
// in line with the new roster, and renders whatever it reports as a sentence
// to append to the reply. Returns "" when there is no syncer wired in or no
// event to update -- in both cases there is simply nothing extra to say.
func (m *Manager) syncEvent(chatIDStr string, users []string) string {
	if m.events == nil {
		return ""
	}
	note, err := m.events.SyncGroupMembers(chatIDStr, users)
	if err != nil {
		return fmt.Sprintf(" The group changed, but the upcoming event could not be updated to match: %v. Say so plainly.", err)
	}
	if note == "" {
		return ""
	}
	return " " + note
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
				Description: `Manages the group of users for the current chat: create it with a list of users, add or remove individual members, remove the whole group, or list its users.

The chat this applies to is determined automatically by the system. You do not have a chat ID and must never ask the user for one.

There is at most one group per chat; the system enforces this and will tell you if one already exists.
If the user provides duplicate usernames, do not reject the request; they are deduplicated for you.
Never claim a group was created or removed unless this tool returned success.
On 'create' and 'add_users', the system itself already DMs every new member and, if any could not be reached, posts that warning directly to the group chat -- you do not need to (and should not) repeat that warning yourself, just confirm briefly.

CONFIRM FIRST, but ONLY for 'remove', 'remove_users' and 'add_users':
'remove' deletes the whole group; the other two change who gets pinged from then on. State what it will do, wait for the user to actually answer, then call the tool. If they have already clearly confirmed in this conversation, just do it -- do not ask twice. Never do this for 'create' or 'list': creating is what they just asked for, and listing changes nothing.

Use 'add_users'/'remove_users' to change who is in an existing group -- do NOT remove and recreate the whole group for that. Pass ONLY the people being added or removed in 'users', never the full roster. If the chat has an upcoming event, its card is updated to match automatically: people added show up on it as still unanswered, people removed disappear from it. Report what the tool says about that; do not claim an event was changed if it says nothing about one.`,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type:        "string",
							Description: "Action to perform: 'create', 'remove', 'list', 'add_users', or 'remove_users'",
							Enum:        []string{"create", "remove", "list", "add_users", "remove_users"},
						},
						"users": {
							Type:        "array",
							Description: "List of user names (e.g. ['@alice', '@bob']). Required for 'create' (the full roster), 'add_users' (only the people being added), and 'remove_users' (only the people being removed). If the user says 'everyone in this chat', use the @usernames you can see in the conversation context.",
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
