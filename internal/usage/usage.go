// Package usage records who talks to the bot, enforces a per-user quota, and
// answers questions about both.
//
// The log is append-only (see storage.AppendJSONL). Every served turn adds one
// line and nothing is ever rewritten, so a crash can cost at most the record
// being written and never the history behind it.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	// UsageReportToolName is the tool the model calls to answer "me dá o
	// relatório de uso".
	UsageReportToolName = "usage_report"

	// GrantQuotaToolName is the tool an admin calls to hand someone more
	// allowance.
	GrantQuotaToolName = "grant_quota"

	// QuotaStatusToolName is the tool for "how much has everyone got left".
	QuotaStatusToolName = "quota_status"

	// usersFileName maps @handle to Telegram chat ID. Used here only to
	// identify the caller and to check a grant is aimed at somebody real.
	usersFileName = "users.json"

	// maxGrant caps a single grant. A quota exists to bound runaway use, and
	// a mistyped "500000" that silently removed the bound would defeat it
	// more quietly than having no limit at all.
	maxGrant = 500

	// LogFileName is the append-only log, under the storage database path.
	// Exported so an operator knows what to back up or rotate, and so tests
	// can start from a clean one.
	LogFileName = "usage.jsonl"

	// DailyPromptLimit is how many messages one person may spend in
	// QuotaWindow. Every served turn costs one, with no exemptions -- see
	// telegram.allowTurn for why there deliberately are none.
	DailyPromptLimit = 50

	// QuotaWindow is a rolling window rather than a calendar day: the quota
	// recovers gradually instead of all at once, and there is no midnight at
	// which someone can spend 100 in two hours.
	QuotaWindow = 24 * time.Hour

	// defaultReportWindow is what "me dá o relatório de uso" means with no
	// period attached.
	defaultReportWindow = 24 * time.Hour

	// maxReportWindow caps how far back one report may reach. The log is read
	// start to finish, so an unbounded window would eventually make a single
	// chat message walk years of records.
	maxReportWindow = 365 * 24 * time.Hour
)

// Kind is what one logged turn was.
type Kind string

const (
	// KindPrompt is a served turn, and the only kind that spends quota.
	KindPrompt Kind = "prompt"

	// KindGrant is extra allowance handed out by an admin, carrying its size
	// in Amount. It is logged rather than stored as a mutable per-user limit
	// so that it rolls off on exactly the same 24h window as the usage it
	// offsets -- a grant is "here is more for today", not a permanent raise,
	// and it needs no second state file to expire.
	KindGrant Kind = "grant"

	// KindBlocked is a turn that was refused because the quota was already
	// spent. Logged so "is the limit biting anyone?" is answerable, and never
	// counted against the quota it was already stopped by.
	KindBlocked Kind = "blocked"
)

// Entry is one line of the log.
//
// Field names are short because every served message writes one of these
// forever.
type Entry struct {
	Timestamp int64  `json:"ts"`
	User      string `json:"user"`
	UserID    int64  `json:"uid,omitempty"`
	ChatID    int64  `json:"cid"`
	ChatTitle string `json:"chat,omitempty"`
	Private   bool   `json:"dm,omitempty"`
	Kind      Kind   `json:"kind"`

	// Amount is the size of a KindGrant, and By is the admin who made it.
	// Unused by every other kind.
	Amount int    `json:"amt,omitempty"`
	By     string `json:"by,omitempty"`
}

// Recorder owns the usage log.
type Recorder struct {
	storage *storage.Client
	bot     *bot.Bot
	admins  []string
}

func NewRecorder(storageClient *storage.Client) *Recorder {
	return &Recorder{storage: storageClient}
}

// SetAdmins sets who may grant quota, from config.yaml's admin_users.
//
// Empty means nobody, and there is no built-in fallback: an unconfigured
// deployment must not hand the power to raise limits to whoever asks first,
// and guessing an owner from the source would be a worse answer than having
// none. The cost is that a server whose config.yaml lacks admin_users has a
// quota nobody can lift, so main logs the active list -- loudly when it is
// empty -- rather than leaving that to be discovered by a refusal.
func (r *Recorder) SetAdmins(handles []string) {
	r.admins = append([]string(nil), handles...)
}

