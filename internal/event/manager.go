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

	// liveSessionsFileName holds events whose start time has passed but whose
	// Meet session is still being watched (waiting for it to end, waiting for
	// a transcript/notes link, or waiting to post the recap). Keeping these
	// separate from eventsFileName is what lets a new event be scheduled for
	// a chat the moment the current one starts, instead of the slot staying
	// occupied for as long as the Meet lifecycle takes to finish -- which
	// used to be indistinguishable from "an event is still pending" as far as
	// create() could tell, and blocked scheduling a follow-up session until
	// someone manually removed the one that had already happened.
	liveSessionsFileName = "live_sessions.json"
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

	// LastNoResponseNudgeDate is the date (YYYY-MM-DD, in the group's
	// timezone) non-responders were last DMed a nudge. Comparing dates rather
	// than latching a bool is what makes this a once-per-day nudge instead of
	// a once-ever one: whoever still has not answered gets DMed again the
	// next day, and the day after that, until they do.
	LastNoResponseNudgeDate string `json:"last_no_response_nudge_date,omitempty"`

	// Reminder24hCalloutSent latches the "who hasn't responded yet" public
	// callout at the 24-hour mark, so it only ever fires once -- whether or
	// not anyone was actually missing at that exact moment.
	Reminder24hCalloutSent bool `json:"reminder_24h_callout_sent,omitempty"`
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
		// The invite DM was just sent above, so the daily no-response nudge
		// has nothing to add today -- seeding today's date here is what makes
		// it wait until tomorrow rather than repeating the same ask hours
		// later. Uses the same resolved location as the event's own time, so
		// "today" means today in the group's timezone, not the server's.
		LastNoResponseNudgeDate: time.Now().In(loc).Format("2006-01-02"),
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
	for _, u := range groupUsers {
		conf, hasConf := event.Confirmations[u]
		if !hasConf || conf == "❔" {
			allResponded = false
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
			m.sendAllRespondedAnnouncement(groupChatID, event, groupUsers)
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

// lateSuffix pulls the "(10 min)" part off a late confirmation, if the user
// gave one, e.g. "🐢 (10 min)" -> "(10 min)". Returns "" for a bare "🐢".
func lateSuffix(conf string) string {
	start := strings.Index(conf, "(")
	if start == -1 {
		return ""
	}
	return conf[start:]
}

// joinOrNenhum renders a name list for the prompt, since an empty Go slice
// printed with %v reads as "[]" -- not obviously "nobody" to a model that has
// no other signal to go on.
func joinOrNenhum(names []string) string {
	if len(names) == 0 {
		return "ninguém"
	}
	return strings.Join(names, ", ")
}

// announcement is what buildAnnouncement decides, and everything
// sendAllRespondedAnnouncement needs to act on that decision.
type announcement struct {
	prompt        string
	fallback      string
	namesToVerify []string // must all appear in the generated text, or fall back
}

// buildAnnouncement classifies every invited user into confirmed / late /
// absent from their emoji confirmation, and picks one of three tones -- not
// two. Everyone-in and a full no-show were always distinct, but late arrivals
// used to collapse into the same "someone bailed, sessão comprometida" bucket
// as an outright no-show, because the only signal available was "is everyone
// exactly 💪". A player running 10 minutes late would get the same doom-laden
// roast as one who said they are not coming at all.
//
// The model is always handed the literal roster, rather than a bare
// yes/no/late signal, because that vagueness -- not tone -- was the actual
// bug: with nothing specific to reference, the model filled in generic,
// disproportionate drama on its own.
//
// This is pure and separate from sendAllRespondedAnnouncement precisely so the
// classification and tone-selection can be tested directly, without a bot
// double -- the previous behavior here (the same over-the-top message for a
// 10-minute delay as for a no-show) is exactly the kind of thing that should
// fail a test if it comes back.
func buildAnnouncement(summary string, groupUsers []string, confirmations map[string]string) announcement {
	var confirmed, late, absent []string
	for _, u := range groupUsers {
		conf := confirmations[u]
		switch {
		case conf == "💪":
			confirmed = append(confirmed, u)
		case strings.HasPrefix(conf, "🐢"):
			if suffix := lateSuffix(conf); suffix != "" {
				late = append(late, fmt.Sprintf("%s %s", u, suffix))
			} else {
				late = append(late, u)
			}
		case conf == "🐔":
			absent = append(absent, u)
		}
	}

	roster := fmt.Sprintf("Confirmados no horário: %s\nAtrasados: %s\nNão vão: %s",
		joinOrNenhum(confirmed), joinOrNenhum(late), joinOrNenhum(absent))

	var a announcement
	switch {
	case len(absent) == 0 && len(late) == 0:
		a.prompt = fmt.Sprintf("Gere uma mensagem animada e nerd avisando o grupo que TODO MUNDO CONFIRMOU presença na sessão de RPG %q, no horário. Seja criativo, faça referências a acertos críticos ou rolagens de dados.\n\n%s", summary, roster)
		a.fallback = fmt.Sprintf("Todos confirmados para %q! Preparem-se para uma sessão de RPG épica e cheia de acertos e falhas críticas! 🎲🐉", summary)

	case len(absent) == 0:
		// Nobody bailed -- only late arrivals. This is the case that used to
		// get the same treatment as a no-show; it should read as a light,
		// specific jab, not a crisis.
		a.prompt = fmt.Sprintf(`Gere uma mensagem curta e bem-humorada sobre a sessão de RPG %q. NINGUÉM faltou -- todo mundo confirmou, só que algumas pessoas vão chegar atrasadas. Seja leve e proporcional: poucos minutos de atraso NÃO é motivo de drama nem ameaça à sessão, é só uma cutucada gostosa citando o nome de quem está atrasado e, se souber, quanto tempo. Não trate isso como se a sessão estivesse comprometida.

Baseie-se EXATAMENTE nesta lista, sem inventar nomes nem tempos:
%s`, summary, roster)
		a.fallback = fmt.Sprintf("Quase todo mundo pronto para %q!\n%s", summary, roster)
		for _, l := range late {
			a.namesToVerify = append(a.namesToVerify, strings.Fields(l)[0])
		}

	default:
		a.prompt = fmt.Sprintf(`Gere uma mensagem engraçada zoando especificamente quem confirmou que NÃO vai à sessão de RPG %q -- cite os nomes de quem faltou. Se alguém só está atrasado (não ausente), não zoe essa pessoa com o mesmo peso; reserve o deboche mais pesado para quem realmente não vai.

Baseie-se EXATAMENTE nesta lista, sem inventar nomes:
%s`, summary, roster)
		a.fallback = fmt.Sprintf("Nem todo mundo vai poder para %q.\n%s", summary, roster)
		a.namesToVerify = absent
	}

	return a
}

// sendAllRespondedAnnouncement fires once, when every invited user has
// answered.
func (m *Manager) sendAllRespondedAnnouncement(groupChatID int64, event Event, groupUsers []string) {
	a := buildAnnouncement(event.Summary, groupUsers, event.Confirmations)

	text := m.generateOrFallback(a.prompt, a.fallback)

	// Whichever names matter for this tone -- who bailed, in the roast case;
	// who is late, in the light-jab case -- must actually be named, or the
	// specific dig the prompt asked for did not happen. Same reasoning as the
	// tag check in sendReminder: verify the one concrete thing the message
	// exists to do, rather than trust a paraphrase-prone model to always do it.
	for _, name := range a.namesToVerify {
		if !strings.Contains(text, name) {
			log.Printf("all-responded announcement dropped a name (%s), using fallback", name)
			text = a.fallback
			break
		}
	}

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

	liveSessions := make(map[string][]Event)
	m.storage.LoadFromDB(liveSessionsFileName, &liveSessions)

	archivedEvents := make(map[string][]Event)
	m.storage.LoadFromDB("archived_events.json", &archivedEvents)

	groups := make(map[string]Group)
	m.storage.LoadFromDB(groupsFileName, &groups)

	knownUsers := make(map[string]int64)
	m.storage.LoadFromDB(usersFileName, &knownUsers)

	eventsChanged := false
	liveChanged := false
	archivedChanged := false
	now := time.Now().Unix()

	for chatIDStr, ev := range events {
		if ev.Timestamp > 0 && ev.Timestamp > now {
			if m.maybeSendDailyNudges(&ev, groups[chatIDStr], knownUsers, now) {
				events[chatIDStr] = ev
				eventsChanged = true
			}
		}

		if ev.Timestamp > 0 && !ev.Reminder24hCalloutSent && ev.Timestamp-now <= 24*60*60 && ev.Timestamp > now {
			m.send24hCallout(chatIDStr, ev)
			ev.Reminder24hCalloutSent = true
			events[chatIDStr] = ev
			eventsChanged = true
		}
		if m.meet != nil && ev.Meet == nil {
			if spaceName, joinURI, err := m.meet.CreateSpace(ctx); err != nil {
				log.Printf("could not create Meet space for chat %s: %v", chatIDStr, err)
			} else {
				ev.Meet = &MeetInfo{SpaceName: spaceName, JoinURI: joinURI}
				events[chatIDStr] = ev
				eventsChanged = true
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
			eventsChanged = true
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
			eventsChanged = true
		}

		if ev.Timestamp > 0 && ev.Timestamp <= now {
			if !ev.ReminderNowSent {
				m.sendReminder(chatIDStr, ev, "agora")
				ev.ReminderNowSent = true
			}

			// The event has started. It moves out of the single upcoming-slot
			// map and into the ongoing-session list immediately -- not once
			// its Meet lifecycle finishes -- specifically so create() (which
			// only ever looks at `events`) sees this chat as free to schedule
			// again right away. Its Meet tracking keeps running from the
			// live-sessions loop below on the very next tick.
			liveSessions[chatIDStr] = append(liveSessions[chatIDStr], ev)
			delete(events, chatIDStr)
			eventsChanged = true
			liveChanged = true
			log.Printf("Event '%s' has started for chat %s; now tracked as an ongoing session", ev.Summary, chatIDStr)
			continue
		}

		events[chatIDStr] = ev
	}

	for chatIDStr, sessions := range liveSessions {
		var stillLive []Event
		for _, ev := range sessions {
			updated, finalized, sessionChanged := m.advanceMeetSession(ctx, chatIDStr, ev, now)
			if sessionChanged {
				liveChanged = true
			}
			if finalized {
				archivedEvents[chatIDStr] = append(archivedEvents[chatIDStr], updated)
				archivedChanged = true
				liveChanged = true
				log.Printf("Alert: Event '%s' has been archived for chat %s", updated.Summary, chatIDStr)
				continue
			}
			stillLive = append(stillLive, updated)
		}
		if len(stillLive) == 0 {
			delete(liveSessions, chatIDStr)
		} else {
			liveSessions[chatIDStr] = stillLive
		}
	}

	if eventsChanged {
		m.storage.MustSave(eventsFileName, events)
	}
	if liveChanged {
		m.storage.MustSave(liveSessionsFileName, liveSessions)
	}
	if archivedChanged {
		m.storage.MustSave("archived_events.json", archivedEvents)
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

// reminderMessage is what buildReminderMessage decides, and everything
// sendReminder needs to act on that decision.
type reminderMessage struct {
	prompt        string
	fallback      string
	confirmedTags []string // must all appear in the generated text, or fall back
}

// buildReminderMessage classifies the invite roster and builds the prompt for
// one reminder. The reminder fires regardless of whether everyone answered --
// the session happens at its scheduled time either way -- but the model is
// told the actual roster (who is late, who never answered, who confirmed they
// are not coming) so it can react to that honestly instead of the message
// reading as if everyone is confirmed when they are not.
//
// Pulled out of sendReminder, same as buildAnnouncement was, so the roster
// classification and prompt construction are directly testable without a bot
// double.
func buildReminderMessage(summary string, confirmations map[string]string, timeMsg string) reminderMessage {
	var confirmedUsers, lateUsers, noResponseUsers, absentUsers []string
	for u, conf := range confirmations {
		switch {
		case conf == "💪":
			confirmedUsers = append(confirmedUsers, u)
		case strings.HasPrefix(conf, "🐢"):
			confirmedUsers = append(confirmedUsers, u)
			lateUsers = append(lateUsers, u)
		case conf == "🐔":
			absentUsers = append(absentUsers, u)
		default:
			noResponseUsers = append(noResponseUsers, u)
		}
	}

	if len(confirmedUsers) == 0 {
		return reminderMessage{}
	}

	tags := strings.Join(confirmedUsers, " ")

	prompt := fmt.Sprintf("Gere uma mensagem animada, nerd e curta avisando que a sessão %q vai começar %s. Diga para eles se prepararem.", summary, timeMsg)
	fallback := fmt.Sprintf("Atenção %s! O evento '%s' começa %s!", tags, summary, timeMsg)

	if len(lateUsers) > 0 || len(noResponseUsers) > 0 || len(absentUsers) > 0 {
		prompt += fmt.Sprintf(`

Nem todo mundo respondeu ao convite. Atrasados: %s. Nunca responderam ao convite: %s. Confirmaram que não vão: %s.
Se fizer sentido, inclua uma cutucada leve e proporcional sobre isso -- não trate como uma crise, é só contexto, e a sessão vai acontecer do mesmo jeito. Não cite nenhum nome além dos listados acima.`,
			joinOrNenhum(lateUsers), joinOrNenhum(noResponseUsers), joinOrNenhum(absentUsers))

		fallback += fmt.Sprintf(" (atrasados: %s; sem resposta: %s; não vão: %s)",
			joinOrNenhum(lateUsers), joinOrNenhum(noResponseUsers), joinOrNenhum(absentUsers))
	}

	prompt += fmt.Sprintf("\n\nNo final da mensagem inclua exatamente estas marcações de usuários, sem alterar nada: %s", tags)

	return reminderMessage{prompt: prompt, fallback: fallback, confirmedTags: confirmedUsers}
}

// noResponseUsers lists everyone still on "❔" -- invited, but never having
// answered yes/no/late at all. A missing key counts the same as an explicit
// "❔": both mean nobody has recorded an answer for that user yet.
func noResponseUsers(confirmations map[string]string) []string {
	var users []string
	for u, conf := range confirmations {
		if conf == "" || conf == "❔" {
			users = append(users, u)
		}
	}
	return users
}

const dailyNudgeHour = 9

// maybeSendDailyNudges DMs everyone who has not yet answered an invite, once
// per calendar day at or after 9am in the group's own timezone -- not the
// server's, for the same reason event scheduling itself resolves times
// against the group's recorded zone rather than guessing.
//
// This only ever nudges people already known to have a DM chat with the bot
// (the same requirement create() already has for the initial invite); anyone
// who has never messaged the bot privately cannot be reached this way, and
// that is reported at event-creation time already, not repeated here.
func (m *Manager) maybeSendDailyNudges(ev *Event, group Group, knownUsers map[string]int64, now int64) (changed bool) {
	pending := noResponseUsers(ev.Confirmations)
	if len(pending) == 0 {
		return false
	}

	loc := time.UTC
	if group.Timezone != "" {
		if l, err := time.LoadLocation(group.Timezone); err == nil {
			loc = l
		}
	}

	localNow := time.Unix(now, 0).In(loc)
	if localNow.Hour() < dailyNudgeHour {
		return false
	}

	today := localNow.Format("2006-01-02")
	if ev.LastNoResponseNudgeDate == today {
		return false
	}

	if m.bot != nil {
		for _, u := range pending {
			uid, ok := knownUsers[u]
			if !ok {
				continue
			}
			m.bot.SendMessage(context.Background(), &bot.SendMessageParams{
				ChatID: uid,
				Text:   fmt.Sprintf("Lembrete: você ainda não respondeu se vai para %q (%s). Me avisa quando puder!", ev.Summary, ev.Date),
			})
		}
	}

	log.Printf("Alert: daily no-response nudge sent for event '%s' (%s)", ev.Summary, strings.Join(pending, " "))
	ev.LastNoResponseNudgeDate = today
	return true
}

// send24hCallout posts, once, a public jab at whoever still has not answered
// with 24 hours left before the session -- deliberately public rather than
// another DM, since a private nudge has already been tried daily and this is
// meant to add a little social pressure instead.
func (m *Manager) send24hCallout(chatIDStr string, ev Event) {
	pending := noResponseUsers(ev.Confirmations)
	if len(pending) == 0 {
		return
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return
	}

	names := strings.Join(pending, " ")
	prompt := fmt.Sprintf("Gere uma mensagem engraçada e levemente provocativa no grupo, dedurando que faltam 24 horas para a sessão %q e que estas pessoas ainda não responderam se vão: %s. Peça para elas responderem logo. Não cite ninguém além das listadas. No final da mensagem inclua exatamente estas marcações, sem alterar nada: %s", ev.Summary, names, names)
	fallback := fmt.Sprintf("Atenção %s! Faltam 24 horas para %q e vocês ainda não responderam se vão. Bora responder!", names, ev.Summary)

	text := m.generateOrFallback(prompt, fallback)

	// Same reasoning as the reminder tags: the point of the message is naming
	// these specific people, so verify rather than trust.
	for _, u := range pending {
		if !strings.Contains(text, u) {
			log.Printf("24h callout dropped a tag for %s, using fallback", u)
			text = fallback
			break
		}
	}

	log.Printf("Alert: 24h no-response callout sent for event '%s' to chat %s (%s)", ev.Summary, chatIDStr, names)

	params := &bot.SendMessageParams{ChatID: chatID, Text: text}
	if ev.MessageID != 0 {
		params.ReplyParameters = &models.ReplyParameters{MessageID: ev.MessageID}
	}
	if m.bot != nil {
		m.bot.SendMessage(context.Background(), params)
	}
}

func (m *Manager) sendReminder(chatIDStr string, ev Event, when string) {
	timeMsg := "daqui a " + when
	if when == "agora" {
		timeMsg = "AGORA"
	}

	rm := buildReminderMessage(ev.Summary, ev.Confirmations, timeMsg)
	if len(rm.confirmedTags) == 0 {
		return
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return
	}

	prompt, fallback, confirmedUsers := rm.prompt, rm.fallback, rm.confirmedTags

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

	hasMeetLink := ev.Meet != nil && ev.Meet.JoinURI != ""
	text = appendMeetLink(text, ev.Meet)

	log.Printf("Alert: Reminder sent for event '%s' (%s) to chat %s", ev.Summary, timeMsg, chatIDStr)

	params := &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	}
	if hasMeetLink {
		// Telegram's preview for a Meet link is a generic "Google Meet" card
		// with no useful information -- just visual noise ahead of the actual
		// reminder text.
		params.LinkPreviewOptions = &models.LinkPreviewOptions{IsDisabled: bot.True()}
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
