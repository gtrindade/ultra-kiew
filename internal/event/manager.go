package event

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	EventManageToolName = "event_manage"
	eventsFileName      = "events.json"
)

func formatPTBRDate(t time.Time) string {
	daysOfWeek := map[time.Weekday]string{
		time.Sunday:    "Domingo",
		time.Monday:    "Segunda-feira",
		time.Tuesday:   "Terça-feira",
		time.Wednesday: "Quarta-feira",
		time.Thursday:  "Quinta-feira",
		time.Friday:    "Sexta-feira",
		time.Saturday:  "Sábado",
	}
	return fmt.Sprintf("%s, %s às %s", daysOfWeek[t.Weekday()], t.Format("02/01/2006"), t.Format("15:04 MST"))
}

type Event struct {
	Date               string            `json:"date"`
	Timestamp          int64             `json:"timestamp,omitempty"`
	Summary            string            `json:"summary"`
	MessageID          int               `json:"messageID"`
	Confirmations      map[string]string `json:"confirmations"`
	Reminder12HourSent bool              `json:"reminder_12h_sent,omitempty"`
	Reminder1HourSent  bool              `json:"reminder_1h_sent,omitempty"`
	ReminderNowSent    bool              `json:"reminder_now_sent,omitempty"`
}

type Group struct {
	Users []string `json:"users"`
}

type Manager struct {
	storage *storage.Client
	bot     *bot.Bot
	ai      *googlegenai.Client
}

func (m *Manager) SetBot(b *bot.Bot) {
	m.bot = b
}

func (m *Manager) SetAI(ai *googlegenai.Client) {
	m.ai = ai
}

func NewManager(storageClient *storage.Client) *Manager {
	return &Manager{
		storage: storageClient,
	}
}