// Admins returns who may currently grant quota, for logging at startup.
func (r *Recorder) Admins() []string {
	return append([]string(nil), r.admins...)
}

// SetBot wires in the Telegram bot so a report can be posted to the chat
// directly. Leaving it unset is supported; the report is then handed back to
// the model instead.
func (r *Recorder) SetBot(b *bot.Bot) {
	r.bot = b
}

// Record appends one entry, filling in the timestamp if the caller left it
// unset.
//
// A failure here is logged, never returned to the caller: usage accounting
// must not be able to stop the bot from answering someone. The cost of a lost
// line is one uncounted message, which is strictly better than a user staring
// at silence because a disk was full.
func (r *Recorder) Record(entry Entry) {
	if entry.Timestamp == 0 {
		entry.Timestamp = time.Now().Unix()
	}
	if err := r.storage.AppendJSONL(LogFileName, entry); err != nil {
		log.Printf("usage: could not record a %s by %s: %v", entry.Kind, entry.User, err)
	}
}

// scan walks the log, handing every decodable entry to fn.
//
// An undecodable line is skipped rather than fatal. The only way to get one is
// a process killed mid-append leaving a truncated final line, and losing the
// whole report over the last partial record would be the wrong trade.
func (r *Recorder) scan(fn func(Entry)) error {
	skipped := 0
	err := r.storage.ScanJSONL(LogFileName, func(line []byte) error {
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			// Swallowing this is the point, and nilerr is right to ask. There
			// is exactly one way to produce a bad line -- a process killed
			// mid-append, leaving a truncated final record -- and returning
			// the error would abandon every intact record before it. Counting
			// and moving on is the correct trade for an append-only log.
			skipped++
			return nil //nolint:nilerr // a torn line must not abort the read
		}
		fn(entry)
		return nil
	})
	if skipped > 0 {
		log.Printf("usage: skipped %d unreadable line(s) in %s", skipped, LogFileName)
	}
	return err
}

// Standing is where one person sits against their quota right now.
type Standing struct {
	// User is who this is about.
	User string
	// Unlimited is true for an administrator, who is never rationed. Whoever
	// can lift everyone else's limit gains nothing from having one of their
	// own, and being locked out by it would leave nobody able to unlock it.
	Unlimited bool
	// Used is messages spent inside the window.
	Used int
	// Granted is extra allowance handed out inside the window.
	Granted int
	// Limit is what they are actually allowed: the base plus any grants.
	Limit int
	// Remaining never goes below zero -- a negative would read as a debt.
	Remaining int
}

// Standing reports where a user sits against their quota.
//
// Matching is case-insensitive because Telegram handles are, and a user
// recorded as @Alice must not get a second allowance as @alice.
//
// A read failure returns a full, untouched allowance rather than none. The
// quota exists to stop runaway use, not to be a second way for a broken disk
// to take the bot down, so an unreadable log fails open and says so.
func (r *Recorder) Standing(user string) Standing {
	cutoff := time.Now().Add(-QuotaWindow).Unix()
	unlimited := r.isAdmin(user)

	var used, granted int
	err := r.scan(func(e Entry) {
		if e.Timestamp < cutoff || !strings.EqualFold(e.User, user) {
			return
		}
		switch e.Kind {
		case KindPrompt:
			used++
		case KindGrant:
			granted += e.Amount
		}
	})
	if err != nil {
		log.Printf("usage: could not read the log to check %s's quota, allowing the message: %v", user, err)
		return Standing{User: user, Unlimited: unlimited, Limit: DailyPromptLimit, Remaining: DailyPromptLimit}
	}

	limit := DailyPromptLimit + granted
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return Standing{User: user, Unlimited: unlimited, Used: used, Granted: granted, Limit: limit, Remaining: remaining}
}

// Allowed reports whether this person may send another message.
func (s Standing) Allowed() bool {
	return s.Unlimited || s.Remaining > 0
}

// Describe renders one person's position in a single line.
func (s Standing) Describe() string {
	if s.Unlimited {
		return fmt.Sprintf("%s — %s (admin, sem limite)", s.User, plural(s.Used, "mensagem", "mensagens"))
	}
	line := fmt.Sprintf("%s — %d de %d, restam %d", s.User, s.Used, s.Limit, s.Remaining)
	if s.Granted > 0 {
		line += fmt.Sprintf(" (inclui +%d de cota extra)", s.Granted)
	}
	return line
}

