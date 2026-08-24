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
	groupsFileName      = "groups.json"
	usersFileName       = "users.json"
)

// commonTimezones maps the abbreviations Brazilian players actually type onto
// IANA zones.
//
// The model used to be asked for a full ISO 8601 string "including the correct
// timezone offset". It cannot know that offset -- it has no calendar of DST
// transitions for the user's location and no reason to know the user is in
// Brazil -- so it guessed, and an event asked for in BRT was written down as
// EDT because that happened to be the server's zone. Offsets are a computation,
// not an opinion: the model now names a zone and Go computes the offset for
// that specific date, which also gets DST right for free.
var commonTimezones = map[string]string{
	"BRT":      "America/Sao_Paulo",
	"BRST":     "America/Sao_Paulo",
	"BR":       "America/Sao_Paulo",
	"BRASIL":   "America/Sao_Paulo",
	"BRASILIA": "America/Sao_Paulo",
	"GMT-3":    "America/Sao_Paulo",
	"UTC-3":    "America/Sao_Paulo",
	"EST":      "America/New_York",
	"EDT":      "America/New_York",
	"ET":       "America/New_York",
	"PST":      "America/Los_Angeles",
	"PDT":      "America/Los_Angeles",
	"PT":       "America/Los_Angeles",
	"CET":      "Europe/Lisbon",
	"WET":      "Europe/Lisbon",
	"PORTUGAL": "Europe/Lisbon",
	"UTC":      "UTC",
	"GMT":      "UTC",
}

// resolveTimezone turns whatever the user said into a real location.
func resolveTimezone(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return nil, fmt.Errorf("empty timezone")
	}
	if iana, ok := commonTimezones[strings.ToUpper(tz)]; ok {
		tz = iana
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", tz)
	}
	return loc, nil
}

// parseLocalDateTime reads a wall-clock time in a given location.
//
// Several spellings are accepted because the model is asked for one shape and
// will sometimes hand back another (a space instead of the T, seconds it was
// not asked for). Anything carrying an offset or a Z is rejected outright
// rather than quietly honoured -- the whole point is that the offset is ours to
// compute, so a model that supplies one has misunderstood and we want to hear
// about it on the next turn rather than book the wrong hour.
func parseLocalDateTime(value string, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)

	if strings.HasSuffix(value, "Z") || strings.Contains(value[min(len(value), 11):], "+") ||
		strings.Contains(value[min(len(value), 11):], "-") {
		return time.Time{}, fmt.Errorf("local_datetime %q carries a timezone offset. Send the plain wall-clock time only, like '2026-04-10T21:00', and name the zone in the 'timezone' argument", value)
	}

	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid local_datetime %q. It must look like '2026-04-10T21:00' with no timezone offset", value)
}

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
	return fmt.Sprintf("%s, %s às %s", daysOfWeek[t.Weekday()], t.Format("02/01/2006"), t.Format("15:04"))
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
	// AllRespondedSent latches the "everyone answered" announcement. Without it
	// the announcement fires again on every subsequent status change, because
	// "everybody has responded" stays true once it becomes true -- one tester
	// changing their answer posted the celebration message a second time.
	AllRespondedSent bool      `json:"all_responded_sent,omitempty"`
	Meet             *MeetInfo `json:"meet,omitempty"`
}

type Group struct {
	Users    []string `json:"users"`
	Timezone string   `json:"timezone,omitempty"`
}

type Manager struct {
	storage *storage.Client
	bot     *bot.Bot
	ai      *googlegenai.Client
	meet    MeetClient
}

func (m *Manager) SetBot(b *bot.Bot) {
	m.bot = b
}

func (m *Manager) SetAI(ai *googlegenai.Client) {
	m.ai = ai
}

func (m *Manager) SetMeet(meet MeetClient) {
	m.meet = meet
}

func NewManager(storageClient *storage.Client) *Manager {
	return &Manager{
		storage: storageClient,
	}
}