func (m *Manager) Manage(args map[string]any) (string, error) {
	var chatID int64
	if v, ok := args["chatID"].(float64); ok {
		chatID = int64(v)
	} else if vStr, ok := args["chatID"].(string); ok {
		var err error
		chatID, err = strconv.ParseInt(vStr, 10, 64)
		if err != nil {
			return "", fmt.Errorf("invalid argument: chatID must be a valid number string")
		}
	} else {
		return "", fmt.Errorf("invalid argument: chatID is required and must be a number or string")
	}

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

	if callerChatID > 0 && (action == "create" || action == "remove") {
		return "", fmt.Errorf("security policy violation: events can only be created or removed from within the actual group chat. You are currently in a private DM. Refuse the request and instruct the user to go to the group chat to perform this action.")
	}

	if (action == "create" || action == "remove") && chatID != callerChatID {
		return "", fmt.Errorf("security policy violation: you can only use '%s' for events directly from within the group chat they belong to. Please refuse the request and instruct the user to go to the group chat to perform this operation.", action)
	}

	switch action {
	case "create":
		tzQuote, _ := args["timezone_quote"].(string)
		if strings.TrimSpace(tzQuote) == "" {
			return "", fmt.Errorf("FAIL: User did not explicitly mention a timezone. Do NOT create the event. You MUST reply asking 'Qual o fuso horário (ex: BRT)?' first.")
		}

		if _, exists := events[chatIDStr]; exists {
			return fmt.Sprintf("An event already exists for chat %d on %s (%q). Please remove it before creating a new one.", chatID, events[chatIDStr].Date, events[chatIDStr].Summary), nil
		}

		groups := make(map[string]Group)
		m.storage.LoadFromDB("groups.json", &groups)
		_, hasGroup := groups[chatIDStr]
		if !hasGroup {
			return fmt.Sprintf("No group exists for this chat %d. A group must be created first before scheduling an event.", chatID), nil
		}

		isoDate, ok := args["iso_date"].(string)
		if !ok || isoDate == "" {
			return "", fmt.Errorf("iso_date is required to create an event and must be in ISO 8601 format")
		}

		t, err := time.Parse(time.RFC3339, isoDate)
		if err != nil {
			return "", fmt.Errorf("invalid iso_date format. Must be ISO 8601 with timezone (e.g., '2026-04-03T21:00:00-03:00'). Error: %v", err)
		}
		timestamp := t.Unix()
		
		date := formatPTBRDate(t)

		if timestamp <= time.Now().Unix() {
			return "", fmt.Errorf("FAILED! The requested event time evaluates to a time in the past! THE EVENT WAS NOT CREATED! The user scheduled the event for %s which has already happened. You MUST explain that it has already passed.", isoDate)
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
						Text:   fmt.Sprintf("%q %s. Vai? Se for atrasar, me dê uma estimativa", summary, date),
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
			Timestamp:     timestamp,
			Summary:       summary,
			MessageID:     messageID,
			Confirmations: confirmations,
		}
		events[chatIDStr] = event
		m.storage.SaveToDBAsync(eventsFileName, events)

		if len(missingUsers) > 0 && m.bot != nil {
			params := &bot.SendMessageParams{
				ChatID: chatID,
				Text:   fmt.Sprintf("Aviso: Não foi possível enviar uma mensagem direta para os seguintes usuários porque eles ainda não iniciaram este bot: %v. Por favor, peça a eles para me enviarem uma DM para iniciar o bot!", missingUsers),
			}
			if messageID != 0 {
				params.ReplyParameters = &models.ReplyParameters{MessageID: messageID}
			}
			m.bot.SendMessage(context.Background(), params)
		}
		return fmt.Sprintf("Successfully created event for chat %d on %s with summary %q. EVENT SUMMARY ALREADY SENT TO CHAT. DO NOT EXPLAIN OR ADD EMOJIS. YOUR ONLY JOB NOW IS TO OUTPUT EXACTLY '__SILENT__' SO NO SPAM IS SENT.", chatID, date, summary), nil

	case "remove":
		if _, exists := events[chatIDStr]; !exists {
			return fmt.Sprintf("No event exists for chat %d", chatID), nil
		}

		summary := events[chatIDStr].Summary
		delete(events, chatIDStr)
		m.storage.SaveToDBAsync(eventsFileName, events)
		log.Printf("Alert: Event '%s' has been manually removed for chat %d", summary, chatID)
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

				var finalMessagePrompt string
				if allYes {
					finalMessagePrompt = "Gere uma mensagem animada e nerd, em português do Brasil (pt-br), informando o grupo que TODO MUNDO CONFIRMOU presença na sessão de RPG. Seja criativo, faça referências a acertos críticos ou rolagens de dados! Exemplo de inspiração: 'Todos confirmados! Preparem-se para uma sessão épica de RPG cheia de acertos e falhas críticas! 🎲🐉'"
				} else {
					finalMessagePrompt = "Gere uma mensagem engraçada e bem-humorada, em português do Brasil (pt-br), zoando pois alguém furou a sessão de RPG. Faça referências a testes de resistência falhos ou falta de compromisso. Exemplo de inspiração: 'Parece que alguém falhou no teste de compromisso. Sempre tem um... 🐔🐢'"
				}

				systemPrompt := fmt.Sprintf("\n[System Note: %s\nIMPORTANTE: Apenas retorne a mensagem final gerada, sem formatações adicionais ou aspas que bloqueiem a fala.]", finalMessagePrompt)

				generatedMessage := ""
				if m.ai != nil {
					generatedMessage, _ = m.ai.SendMessage(context.Background(), chatID, event.Summary, systemPrompt)
				}

				if generatedMessage == "" {
					if allYes {
						generatedMessage = "Todos confirmados! Preparem-se para uma sessão de RPG épica e cheia de acertos e falhas críticas! 🎲🐉"
					} else {
						generatedMessage = "Parece que alguém falhou no teste de compromisso. Sempre tem um... 🐔🐢"
					}
				}

				params := &bot.SendMessageParams{
					ChatID: chatID,
					Text:   generatedMessage,
				}
				if event.MessageID != 0 {
					params.ReplyParameters = &models.ReplyParameters{MessageID: event.MessageID}
				}
				m.bot.SendMessage(context.Background(), params)
			}
		}

		return fmt.Sprintf("Successfully updated status for %s to %s.", username, status), nil

	default:
		return "", fmt.Errorf("invalid action: %s, must be one of [create, remove, get, update_status]", action)
	}
}

func (m *Manager) StartEventMonitor(ctx context.Context, checkInterval time.Duration) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			events := make(map[string]Event)
			m.storage.LoadFromDB(eventsFileName, &events)

			archivedEvents := make(map[string][]Event)
			m.storage.LoadFromDB("archived_events.json", &archivedEvents)

			changed := false
			now := time.Now().Unix()

			for chatIDStr, ev := range events {

				// 12 hour reminder
				if ev.Timestamp > 0 && !ev.Reminder12HourSent && ev.Timestamp-now <= 12*60*60 && ev.Timestamp-now > 2*60*60 {
					durationSeconds := ev.Timestamp - now
					var whenStr string
					if durationSeconds >= 11*60*60+55*60 {
						whenStr = "12 horas"
					} else {
						hours := durationSeconds / 3600
						if hours == 1 {
							whenStr = "1 hora"
						} else {
							whenStr = fmt.Sprintf("%d horas", hours)
						}
					}

					sendReminder(m, chatIDStr, ev, whenStr)
					ev.Reminder12HourSent = true
					events[chatIDStr] = ev
					changed = true
				}

				if ev.Timestamp > 0 && !ev.Reminder1HourSent && ev.Timestamp-now <= 60*60 && ev.Timestamp > now {
					durationSeconds := ev.Timestamp - now
					var whenStr string
					if durationSeconds >= 55*60 {
						whenStr = "1 hora"
					} else {
						minutes := durationSeconds / 60
						if minutes <= 1 {
							whenStr = "1 minuto"
						} else {
							whenStr = fmt.Sprintf("%d minutos", minutes)
						}
					}

					sendReminder(m, chatIDStr, ev, whenStr)
					ev.Reminder1HourSent = true
					events[chatIDStr] = ev
					changed = true
				}

				if ev.Timestamp > 0 && ev.Timestamp <= now {
					if !ev.ReminderNowSent {
						sendReminder(m, chatIDStr, ev, "agora")
						ev.ReminderNowSent = true
					}
					archivedEvents[chatIDStr] = append(archivedEvents[chatIDStr], ev)
					delete(events, chatIDStr)
					changed = true
					log.Printf("Alert: Event '%s' has been deleted/archived for chat %s", ev.Summary, chatIDStr)
				}
			}

			if changed {
				m.storage.SaveToDBAsync("archived_events.json", archivedEvents)
				m.storage.SaveToDBAsync(eventsFileName, events)
			}
		}
	}
}