// QuotaMessage is what a user is told when they have nothing left.
//
// admins comes from config rather than being written into the sentence,
// because the one thing worse than "you are out of messages" is being told to
// go ask somebody who cannot help. With none configured the offer is omitted
// entirely rather than pointed at nobody.
func QuotaMessage(s Standing, admins []string) string {
	msg := fmt.Sprintf(
		"Você já usou %d de %d mensagens nas últimas 24 horas, que é o seu limite. "+
			"A cota vai liberando aos poucos conforme as mensagens antigas completam 24h -- tenta de novo mais tarde",
		s.Used, s.Limit)

	switch len(admins) {
	case 0:
		return msg + "."
	case 1:
		return fmt.Sprintf("%s, ou fala com %s se precisar de mais agora.", msg, admins[0])
	default:
		return fmt.Sprintf("%s, ou fala com um dos admins (%s) se precisar de mais agora.",
			msg, strings.Join(admins, ", "))
	}
}

// userTotals is one row of a report.
type userTotals struct {
	User    string
	Prompts int
	Blocked int
	Granted int
}

// Report is the aggregate answer for one window and scope.
type Report struct {
	Since time.Time
	Until time.Time
	// ChatTitle is empty for a global report.
	ChatTitle string
	// ChatID is 0 for a global report.
	ChatID int64
	Rows   []userTotals
	Chats  int
}

// Totals sums the rows.
func (rep Report) Totals() (prompts, blocked int) {
	for _, row := range rep.Rows {
		prompts += row.Prompts
		blocked += row.Blocked
	}
	return prompts, blocked
}

// Build aggregates the log for a window, optionally narrowed to one chat.
//
// chatID 0 means every chat.
func (r *Recorder) Build(since, until time.Time, chatID int64) (Report, error) {
	rep := Report{Since: since, Until: until, ChatID: chatID}
	from, to := since.Unix(), until.Unix()

	byUser := map[string]*userTotals{}
	chats := map[int64]bool{}

	err := r.scan(func(e Entry) {
		if e.Timestamp < from || e.Timestamp > to {
			return
		}
		if chatID != 0 && e.ChatID != chatID {
			return
		}
		chats[e.ChatID] = true
		if chatID != 0 && e.ChatTitle != "" {
			rep.ChatTitle = e.ChatTitle
		}

		key := strings.ToLower(e.User)
		row := byUser[key]
		if row == nil {
			row = &userTotals{User: e.User}
			byUser[key] = row
		}
		switch e.Kind {
		case KindPrompt:
			row.Prompts++
		case KindBlocked:
			row.Blocked++
		case KindGrant:
			row.Granted += e.Amount
		}
	})
	if err != nil {
		return rep, err
	}

	rep.Chats = len(chats)
	for _, row := range byUser {
		rep.Rows = append(rep.Rows, *row)
	}
	// Busiest first, then by name so equal counts do not shuffle between two
	// reports over the same data.
	sort.Slice(rep.Rows, func(i, j int) bool {
		a, b := rep.Rows[i], rep.Rows[j]
		if a.Prompts != b.Prompts {
			return a.Prompts > b.Prompts
		}
		return strings.ToLower(a.User) < strings.ToLower(b.User)
	})
	return rep, nil
}

