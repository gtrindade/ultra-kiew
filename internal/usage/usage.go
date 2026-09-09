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
}

// Recorder owns the usage log.
type Recorder struct {
	storage *storage.Client
	bot     *bot.Bot
}

func NewRecorder(storageClient *storage.Client) *Recorder {
	return &Recorder{storage: storageClient}
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

// PromptsSince counts the prompts one user has spent since a moment.
//
// Matching is case-insensitive because Telegram handles are, and a user
// recorded as @Alice must not get a second allowance as @alice.
func (r *Recorder) PromptsSince(user string, since time.Time) (int, error) {
	cutoff := since.Unix()
	count := 0
	err := r.scan(func(e Entry) {
		if e.Kind == KindPrompt && e.Timestamp >= cutoff && strings.EqualFold(e.User, user) {
			count++
		}
	})
	return count, err
}

// Allowance reports how much of the quota a user has left right now.
//
// A read failure returns the full allowance rather than none. The quota exists
// to stop runaway use, not to be a second way for a broken disk to take the
// bot down, so an unreadable log fails open and says so in the server log.
func (r *Recorder) Allowance(user string) (remaining int, used int) {
	used, err := r.PromptsSince(user, time.Now().Add(-QuotaWindow))
	if err != nil {
		log.Printf("usage: could not read the log to check %s's quota, allowing the message: %v", user, err)
		return DailyPromptLimit, 0
	}
	remaining = DailyPromptLimit - used
	if remaining < 0 {
		remaining = 0
	}
	return remaining, used
}

// QuotaMessage is what a user is told when they have nothing left.
func QuotaMessage(used int) string {
	return fmt.Sprintf(
		"Você já usou %d mensagens comigo nas últimas 24 horas, que é o limite. "+
			"A cota vai liberando aos poucos conforme as mensagens antigas completam 24h -- tenta de novo mais tarde.",
		used)
}

// userTotals is one row of a report.
type userTotals struct {
	User    string
	Prompts int
	Blocked int
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