func (m *Manager) Manage(args map[string]any) (string, error) {
	// See group.Manage: the chat is decided by the code, not the model.
	callerChatID, ok := args[googlegenai.ArgCallerChatID].(int64)
	if !ok {
		return "", fmt.Errorf("internal error: caller chat context is missing")
	}
	isPrivate, _ := args[googlegenai.ArgIsPrivate].(bool)

	action, ok := args["action"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: action is required")
	}

	// update_status is the one action that legitimately targets a chat other
	// than the caller's: it arrives in a DM and applies to a group event. Every
	// other action applies to the chat it was sent from, full stop.
	if action == "update_status" {
		return m.updateStatus(args, callerChatID, isPrivate)
	}

	if isPrivate {
		return "", fmt.Errorf("events can only be managed from inside the group chat itself, and this is a private DM. Tell the user to go to the group chat")
	}

	events := make(map[string]Event)
	m.storage.LoadFromDB(eventsFileName, &events)
	chatIDStr := fmt.Sprintf("%d", callerChatID)

	switch action {
	case "create":
		return m.create(args, callerChatID, chatIDStr, events)

	case "remove":
		if _, exists := events[chatIDStr]; !exists {
			return "No event exists for this chat, so there was nothing to remove.", nil
		}

		summary := events[chatIDStr].Summary
		delete(events, chatIDStr)
		m.storage.MustSave(eventsFileName, events)
		log.Printf("Alert: Event '%s' has been manually removed for chat %d", summary, callerChatID)
		return "Successfully removed the event for this chat.", nil

	case "get":
		event, exists := events[chatIDStr]
		if !exists {
			return "No event is scheduled for this chat.", nil
		}
		return fmt.Sprintf("The current event is %q on %s.", event.Summary, event.Date), nil

	default:
		return "", fmt.Errorf("invalid action: %s, must be one of [create, remove, get, update_status]", action)
	}
}

func (m *Manager) create(args map[string]any, chatID int64, chatIDStr string, events map[string]Event) (string, error) {
	if existing, exists := events[chatIDStr]; exists {
		return fmt.Sprintf("An event already exists on %s (%q). NO NEW EVENT WAS CREATED. Tell the user they must remove that one first.", existing.Date, existing.Summary), nil
	}

	groups := make(map[string]Group)
	m.storage.LoadFromDB(groupsFileName, &groups)
	group, hasGroup := groups[chatIDStr]
	if !hasGroup || len(group.Users) == 0 {
		return "No group exists for this chat. A group must be created first (group_manage) before scheduling an event. Ask the user who should be in the group.", nil
	}

	localDateTime, _ := args["local_datetime"].(string)
	if strings.TrimSpace(localDateTime) == "" {
		return "", fmt.Errorf("local_datetime is required, as 'YYYY-MM-DDTHH:MM' in the users own local wall-clock time, with NO timezone offset")
	}

	// The timezone comes from the group if we already learned it, and is only
	// asked for once. The previous design asked on every create and tried to
	// police it by making the model quote the users words back -- which the
	// model simply learned to satisfy. Remembering the answer removes the
	// question instead of trying to enforce it.
	tzInput, _ := args["timezone"].(string)
	tzSource := "informado"
	if strings.TrimSpace(tzInput) == "" {
		if group.Timezone == "" {
			return "", fmt.Errorf("NO EVENT WAS CREATED. This chat has no timezone on record yet. Do not guess it. Reply asking the user: 'Qual o fuso horário? (ex: BRT)' and call this tool again once they answer")
		}
		tzInput = group.Timezone
		tzSource = "lembrado"
	}

	loc, err := resolveTimezone(tzInput)
	if err != nil {
		return "", fmt.Errorf("%v. Ask the user to state the timezone again, e.g. 'BRT'", err)
	}

	// Parsed in the resolved location, so the offset -- and DST for that
	// specific date -- is computed rather than supplied.
	t, err := parseLocalDateTime(localDateTime, loc)
	if err != nil {
		return "", err
	}

	if !t.After(time.Now()) {
		return "", fmt.Errorf("NO EVENT WAS CREATED. %s is in the past. Tell the user that time has already passed and ask what they actually meant", formatPTBRDate(t))
	}

	summary, ok := args[googlegenai.ArgChatTitle].(string)
	if !ok || summary == "" {
		summary = "Sessão de RPG"
	}

	// Remember the timezone for next time.
	if group.Timezone == "" {
		group.Timezone = loc.String()
		groups[chatIDStr] = group
		m.storage.MustSave(groupsFileName, groups)
	}

	date := formatPTBRDate(t)

	var missingUsers []string
	knownUsers := make(map[string]int64)
	m.storage.LoadFromDB(usersFileName, &knownUsers)

	confirmations := make(map[string]string)
	for _, u := range group.Users {
		confirmations[u] = "❔"
	}

	var messageID int
	if m.bot != nil {
		msg, err := m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
			ChatID: chatID,
			Text:   renderEventText(summary, date, group.Users, confirmations),
		})
		if err == nil && msg != nil {
			messageID = msg.ID
		}

		for _, u := range group.Users {
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

	events[chatIDStr] = Event{
		Date:          date,
		Timestamp:     t.Unix(),
		Summary:       summary,
		MessageID:     messageID,
		Confirmations: confirmations,
	}
	m.storage.MustSave(eventsFileName, events)

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

	log.Printf("Event created for chat %d on %s (%s, tz %s %s)", chatID, date, loc, tzInput, tzSource)

	return "Event created. The summary card has ALREADY been posted to the chat by the system. Do not describe it, do not repeat the date, do not add emojis. Output exactly '__SILENT__' and nothing else.", nil
}