// Render turns a report into the message posted to the chat.
func (rep Report) Render() string {
	var sb strings.Builder

	scope := "todos os grupos"
	if rep.ChatID != 0 {
		scope = "este grupo"
		if rep.ChatTitle != "" {
			scope = fmt.Sprintf("%q", rep.ChatTitle)
		}
	}

	fmt.Fprintf(&sb, "Relatório de uso — %s\n%s até %s\n\n",
		scope,
		rep.Since.Local().Format("02/01 15:04"),
		rep.Until.Local().Format("02/01 15:04"))

	if len(rep.Rows) == 0 {
		sb.WriteString("Ninguém falou comigo nesse período.")
		return sb.String()
	}

	for _, row := range rep.Rows {
		fmt.Fprintf(&sb, "%s — %s", row.User, plural(row.Prompts, "mensagem", "mensagens"))
		if row.Blocked > 0 {
			fmt.Fprintf(&sb, ", %s no limite", plural(row.Blocked, "bloqueada", "bloqueadas"))
		}
		if row.Granted > 0 {
			fmt.Fprintf(&sb, ", +%d de cota extra", row.Granted)
		}
		if remaining := DailyPromptLimit - row.Prompts; remaining <= 10 && rep.showsRemaining() {
			fmt.Fprintf(&sb, " (restam %d)", max(remaining, 0))
		}
		sb.WriteString("\n")
	}

	prompts, blocked := rep.Totals()
	fmt.Fprintf(&sb, "\nTotal: %s de %s",
		plural(prompts, "mensagem", "mensagens"),
		plural(len(rep.Rows), "pessoa", "pessoas"))
	if rep.ChatID == 0 && rep.Chats > 1 {
		fmt.Fprintf(&sb, " em %d chats", rep.Chats)
	}
	if blocked > 0 {
		fmt.Fprintf(&sb, ". %s pelo limite de %d/24h",
			plural(blocked, "mensagem barrada", "mensagens barradas"), DailyPromptLimit)
	}
	sb.WriteString(".")
	return sb.String()
}

