package event

import (
	"fmt"

	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	EventManageToolName = "event_manage"
	eventsFileName      = "events.json"
)

type Event struct {
	Date    string `json:"date"`
	Summary string `json:"summary"`
}

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

	events := make(map[string]Event)
	m.storage.LoadFromDB(eventsFileName, &events)

	chatIDStr := fmt.Sprintf("%d", chatID)

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

		event := Event{
			Date:    date,
			Summary: summary,
		}
		events[chatIDStr] = event
		m.storage.SaveToDBAsync(eventsFileName, events)
		return fmt.Sprintf("Successfully created event for chat %d on %s with summary %q", chatID, date, summary), nil

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

	default:
		return "", fmt.Errorf("invalid action: %s, must be one of [create, remove, get]", action)
	}
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        EventManageToolName,
				Description: `Manages a single event for the current chat. Can create, remove, or get the current event. Before creating an event, the system will automatically check if a group exists and if an event already exists. Don't ever need to send the chat ID back to the user.

CRITICAL INSTRUCTIONS FOR CREATING EVENTS:
1) If the user doesn't specify a date, YOU MUST NOT call this tool. Instead, reply naturally and ask the user when they want to schedule the event.
2) If the user provides a relative date like "next Friday", interpret it and provide a fully formatted timestamp (including the timezone, preferably BRT or America/Sao_Paulo) in the 'date' argument before calling the tool. For example: '2026-03-20 21:00:00 BRT'.
3) DO NOT ask the user for an event name, title, or summary. The system will automatically use the chat title as the event summary internally!`,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type:        "string",
							Description: "Action to perform: 'create', 'remove', or 'get'",
							Enum:        []string{"create", "remove", "get"},
						},
						"chatID": {
							Type:        "integer",
							Description: "Chat ID to associate with the event. It will always be available in the format at the end of the context message. You can only take the chatID from the end of the message, if there are multiple chatIDs, take the last one.",
						},
						"date": {
							Type:        "string",
							Description: "Date and time of the event. REQUIRED for 'create' action. Omit for other actions.",
						},
					},
					Required: []string{"action", "chatID"},
				},
			},
		},
	}
}