// updateStatus records one persons answer to an invite.
//
// Who is answering is taken from the Telegram user, never from the model. The
// old version passed a username the model had parsed out of the text, guarded
// only by a prompt telling it to refuse answering on someone elses behalf --
// so a user asking the bot to mark a friend as confirmed was one persuasive
// sentence away from working.
func (m *Manager) updateStatus(args map[string]any, callerChatID int64, isPrivate bool) (string, error) {
	if !isPrivate {
		return "", fmt.Errorf("status answers are only accepted in a private DM with the bot. In the group, tell the user to answer in their DM with me")
	}

	// In a private chat the chat ID is the user ID, so this identifies the
	// caller with no help from the model.
	knownUsers := make(map[string]int64)
	m.storage.LoadFromDB(usersFileName, &knownUsers)
	var username string
	for name, uid := range knownUsers {
		if uid == callerChatID {
			username = name
			break
		}
	}
	if username == "" {
		return "", fmt.Errorf("I do not recognise this user yet, so I cannot record an answer")
	}

	status, _ := args["status"].(string)
	lateTime, _ := args["late_time"].(string)

	var emoji string
	switch status {
	case "yes":
		emoji = "💪"
	case "no":
		emoji = "🐔"
	case "late":
		if lateTime != "" {
			emoji = fmt.Sprintf("🐢 (%s)", lateTime)
		} else {
			emoji = "🐢"
		}
	default:
		return "", fmt.Errorf("invalid status %q: must be 'yes', 'no' or 'late'", status)
	}

	events := make(map[string]Event)
	m.storage.LoadFromDB(eventsFileName, &events)

	// Which events this user may answer for is computed here, from storage.
	// The model may name one when there is more than one, but it can only pick
	// from this list -- an ID it invents matches nothing and changes nothing.
	var pending []string
	for chatIDStr, ev := range events {
		if _, isInvited := ev.Confirmations[username]; isInvited {
			pending = append(pending, chatIDStr)
		}
	}

	if len(pending) == 0 {
		return "This user has no event invites to answer right now.", nil
	}

	target := pending[0]
	if len(pending) > 1 {
		requested, _ := args["event_group_id"].(string)
		found := false
		for _, p := range pending {
			if p == requested {
				target = p
				found = true
				break
			}
		}
		if !found {
			var summaries []string
			for _, p := range pending {
				summaries = append(summaries, fmt.Sprintf("%q on %s (event_group_id %s)", events[p].Summary, events[p].Date, p))
			}
			return fmt.Sprintf("This user has more than one pending invite and did not clearly pick one: %s. Ask which event they mean, then call this tool again with the matching event_group_id.", strings.Join(summaries, " | ")), nil
		}
	}

	event := events[target]
	if event.Confirmations == nil {
		event.Confirmations = make(map[string]string)
	}
	event.Confirmations[username] = emoji

	groups := make(map[string]Group)
	m.storage.LoadFromDB(groupsFileName, &groups)
	groupUsers := groups[target].Users

	groupChatID, err := strconv.ParseInt(target, 10, 64)
	if err != nil {
		return "", fmt.Errorf("internal error: corrupt event key %q", target)
	}

	allResponded := true
	allYes := true
	for _, u := range groupUsers {
		conf, hasConf := event.Confirmations[u]
		if !hasConf || conf == "❔" {
			allResponded = false
		}
		if conf != "💪" {
			allYes = false
		}
	}

	announce := allResponded && !event.AllRespondedSent
	if announce {
		event.AllRespondedSent = true
	}

	events[target] = event
	m.storage.MustSave(eventsFileName, events)

	if m.bot != nil && event.MessageID != 0 {
		m.bot.EditMessageText(context.Background(), &bot.EditMessageTextParams{
			ChatID:    groupChatID,
			MessageID: event.MessageID,
			Text:      renderEventText(event.Summary, event.Date, groupUsers, event.Confirmations),
		})

		if announce {
			m.sendAllRespondedAnnouncement(groupChatID, event, allYes)
		}
	}

	return fmt.Sprintf("Recorded %s as %q for %q. Confirm this back to the user briefly.", username, status, event.Summary), nil
}