// showsRemaining reports whether "restam N" would be a true statement about
// this report's rows, rather than arithmetic that merely looks like one.
//
// Two conditions, and the second is the subtle one:
//
//   - The window has to be the quota window, ending now. Subtracting a week's
//     or a month's total from a daily limit is meaningless.
//   - The report has to be global. The quota is per person across every chat,
//     but a group-scoped row only counts that group -- so "restam 3" on a
//     group report would be wrong for anyone who also talks to the bot
//     elsewhere, and wrong in the dangerous direction: it would promise
//     headroom that is already spent.
func (rep Report) showsRemaining() bool {
	if rep.ChatID != 0 {
		return false
	}
	span := rep.Until.Sub(rep.Since)
	if span < QuotaWindow-time.Minute || span > QuotaWindow+time.Minute {
		return false
	}
	return time.Since(rep.Until) < 5*time.Minute
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Manage is the tool entry point.
//
// Scope is decided by where the question was asked, not by an argument the
// model supplies: in a group it is that group's usage, in a DM it is
// everything. That mirrors how every other tool here treats the caller's chat
// -- the code knows it, so the model is never asked and can never get it
// wrong.
func (r *Recorder) Manage(args map[string]any) (string, error) {
	chatID, ok := args[googlegenai.ArgCallerChatID].(int64)
	if !ok {
		return "", fmt.Errorf("internal error: caller chat context is missing")
	}
	isPrivate, _ := args[googlegenai.ArgIsPrivate].(bool)

	since, until, err := resolveWindow(args)
	if err != nil {
		return "", err
	}

	// A DM is the operator's console: report on everything. A group chat sees
	// only itself.
	scopeChatID := chatID
	if isPrivate {
		scopeChatID = 0
	}

	report, err := r.Build(since, until, scopeChatID)
	if err != nil {
		return "", fmt.Errorf("could not read the usage log: %w", err)
	}
	if scopeChatID != 0 && report.ChatTitle == "" {
		// No records in range carried a title, so fall back to the live one.
		report.ChatTitle, _ = args[googlegenai.ArgChatTitle].(string)
	}

	rendered := report.Render()

	// Posted directly rather than handed back for the model to repeat. This
	// is the one tool whose entire output is numbers, and a model that
	// paraphrases "23" as "umas 20" makes the feature worthless. Same reason
	// group_manage posts its own missing-user warning.
	if r.bot != nil {
		if _, err := r.bot.SendMessage(context.Background(), &bot.SendMessageParams{
			ChatID: chatID,
			Text:   rendered,
		}); err != nil {
			log.Printf("usage: could not post the report to chat %d, falling back to the model: %v", chatID, err)
			return rendered, nil
		}
		return "The usage report has ALREADY been posted to the chat, in full, by the code. Do not repeat it, do not summarise it, and above all do not restate any of the numbers. Just acknowledge it in one short sentence.", nil
	}

	return rendered, nil
}

// resolveWindow works out which period a report covers.
//
// Unlike event scheduling, an explicit offset is required here rather than
// refused. The reasoning is opposite because the situation is: an event is a
// future wall-clock time whose offset the model cannot know, while a report
// boundary is a point on the timeline the model has just been shown -- the
// prompt hands it <current_time> in exactly this format, so it has a correct
// example in front of it and nothing to guess.
func resolveWindow(args map[string]any) (since, until time.Time, err error) {
	now := time.Now()
	until = now

	if raw, ok := args["until"].(string); ok && strings.TrimSpace(raw) != "" {
		until, err = parseInstant(raw, "until")
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}

	sinceRaw, hasSince := args["since"].(string)
	if hasSince && strings.TrimSpace(sinceRaw) != "" {
		since, err = parseInstant(sinceRaw, "since")
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	} else {
		window := defaultReportWindow
		if hours, ok := numberArg(args, "hours"); ok {
			if hours <= 0 {
				return time.Time{}, time.Time{}, fmt.Errorf("'hours' must be greater than zero")
			}
			window = time.Duration(hours * float64(time.Hour))
		}
		since = until.Add(-window)
	}

	if !until.After(since) {
		return time.Time{}, time.Time{}, fmt.Errorf("the period ends at or before it starts (%s to %s). Ask the user which period they meant",
			since.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	if until.Sub(since) > maxReportWindow {
		return time.Time{}, time.Time{}, fmt.Errorf("that period is longer than a year. Ask the user for a shorter one")
	}
	return since, until, nil
}

func parseInstant(value, field string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04Z0700"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	// A bare date is unambiguous enough to accept, and is what someone means
	// by "desde dia 1". Local midnight, since that is the day they live in.
	if t, err := time.ParseInLocation("2006-01-02", value, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("could not read %q as a time for '%s'. Use a full timestamp with offset, copied from the current_time you were given (like 2026-09-08T14:30:00-03:00), or a plain date like 2026-09-08", value, field)
}

// numberArg reads a JSON number, which arrives as float64 but which a model
// will sometimes send as a string instead.
func numberArg(args map[string]any, key string) (float64, bool) {
	switch v := args[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func GetToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: UsageReportToolName,
				Description: "Reports who has been talking to the bot and how much. " +
					"Call this for any question about usage, activity, quotas or how many messages people have sent -- " +
					"'me dá o relatório de uso das últimas 24h', 'quem mais falou com você essa semana', 'quanto eu já usei hoje'. " +
					"The report is posted to the chat by the code itself, so once this returns you only acknowledge it briefly and NEVER restate the numbers. " +
					"Scope is automatic and is not yours to choose: asked in a group it covers that group, asked in a DM it covers every group. " +
					"Defaults to the last 24 hours when no period is given.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"hours": {
							Type:        "number",
							Description: "How many hours back to look from now. Use this for relative periods: 24 for the last day, 168 for the last week. Ignored if 'since' is given.",
							Example:     24,
						},
						"since": {
							Type:        "string",
							Description: "Start of the period, as a full timestamp with offset copied from the current_time you were given (2026-09-08T14:30:00-03:00), or a plain date (2026-09-08). Only for an explicit period the user named.",
						},
						"until": {
							Type:        "string",
							Description: "End of the period, same format as 'since'. Defaults to now.",
						},
					},
				},
			},
		},
	}
}

// callerHandle identifies who is calling, from the chat the message arrived
// in.
//
// This works only in a private chat, where Telegram's chat ID is the user's
// own ID, so users.json resolves it to a handle. That is the same trick
// event.updateStatus uses, and it is deliberate: the caller's identity comes
// from Telegram, never from an argument the model filled in. An admin check
// the model could satisfy by claiming to be someone would not be an admin
// check at all.
func (r *Recorder) callerHandle(callerChatID int64) string {
	knownUsers := make(map[string]int64)
	r.storage.LoadOrLog(usersFileName, &knownUsers)
	for handle, id := range knownUsers {
		if id == callerChatID {
			return handle
		}
	}
	return ""
}

func (r *Recorder) isAdmin(handle string) bool {
	for _, admin := range r.admins {
		if strings.EqualFold(admin, handle) {
			return true
		}
	}
	return false
}