func sendReminder(m *Manager, chatIDStr string, ev Event, when string) {
	var confirmedUsers []string
	for u, conf := range ev.Confirmations {
		if conf == "💪" || strings.HasPrefix(conf, "🐢") {
			confirmedUsers = append(confirmedUsers, u)
		}
	}

	if len(confirmedUsers) == 0 {
		return
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return
	}

	tags := strings.Join(confirmedUsers, " ")

	var timeMsg string
	if when == "agora" {
		timeMsg = "AGORA"
	} else {
		timeMsg = "daqui a " + when
	}

	systemPrompt := fmt.Sprintf("\n[System Note: Gere uma mensagem animada, nerd e curta em português (pt-br) avisando que a sessão '%s' vai começar %s! Diga para eles se prepararem. No final da mensagem, certifique-se de incluir as seguintes marcações (tags dos usuários) exatamente como estão para chamá-los: %s]", ev.Summary, timeMsg, tags)

	generatedMessage := ""
	if m.ai != nil {
		generatedMessage, _ = m.ai.SendMessage(context.Background(), chatID, ev.Summary, systemPrompt)
	}
	if generatedMessage == "" {
		generatedMessage = fmt.Sprintf("Atenção %s! O evento '%s' começa %s!", tags, ev.Summary, timeMsg)
	}

	log.Printf("Alert: Reminder sent for event '%s' (%s) to chat %s", ev.Summary, timeMsg, chatIDStr)

	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   generatedMessage,
	}
	if ev.MessageID != 0 {
		params.ReplyParameters = &models.ReplyParameters{MessageID: ev.MessageID}
	}
	m.bot.SendMessage(context.Background(), params)
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: EventManageToolName,
				Description: `Manages events. Can create, remove, get the current event, or update_status for a user reacting via DM. Before creating an event, the system will automatically check if a group exists and if an event already exists. Don't ever need to send the chat ID back to the user.

CRITICAL INSTRUCTIONS FOR CREATING EVENTS:
1) If the user doesn't specify a date, YOU MUST NOT call this tool. Instead, reply naturally and ask the user when they want to schedule the event.
2) DO NOT ask the user for an event name, title, or summary. The system will automatically use the chat title as the event summary internally!
3) After calling 'create', do not reply explaining what you did. The group is already notified automatically.
4) You MUST provide the 'iso_date' argument as an exact ISO 8601 string completely including the correct timezone offset (e.g. '2026-03-20T21:00:00-03:00'). CRITICAL: If the user does not explicitly specify a timezone (like BRT or EDT), YOU MUST NOT CALL THIS TOOL! You must abort, reply to the user naturally, and ask them "Qual o fuso horário?" before you create the event. Only call this tool when the timezone is definitively known.
5) If the user asks to schedule an event for a time TODAY that has ALREADY PASSED, DO NOT silently schedule it for tomorrow! You must refuse and ask them explicitly if they meant tomorrow.

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
							Type:        "string",
							Description: "Chat ID to associate with the event. Generally found at the end of the context message. HOWEVER, if using 'update_status' from a DM System Note, you MUST use the specific group chat ID provided in that note, completely ignoring the DM chat ID at the end of the prompt.",
						},
						"iso_date": {
							Type:        "string",
							Description: "The exact date and time of the event in ISO 8601 format INCLUDING timezone offset. Do NOT guess the timezone; if none is provided, do NOT call this tool and ask the user for it first. REQUIRED.",
						},
						"timezone_quote": {
							Type:        "string",
							Description: "You MUST strictly quote the EXACT words the user used to specify the timezone (e.g. 'BRT', 'horário de brasília', etc). If the user did not explicitly state a timezone, you MUST leave this empty. Do not guess or infer. Required for create action.",
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
