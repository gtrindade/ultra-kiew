package event

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	EventManageToolName = "event_manage"
	eventsFileName      = "events.json"
)

type Event struct {
	Date          string            `json:"date"`
	Summary       string            `json:"summary"`
	MessageID     int               `json:"messageID"`
	Confirmations map[string]string `json:"confirmations"`
}

type Group struct {
	Users []string `json:"users"`
}

type Manager struct {
	storage *storage.Client
	bot     *bot.Bot
}

func (m *Manager) SetBot(b *bot.Bot) {
	m.bot = b
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

	events := make(map[string]Event)
	m.storage.LoadFromDB(eventsFileName, &events)

	chatIDStr := fmt.Sprintf("%d", chatID)
	
	callerChatID, ok := args["_callerChatID"].(int64)
	if !ok {
		callerChatID = 0
	}

	if (action == "create" || action == "remove") && chatID != callerChatID {
		return "", fmt.Errorf("security policy violation: you can only use '%s' for events directly from within the group chat they belong to. Please refuse the request and instruct the user to go to the group chat to perform this operation.", action)
	}

	switch action {
	case "create":
		if _, exists := events[chatIDStr]; exists {
			return fmt.Sprintf("An event already exists for chat %d on %s (%q). Please remove it before creating a new one.", chatID, events[chatIDStr].Date, events[chatIDStr].Summary), nil
		}

		groups := make(map[string]Group)
		m.storage.LoadFromDB("groups.json", &groups)
		_, hasGroup := groups[chatIDStr]
		if !hasGroup {
			return fmt.Sprintf("No group exists for this chat %d. A group must be created first before scheduling an event.", chatID), nil
		}

		date, ok := args["date"].(string)
		if !ok || date == "" {
			return "", fmt.Errorf("date is required to create an event. If the user didn't specify one, do not try to guess. Just respond to the user naturally and ask them when they'd like to schedule the event.")
		}

		summary, ok := args["_chatTitle"].(string)
		if !ok || summary == "" {
			return "", fmt.Errorf("internal error: _chatTitle context is missing")
		}

		var missingUsers []string
		knownUsers := make(map[string]int64)
		m.storage.LoadFromDB("users.json", &knownUsers)
		
		confirmations := make(map[string]string)
		eventText := fmt.Sprintf("%s - %s\n\n", summary, date)
		for _, u := range groups[chatIDStr].Users {
			confirmations[u] = "❔"
			eventText += fmt.Sprintf("\t%s ❔\n", u)
		}

		var messageID int
		if m.bot != nil {
			msg, err := m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
				ChatID: chatID,
				Text:   eventText,
			})
			if err == nil && msg != nil {
				messageID = msg.ID
			}

			for _, u := range groups[chatIDStr].Users {
				if uid, exists := knownUsers[u]; exists {
					_, err := m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
						ChatID: uid,
						Text:   fmt.Sprintf("You have been invited to %q on %s in the group. Can you show up? (Reply to me here!)", summary, date),
					})
					if err != nil {
						missingUsers = append(missingUsers, u)
					}
				} else {
					missingUsers = append(missingUsers, u)
				}
			}
		}

		event := Event{
			Date:          date,
			Summary:       summary,
			MessageID:     messageID,
			Confirmations: confirmations,
		}
		events[chatIDStr] = event
		m.storage.SaveToDBAsync(eventsFileName, events)

		if len(missingUsers) > 0 && m.bot != nil {
			m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
				ChatID: chatID,
				Text:   fmt.Sprintf("Warning: Could not send a direct message to the following users because they haven't started this bot yet: %v. Please ask them to DM me and start the bot!", missingUsers),
			})
		}
		return fmt.Sprintf("Successfully created event for chat %d on %s with summary %q. EVENT SUMMARY ALREADY SENT TO CHAT. DO NOT EXPLAIN OR ADD EMOJIS. YOUR ONLY JOB NOW IS TO OUTPUT EXACTLY '__SILENT__' SO NO SPAM IS SENT.", chatID, date, summary), nil

	case "remove":
		if _, exists := events[chatIDStr]; !exists {
			return fmt.Sprintf("No event exists for chat %d", chatID), nil
		}
		delete(events, chatIDStr)
		m.storage.SaveToDBAsync(eventsFileName, events)
		return fmt.Sprintf("Successfully removed event for chat %d", chatID), nil

	case "get":
		event, exists := events[chatIDStr]
		if !exists {
			return fmt.Sprintf("No event exists for chat %d", chatID), nil
		}
		return fmt.Sprintf("Current event for chat %d is on %s: %q", chatID, event.Date, event.Summary), nil

	case "update_status":
		event, exists := events[chatIDStr]
		if !exists {
			return fmt.Sprintf("No event exists for chat %d to update status.", chatID), nil
		}

		username, _ := args["username"].(string)
		status, _ := args["status"].(string)
		lateTime, _ := args["late_time"].(string)

		if username == "" || status == "" {
			return "", fmt.Errorf("username and status are required for update_status")
		}

		if event.Confirmations == nil {
			event.Confirmations = make(map[string]string)
		}

		var emoji string
		if status == "yes" {
			emoji = "💪"
		} else if status == "no" {
			emoji = "🐔"
		} else if status == "late" {
			if lateTime != "" {
				emoji = fmt.Sprintf("🐢 (%s)", lateTime)
			} else {
				emoji = "🐢"
			}
		} else {
			return "", fmt.Errorf("invalid status: %s", status)
		}

		groups := make(map[string]Group)
		m.storage.LoadFromDB("groups.json", &groups)
		groupUsers := groups[chatIDStr].Users

		var actualUsername string
		for _, u := range groupUsers {
			if strings.EqualFold(u, username) {
				actualUsername = u
				break
			}
		}
		if actualUsername != "" {
			username = actualUsername
		}

		event.Confirmations[username] = emoji
		events[chatIDStr] = event
		m.storage.SaveToDBAsync(eventsFileName, events)

		eventText := fmt.Sprintf("%s - %s\n\n", event.Summary, event.Date)
		allResponded := true
		for _, u := range groupUsers {
			conf, hasConf := event.Confirmations[u]
			if !hasConf || conf == "❔" {
				conf = "❔"
				allResponded = false
			}
			eventText += fmt.Sprintf("\t%s %s\n", u, conf)
		}

		if m.bot != nil && event.MessageID != 0 {
			m.bot.EditMessageText(context.Background(), &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: event.MessageID,
				Text:      eventText,
			})

			if allResponded {
				allYes := true
				for _, u := range groupUsers {
					if event.Confirmations[u] != "💪" {
						allYes = false
						break
					}
				}

				var finalMessage string
				if allYes {
					finalMessage = "All confirmed! Get ready for an epic and cheesy RPG session full of critical hits and critical failures! 🎲🐉"
				} else {
					finalMessage = "Looks like someone failed their commitment check. Always that one person... 🐔🐢"
				}

				m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
					ChatID: chatID,
					Text:   finalMessage,
				})
			}
		}

		return fmt.Sprintf("Successfully updated status for %s to %s.", username, status), nil

	default:
		return "", fmt.Errorf("invalid action: %s, must be one of [create, remove, get, update_status]", action)
	}
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        EventManageToolName,
				Description: `Manages events. Can create, remove, get the current event, or update_status for a user reacting via DM. Before creating an event, the system will automatically check if a group exists and if an event already exists. Don't ever need to send the chat ID back to the user.

CRITICAL INSTRUCTIONS FOR CREATING EVENTS:
1) If the user doesn't specify a date, YOU MUST NOT call this tool. Instead, reply naturally and ask the user when they want to schedule the event.
2) If the user provides a relative date like "next Friday", interpret it and provide a fully human-readable format in pt-br (including the timezone) in the 'date' argument before calling the tool. For example: 'Sexta-feira, 20/03/2026 às 21:00 BRT'. DO NOT use internal string formats like '2026-03-20 21:00:00'.
3) DO NOT ask the user for an event name, title, or summary. The system will automatically use the chat title as the event summary internally!
4) After calling 'create', do not reply explaining what you did. The group is already notified automatically.

CRITICAL INSTRUCTION FOR UPDATE_STATUS:
1) If you receive a System Note indicating the user is responding to an event via DM, you MUST use 'update_status'. Parse their answer (yes, no, or late) and immediately call the tool. Do NOT just reply 'I'll keep that in mind' - you must always call 'event_manage(action="update_status")'`,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type:        "string",
							Description: "Action to perform: 'create', 'remove', 'get', or 'update_status'",
							Enum:        []string{"create", "remove", "get", "update_status"},
						},
						"chatID": {
							Type:        "integer",
							Description: "Chat ID to associate with the event. Generally found at the end of the context message. HOWEVER, if using 'update_status' from a DM System Note, you MUST use the specific group chat ID provided in that note, completely ignoring the DM chat ID at the end of the prompt.",
						},
						"date": {
							Type:        "string",
							Description: "Date and time of the event. REQUIRED for 'create' action. Omit for other actions.",
						},
						"username": {
							Type:        "string",
							Description: "Username of the person responding (e.g. @alice). Required for update_status action.",
						},
						"status": {
							Type:        "string",
							Description: "Status of the user's confirmation: 'yes', 'no', or 'late'. Required for update_status action.",
							Enum:        []string{"yes", "no", "late"},
						},
						"late_time": {
							Type:        "string",
							Description: "If status is 'late', how late will they be? (e.g., '20 mins', '1 hour'). Optional.",
						},
					},
					Required: []string{"action", "chatID"},
				},
			},
		},
	}
}