// Grant hands someone extra allowance for the rest of the quota window.
//
// Private chat only, and only for an admin. Both restrictions are enforced
// here rather than described in the prompt, because a rule the model can be
// talked out of is not a rule -- and this one governs who gets to remove a
// limit.
func (r *Recorder) Grant(args map[string]any) (string, error) {
	callerChatID, ok := args[googlegenai.ArgCallerChatID].(int64)
	if !ok {
		return "", fmt.Errorf("internal error: caller chat context is missing")
	}
	isPrivate, _ := args[googlegenai.ArgIsPrivate].(bool)

	if !isPrivate {
		return "", fmt.Errorf("quota can only be granted in a private DM with the bot, and this is a group chat. Tell the user to do it in their DM with me")
	}

	caller := r.callerHandle(callerChatID)
	if caller == "" || !r.isAdmin(caller) {
		// One message for "not an admin" and for "not recognised at all". The
		// difference is of no use to whoever is asking, and spelling it out
		// would tell them how close they got.
		return "", fmt.Errorf("only an administrator can change someone's quota, and this user is not one. Say so plainly and do not offer to do it another way")
	}

	target, _ := args["user"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("invalid argument: 'user' is required. Ask which person the extra quota is for, as an @username")
	}
	if !strings.HasPrefix(target, "@") {
		target = "@" + target
	}

	amount, ok := intArg(args, "amount")
	if !ok {
		return "", fmt.Errorf("invalid argument: 'amount' is required and must be a whole number of extra messages")
	}
	if amount <= 0 {
		return "", fmt.Errorf("'amount' must be greater than zero. To take quota away, there is no tool: it expires on its own within %s", QuotaWindow)
	}
	if amount > maxGrant {
		return "", fmt.Errorf("%d is more than the %d maximum for one grant. Confirm the number with the user before trying again", amount, maxGrant)
	}

	// A grant aimed at a handle nobody owns is a silent no-op that looks like
	// success, which is the worst outcome available here: the admin believes
	// they helped and the person keeps hitting the wall.
	knownUsers := make(map[string]int64)
	r.storage.LoadOrLog(usersFileName, &knownUsers)
	resolved := ""
	for handle := range knownUsers {
		if strings.EqualFold(handle, target) {
			resolved = handle
			break
		}
	}
	if resolved == "" {
		return "", fmt.Errorf("this bot has never seen %s, so granting them quota would do nothing. Check the @username -- they have to have talked to me at least once", target)
	}

	r.Record(Entry{
		User:   resolved,
		ChatID: callerChatID,
		Kind:   KindGrant,
		Amount: amount,
		By:     caller,
	})

	after := r.Standing(resolved)
	log.Printf("usage: %s granted %s +%d messages (now %d/%d used)", caller, resolved, amount, after.Used, after.Limit)

	return fmt.Sprintf(
		"Granted %s an extra %d messages. Their limit is now %d for the next 24 hours (%d already used, %d left), after which it returns to %d. Tell the user this plainly.",
		resolved, amount, after.Limit, after.Used, after.Remaining, DailyPromptLimit), nil
}

// intArg reads a whole number, tolerating the float64 a JSON number decodes to
// and the string a model sometimes sends instead.
func intArg(args map[string]any, key string) (int, bool) {
	value, ok := numberArg(args, key)
	if !ok || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

func GetGrantToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: GrantQuotaToolName,
				Description: "Gives one person extra messages on top of the standard daily limit, for the next 24 hours. " +
					"Only an administrator can do this, and only in a private DM -- the code checks both and will refuse, so never promise it before calling. " +
					"If it refuses, relay that plainly and do not look for another way to do the same thing. " +
					"Use it when an administrator says something like 'da mais 50 pro @fulano' or 'aumenta a cota do @fulano'.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"user": {
							Type:        "string",
							Description: "The @username to give extra messages to.",
							Example:     "@bmaraujo",
						},
						"amount": {
							Type:        "number",
							Description: "How many extra messages to add, a whole number greater than zero.",
							Example:     50,
						},
					},
					Required: []string{"user", "amount"},
				},
			},
		},
	}
}