func renderEventText(summary, date string, users []string, confirmations map[string]string) string {
	text := fmt.Sprintf("%s - %s\n\n", summary, date)
	for _, u := range users {
		conf, ok := confirmations[u]
		if !ok || conf == "" {
			conf = "❔"
		}
		text += fmt.Sprintf("\t%s %s\n", u, conf)
	}
	return text
}

func (m *Manager) sendAllRespondedAnnouncement(groupChatID int64, event Event, allYes bool) {
	var prompt, fallback string
	if allYes {
		prompt = fmt.Sprintf("Gere uma mensagem animada e nerd avisando o grupo que TODO MUNDO CONFIRMOU presença na sessão de RPG %q. Seja criativo, faça referências a acertos críticos ou rolagens de dados.", event.Summary)
		fallback = "Todos confirmados! Preparem-se para uma sessão de RPG épica e cheia de acertos e falhas críticas! 🎲🐉"
	} else {
		prompt = fmt.Sprintf("Gere uma mensagem engraçada zoando porque alguém furou a sessão de RPG %q. Faça referências a testes de resistência falhos ou falta de compromisso.", event.Summary)
		fallback = "Parece que alguém falhou no teste de compromisso. Sempre tem um... 🐔🐢"
	}

	text := m.generateOrFallback(prompt, fallback)

	params := &bot.SendMessageParams{ChatID: groupChatID, Text: text}
	if event.MessageID != 0 {
		params.ReplyParameters = &models.ReplyParameters{MessageID: event.MessageID}
	}
	m.bot.SendMessage(context.Background(), params)
}

// generateOrFallback asks the model for flavour text only.
//
// This goes through GenerateText, which is stateless and has no tools. It used
// to go through the users live chat session, where the model answered the
// half-finished conversation it found there instead: a scheduled reminder came
// out as "preciso que você me diga o fuso horário", and nothing stopped it from
// calling event tools as a side effect of a reminder. What the bot says is a
// decision the code has already made; only the wording is the models.
func (m *Manager) generateOrFallback(prompt, fallback string) string {
	if m.ai == nil {
		return fallback
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	text, err := m.ai.GenerateText(ctx, prompt)
	if err != nil {
		log.Printf("flavour text generation failed, using fallback: %v", err)
		return fallback
	}
	if strings.TrimSpace(text) == "" {
		return fallback
	}
	return text
}

func (m *Manager) StartEventMonitor(ctx context.Context, checkInterval time.Duration) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runMonitorTick(ctx)
		}
	}
}

func (m *Manager) runMonitorTick(ctx context.Context) {
	events := make(map[string]Event)
	m.storage.LoadFromDB(eventsFileName, &events)

	archivedEvents := make(map[string][]Event)
	m.storage.LoadFromDB("archived_events.json", &archivedEvents)

	changed := false
	now := time.Now().Unix()

	for chatIDStr, ev := range events {
		if m.meet != nil && ev.Meet == nil {
			if spaceName, joinURI, err := m.meet.CreateSpace(ctx); err != nil {
				log.Printf("could not create Meet space for chat %s: %v", chatIDStr, err)
			} else {
				ev.Meet = &MeetInfo{SpaceName: spaceName, JoinURI: joinURI}
				events[chatIDStr] = ev
				changed = true
				log.Printf("Alert: Meet space created for event '%s' in chat %s: %s", ev.Summary, chatIDStr, joinURI)
			}
		}

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

			m.sendReminder(chatIDStr, ev, whenStr)
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

			m.sendReminder(chatIDStr, ev, whenStr)
			ev.Reminder1HourSent = true
			events[chatIDStr] = ev
			changed = true
		}

		if ev.Timestamp > 0 && ev.Timestamp <= now {
			if !ev.ReminderNowSent {
				m.sendReminder(chatIDStr, ev, "agora")
				ev.ReminderNowSent = true
				events[chatIDStr] = ev
				changed = true
			}

			updated, finalized, sessionChanged := m.advanceMeetSession(ctx, chatIDStr, ev, now)
			if sessionChanged {
				events[chatIDStr] = updated
				ev = updated
				changed = true
			}
			if finalized {
				archivedEvents[chatIDStr] = append(archivedEvents[chatIDStr], updated)
				delete(events, chatIDStr)
				changed = true
				log.Printf("Alert: Event '%s' has been deleted/archived for chat %s", updated.Summary, chatIDStr)
			}
		}
	}

	if changed {
		m.storage.SaveToDBAsync("archived_events.json", archivedEvents)
		m.storage.MustSave(eventsFileName, events)
	}
}