// QuotaStatus answers "quanto fulano já usou" and "como está a cota de todo
// mundo".
//
// Private chat only, for the same reason Grant is: the caller is identified
// from the chat ID, and in a group there is nothing to identify them with. An
// administrator may ask about anyone, or about everyone at once. Anyone else
// may ask about themselves and nobody else -- not because a remaining count is
// a secret, but because a tool that answered for arbitrary handles would let
// the model be talked into a roll call by whoever asked nicely.
func (r *Recorder) QuotaStatus(args map[string]any) (string, error) {
	callerChatID, ok := args[googlegenai.ArgCallerChatID].(int64)
	if !ok {
		return "", fmt.Errorf("internal error: caller chat context is missing")
	}
	isPrivate, _ := args[googlegenai.ArgIsPrivate].(bool)

	if !isPrivate {
		return "", fmt.Errorf("quotas can only be checked in a private DM with the bot, since that is the only place I can tell who is asking. Tell the user to ask me directly")
	}

	caller := r.callerHandle(callerChatID)
	if caller == "" {
		return "", fmt.Errorf("this user is not recognised yet, so their quota cannot be looked up")
	}
	admin := r.isAdmin(caller)

	target, _ := args["user"].(string)
	target = strings.TrimSpace(target)
	if target != "" && !strings.HasPrefix(target, "@") {
		target = "@" + target
	}

	// Everyone, which only an admin may ask for.
	if target == "" {
		if !admin {
			return r.Standing(caller).Describe() + "\n\n(Só um administrador pode ver a cota dos outros.)", nil
		}
		return r.everyoneStanding()
	}

	if !admin && !strings.EqualFold(target, caller) {
		return "", fmt.Errorf("only an administrator can look up somebody else's quota. Tell the user they can ask about their own")
	}
	return r.Standing(target).Describe(), nil
}

// everyoneStanding reports where every person the bot has ever seen sits right
// now, busiest first.
func (r *Recorder) everyoneStanding() (string, error) {
	knownUsers := make(map[string]int64)
	r.storage.LoadOrLog(usersFileName, &knownUsers)

	// Anyone with activity in the window counts too, even if users.json has
	// somehow lost them -- the log is the record of what actually happened.
	seen := map[string]string{}
	for handle := range knownUsers {
		seen[strings.ToLower(handle)] = handle
	}
	cutoff := time.Now().Add(-QuotaWindow).Unix()
	if err := r.scan(func(e Entry) {
		if e.Timestamp >= cutoff && e.User != "" {
			if _, known := seen[strings.ToLower(e.User)]; !known {
				seen[strings.ToLower(e.User)] = e.User
			}
		}
	}); err != nil {
		return "", fmt.Errorf("could not read the usage log: %w", err)
	}

	if len(seen) == 0 {
		return "Ninguém falou comigo ainda, então não há cota para mostrar.", nil
	}

	standings := make([]Standing, 0, len(seen))
	for _, handle := range seen {
		standings = append(standings, r.Standing(handle))
	}
	// Busiest first, then by name so two calls over the same data agree.
	sort.Slice(standings, func(i, j int) bool {
		if standings[i].Used != standings[j].Used {
			return standings[i].Used > standings[j].Used
		}
		return strings.ToLower(standings[i].User) < strings.ToLower(standings[j].User)
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "Cotas agora (últimas %d horas, limite base %d):\n\n", int(QuotaWindow.Hours()), DailyPromptLimit)
	for _, s := range standings {
		sb.WriteString(s.Describe())
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func GetQuotaStatusToolConfig() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name: QuotaStatusToolName,
				Description: "Reports how much of their message quota people have left right now. " +
					"Call it for 'quanto eu já usei', 'como está a cota do @fulano', 'mostra as cotas de todo mundo'. " +
					"Leave 'user' out to cover everyone, which only an administrator may do; give a 'user' to ask about one person. " +
					"Anyone may ask about themselves; only an administrator may ask about somebody else, and the code enforces that. " +
					"Private DM only. This is about quota standing right now -- for how much people TALKED to you over some period, use " + UsageReportToolName + " instead.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"user": {
							Type:        "string",
							Description: "The @username to check. Omit to list everyone.",
							Example:     "@bmaraujo",
						},
					},
				},
			},
		},
	}
}