// appendMeetLink adds the join link after generation, never handing it to the
// model to weave in -- same reason the tags are verified rather than trusted:
// a URL is not something to let a paraphrase-prone model retype.
//
// This is its own function, rather than inline in sendReminder, because it
// went missing once already: the append was written, described, and believed
// committed, but silently dropped in a later rewrite of the surrounding code
// and nothing caught it until a real reminder shipped with no link. A named
// function with a direct test is what makes that regression visible again if
// it happens a second time.
func appendMeetLink(text string, meetInfo *MeetInfo) string {
	if meetInfo == nil || meetInfo.JoinURI == "" {
		return text
	}
	return text + "\n\n🔗 " + meetInfo.JoinURI
}

func (m *Manager) sendReminder(chatIDStr string, ev Event, when string) {
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

	timeMsg := "daqui a " + when
	if when == "agora" {
		timeMsg = "AGORA"
	}

	prompt := fmt.Sprintf("Gere uma mensagem animada, nerd e curta avisando que a sessão %q vai começar %s. Diga para eles se prepararem. No final da mensagem inclua exatamente estas marcações de usuários, sem alterar nada: %s", ev.Summary, timeMsg, tags)
	fallback := fmt.Sprintf("Atenção %s! O evento '%s' começa %s!", tags, ev.Summary, timeMsg)

	text := m.generateOrFallback(prompt, fallback)

	// The tags are the point of the reminder, so verify rather than trust: if
	// the model dropped or mangled them, nobody gets a notification.
	for _, u := range confirmedUsers {
		if !strings.Contains(text, u) {
			log.Printf("reminder text dropped the tag for %s, using fallback", u)
			text = fallback
			break
		}
	}

	text = appendMeetLink(text, ev.Meet)

	log.Printf("Alert: Reminder sent for event '%s' (%s) to chat %s", ev.Summary, timeMsg, chatIDStr)

	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}
	if ev.MessageID != 0 {
		params.ReplyParameters = &models.ReplyParameters{MessageID: ev.MessageID}
	}
	if m.bot != nil {
		m.bot.SendMessage(context.Background(), params)
	}
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: EventManageToolName,
				Description: `Manages the single scheduled event for a chat: create, remove, get, or update_status.

The chat this applies to is determined automatically by the system. You do not have a chat ID, you cannot see one, and you must never ask a user for one.

CREATING AN EVENT:
1) If the user has not said when, do NOT call this tool. Ask them when.
2) Do NOT ask for an event name or title. The system uses the chat title.
3) Give 'local_datetime' as the users plain wall-clock time, 'YYYY-MM-DDTHH:MM', with NO timezone offset. Resolve "amanhã", "sábado" etc. against the <current_time> you were given.
4) Only pass 'timezone' if the user actually named one in this conversation (e.g. "BRT", "horário de Brasília"). Never guess it and never infer it from anything. If the chat already has a timezone on record the system uses that and you do not need to pass it; if it does not, the tool will tell you to ask, and you should ask exactly "Qual o fuso horário? (ex: BRT)".
5) The system checks for a past date, an existing event and a missing group, and will tell you. Report what it says honestly; never claim an event was created when the tool said otherwise.

UPDATE_STATUS (only in a private DM, when a user answers their invite):
Parse their answer as yes / no / late and call the tool immediately. Do not reply "vou anotar" without calling it. The system knows who is speaking; you do not pass a username and you cannot answer on anyone elses behalf.`,
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"action": {
							Type:        "string",
							Description: "Action to perform: 'create', 'remove', 'get', or 'update_status'",
							Enum:        []string{"create", "remove", "get", "update_status"},
						},
						"local_datetime": {
							Type:        "string",
							Description: "The event date and time as the user would say it on a wall clock: 'YYYY-MM-DDTHH:MM'. NO timezone offset, no 'Z'. Required for 'create'.",
						},
						"timezone": {
							Type:        "string",
							Description: "The timezone the user explicitly named, e.g. 'BRT' or 'America/Sao_Paulo'. Leave empty if they did not name one. NEVER guess or infer this.",
						},
						"status": {
							Type:        "string",
							Description: "The answer being recorded: 'yes', 'no', or 'late'. Required for update_status.",
							Enum:        []string{"yes", "no", "late"},
						},
						"late_time": {
							Type:        "string",
							Description: "If status is 'late', how late (e.g. '20 min', '1 hora'). Optional.",
						},
						"event_group_id": {
							Type:        "string",
							Description: "Only needed when the tool has told you the user has more than one pending invite: the event_group_id it listed for the one they picked. Never make this value up.",
						},
					},
					Required: []string{"action"},
				},
			},
		},
	}
}
