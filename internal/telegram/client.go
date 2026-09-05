package telegram

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

// contextCarryOver is how many recent messages survive a turn the bot answered.
// Enough to keep a short exchange coherent across a restart, small enough that
// the model is not re-reading the same backlog on every mention.
const contextCarryOver = 20

const lineSep = "\n"

// How much of a replied-to message is quoted, in runes.
//
// Two limits because the two renderings have different jobs. The backlog line
// only needs enough to tell one referent from another, and has to stay on one
// line; the block shown for the message actually being answered is the one the
// model reasons over, so it gets room for something as long as an event card.
const (
	replyQuoteMaxRunes   = 80
	replyContextMaxRunes = 600
)

// Client represents the Telegram bot client.
type Client struct {
	bot            *bot.Bot
	ai             *googlegenai.Client
	storage        *storage.Client
	botName        string
	lock           sync.RWMutex
	chatHistory    map[int64][]*SavedMessage
	maxHistorySize int
	users          map[string]int64
	usersLock      sync.RWMutex

	// selfID is this bot's own Telegram user ID, so a reply to something the
	// bot said can be recognised by identity rather than by matching the
	// configured bot_name against a handle. The two are not required to be
	// the same string, and another bot in the group is not this one.
	selfID int64
}

// SavedMessage represents a message saved from a user.
type SavedMessage struct {
	UserID    int64
	UserName  string
	Text      string
	Timestamp time.Time

	// ReplyToUser and ReplyToText record what this message was a reply to, if
	// anything. Both are omitempty so a chat_history.json written before these
	// existed still decodes.
	//
	// Without them a reply reached the model as a bare "sim", "esse mesmo" or
	// "bora" with no referent: Telegram shows the quoted message in the
	// client, but the update only carried the new text. That is worst exactly
	// where replies are most natural -- answering the bot own event card,
	// which is not in the backlog at all and may predate the current genai
	// session by a restart.
	ReplyToUser string `json:",omitempty"`
	ReplyToText string `json:",omitempty"`
}

func (m *SavedMessage) String() string {
	return fmt.Sprintf("[%s - %s]%s: `%s`",
		m.Timestamp.Format(time.RFC3339), m.UserName, m.replyMarker(), m.Text)
}

// replyMarker renders the reply as a parenthetical inside the existing line
// format, rather than as a second line.
//
// One line is a hard requirement, not tidiness: googlegenai.leakedLineRegex
// strips echoed transcript lines one at a time, and a continuation line would
// slip past it -- which is the exact failure that once had the bot quoting
// messages real users never sent.
func (m *SavedMessage) replyMarker() string {
	if m.ReplyToUser == "" && m.ReplyToText == "" {
		return ""
	}
	who := m.ReplyToUser
	if who == "" {
		who = "alguem"
	}
	quoted := truncateRunes(strings.Join(strings.Fields(m.ReplyToText), " "), replyQuoteMaxRunes)
	if quoted == "" {
		return fmt.Sprintf(" (em resposta a %s)", who)
	}
	return fmt.Sprintf(" (em resposta a %s: %q)", who, quoted)
}

// ReplyContext renders the full quoted message for the <replying_to> block,
// keeping the line breaks that make something like an event card readable.
// Returns "" when the message was not a reply.
func (m *SavedMessage) ReplyContext() string {
	if m.ReplyToUser == "" && m.ReplyToText == "" {
		return ""
	}
	who := m.ReplyToUser
	if who == "" {
		who = "alguem"
	}
	if m.ReplyToText == "" {
		return fmt.Sprintf("%s (mensagem sem texto)", who)
	}
	return fmt.Sprintf("%s: %s", who, truncateRunes(m.ReplyToText, replyContextMaxRunes))
}

// truncateRunes cuts to at most maxRunes runes -- runes rather than bytes
// because everything here is Portuguese prose and card emoji, where a byte cut
// lands mid-character.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return strings.TrimRight(string(runes[:maxRunes]), " ") + "..."
}

// NewBot creates a new Telegram bot client with the provided configuration and AI client.
func NewBot(config *config.Config, ai *googlegenai.Client, storageClient *storage.Client) (*Client, error) {
	c := &Client{
		storage:        storageClient,
		ai:             ai,
		chatHistory:    make(map[int64][]*SavedMessage),
		maxHistorySize: 600,
		users:          make(map[string]int64),
	}
	opts := []bot.Option{
		bot.WithDefaultHandler(c.handler),
		bot.WithCheckInitTimeout(time.Second * 30),
		// The library's default is to spawn a new, unsynchronized goroutine
		// per incoming update. Every event/group tool handler does a plain
		// load-mutate-save on a JSON file with no locking across that whole
		// sequence, so two updates landing close together (three quick
		// create/remove cycles in the same chat, or several people answering
		// their DM invite around the same moment) can interleave: an older
		// create() whose SendMessage call happened to take longer can finish
		// and persist its card's message ID *after* a newer create/remove
		// already saved -- a plain lost update. That is what "the status was
		// recorded against the right event, but the card that got edited was
		// the wrong one" actually was: not a wrong event, a stale MessageID
		// from a write that landed out of order. This bot serves a handful of
		// people scheduling RPG sessions; there is no throughput reason to
		// risk that for concurrency, so updates are processed one at a time.
		bot.WithNotAsyncHandlers(),
	}

	if config.TelegramBotToken == "" {
		return nil, fmt.Errorf("missing telegram_bot_token in config.yaml")
	}

	b, err := bot.New(config.TelegramBotToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	c.bot = b
	c.botName = config.BotName
	// Parsed straight out of the token; no network call.
	c.selfID = b.ID()

	err = c.storage.LoadChatHistory(&c.chatHistory)
	if err != nil {
		return nil, fmt.Errorf("failed to load chat history: %w", err)
	}

	c.storage.LoadOrLog("users.json", &c.users)

	return c, nil
}

// Start starts the Telegram bot and listens for updates, returning when the
// caller's context is cancelled or the process is interrupted.
//
// The interrupt handler is layered ON TOP of the passed context rather than
// replacing it. It used to be built from context.Background(), which silently
// discarded the caller's context entirely: main hands us the same ctx the
// event monitor goroutine runs under, so anything that cancelled it could
// never stop the bot loop, and Ctrl-C was the only thing that could.
func (c *Client) Start(ctx context.Context) {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	fmt.Println("Starting Telegram bot...")
	c.bot.Start(ctx)
}

func (c *Client) handler(ctx context.Context, b *bot.Bot, update *models.Update) {
	var response string
	var err error

	if update == nil || update.Message == nil || update.Message.From == nil {
		return
	}

	c.trackUser(update.Message.From)

	chatID := update.Message.Chat.ID
	text := update.Message.Text

	isChatPrivate := update.Message.Chat.Type == models.ChatTypePrivate

	var systemNote string
	if isChatPrivate {
		username := "@" + update.Message.From.Username
		var events map[string]any // use a generic map to check existing events without importing internal/event
		c.storage.LoadOrLog("events.json", &events)

		type Pending struct {
			GroupID string
			Summary string
			Date    string
			User    string
		}
		var pendingEvents []Pending

		for chatIDStr, evRaw := range events {
			ev, ok := evRaw.(map[string]any)
			if !ok {
				continue
			}
			confsRaw, hasConfs := ev["confirmations"]
			if hasConfs {
				confs, ok := confsRaw.(map[string]any)
				if ok {
					var foundConf string
					var foundUser string
					for k, v := range confs {
						if strings.EqualFold(k, username) {
							foundUser = k
							if vStr, vok := v.(string); vok {
								foundConf = vStr
							}
							break
						}
					}

					if foundConf == "❔" {
						summary, _ := ev["summary"].(string)
						date, _ := ev["date"].(string)
						pendingEvents = append(pendingEvents, Pending{GroupID: chatIDStr, Summary: summary, Date: date, User: foundUser})
					}
				}
			}
		}

		if len(pendingEvents) == 1 {
			p := pendingEvents[0]
			systemNote = fmt.Sprintf("This user has exactly ONE pending event invite: %q on %s. If this message is an answer to that invite, work out whether it is yes, no or late and call event_manage with action='update_status' RIGHT NOW. Do not reply that you will note it down without calling the tool. The system already knows who is speaking and which event this is, so you do not pass a username or an event id.", p.Summary, p.Date)
		} else if len(pendingEvents) > 1 {
			var evStrings []string
			for _, p := range pendingEvents {
				evStrings = append(evStrings, fmt.Sprintf("%q on %s (event_group_id %s)", p.Summary, p.Date, p.GroupID))
			}
			systemNote = fmt.Sprintf("This user has MULTIPLE pending event invites: %s. If they have made clear which one they are answering, call event_manage with action='update_status' now, passing the matching event_group_id. If they say 'todos' or 'ambos', call it once per event. If it is not yet clear which one they mean, ask them before calling the tool. Do not reply that you will note it down without calling the tool.", strings.Join(evStrings, " | "))
		}
	}

	hasBotName := strings.Contains(strings.ToLower(text), strings.ToLower(c.botName))
	isReplyToBot := update.Message.ReplyToMessage != nil && c.isSelf(update.Message.ReplyToMessage.From)
	if !isChatPrivate && !hasBotName && !isReplyToBot {
		c.addToChatHistory(update)
		return
	}
	// The message being answered is recorded too, and the backlog is trimmed
	// rather than emptied.
	//
	// It used to be cleared outright on every turn the bot answered, on the
	// theory that the genai session now holds it. That session is in-memory
	// only: after a restart the bot has neither the session nor the backlog it
	// threw away, so it loses the conversation entirely -- which is exactly
	// what happened when it was restarted mid-test. Keeping a bounded tail
	// means a restart costs recent context instead of all of it.
	c.addToChatHistory(update)
	current := c.getMessageFromUpdate(update)
	c.logReplyCapture(update, current)
	prompt := googlegenai.BuildPrompt(googlegenai.Prompt{
		History:    c.getChatHistoryBefore(chatID, 1),
		SystemNote: systemNote,
		Message:    current.String(),
		ReplyingTo: current.ReplyContext(),
	})
	c.trimChatHistory(chatID)

	chatTitle := update.Message.Chat.Title
	response, err = c.ai.SendMessage(ctx, chatID, chatTitle, prompt)

	if err != nil {
		log.Printf("Failed to send message to AI: %v", err)
		response = "Sorry, something went wrong."
	}

	var replyParams *models.ReplyParameters
	if !isChatPrivate {
		replyParams = &models.ReplyParameters{
			MessageID: update.Message.ID,
		}
	}

	// Both halves of this matter, and each branch had only one of them.
	//
	// The empty check: a turn that ends in nothing but tool calls has no text
	// to send, and Telegram rejects an empty message anyway.
	//
	// The error check: this is the bot's actual reply to the user. Everywhere
	// else a failed send costs one notification, which is why .golangci.yml
	// excuses SendMessage from errcheck at large -- here it means the user
	// asked something and silently got nothing back, so it gets logged.
	if response != "" {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ReplyParameters: replyParams,
			ChatID:          chatID,
			Text:            response,
		}); err != nil {
			log.Printf("chat %d: failed to deliver the reply: %v", chatID, err)
		}
	}
}

// logReplyCapture records, for one handled message, what Telegram actually
// delivered about the reply and what was made of it.
//
// This exists because "the model does not see the reply" has three completely
// different causes that look identical from the chat: Telegram did not send
// reply_to_message at all, it sent it and this code dropped it, or the running
// binary predates reply support. One line per handled message tells them
// apart, and its mere presence in the log proves which build is deployed.
func (c *Client) logReplyCapture(update *models.Update, msg *SavedMessage) {
	log.Printf("chat %d: handling message from @%s [reply_to_message=%t quote=%t external_reply=%t -> captured_from=%q captured_len=%d]",
		update.Message.Chat.ID,
		update.Message.From.Username,
		update.Message.ReplyToMessage != nil,
		update.Message.Quote != nil,
		update.Message.ExternalReply != nil,
		msg.ReplyToUser,
		len(msg.ReplyToText),
	)
}

// isSelf reports whether a user is this bot.
//
// The ID is the real answer; the handle comparison is a fallback for the case
// where the token could not be parsed, and it is kept behind IsBot so a human
// who happens to be named like the bot is never mistaken for it.
func (c *Client) isSelf(from *models.User) bool {
	if from == nil {
		return false
	}
	if c.selfID != 0 && from.ID == c.selfID {
		return true
	}
	return from.IsBot && c.botName != "" && strings.EqualFold(from.Username, c.botName)
}

func (c *Client) getMessageFromUpdate(update *models.Update) *SavedMessage {
	replyUser, replyText := c.describeReplyTo(update.Message)
	return &SavedMessage{
		UserID:      update.Message.From.ID,
		UserName:    update.Message.From.Username,
		Text:        update.Message.Text,
		Timestamp:   time.Unix(int64(update.Message.Date), 0),
		ReplyToUser: replyUser,
		ReplyToText: replyText,
	}
}

// describeReplyTo works out who and what a message was replying to.
//
// Telegram spreads this across three fields, and any of them can be the only
// one present:
//
//   - ReplyToMessage: the ordinary case, a reply inside this chat.
//   - Quote: the fragment the user highlighted before replying. When they took
//     the trouble to highlight one line of a long message, that line is a far
//     better statement of what they mean than the whole thing, so it wins.
//   - ExternalReply: a reply to a message in ANOTHER chat (a forwarded channel
//     post, say). ReplyToMessage is nil in that case and ExternalReply carries
//     no text of its own -- Quote is the only place the words are. Dropping a
//     Quote just because ReplyToMessage was nil, which this used to do, threw
//     that case away entirely.
func (c *Client) describeReplyTo(msg *models.Message) (user, text string) {
	replyTo, quote, external := msg.ReplyToMessage, msg.Quote, msg.ExternalReply
	if replyTo == nil && quote == nil && external == nil {
		return "", ""
	}

	quoted := ""
	if quote != nil {
		quoted = strings.TrimSpace(quote.Text)
	}

	if replyTo == nil {
		// A reply to something outside this chat. Nobody local to attribute it
		// to, but the words -- if we have them -- are the whole point.
		if quoted != "" {
			return "alguem, em outra conversa", quoted
		}
		return "alguem, em outra conversa", "(uma mensagem de outro chat)"
	}

	user = c.replyAuthor(replyTo.From)

	switch {
	case quoted != "":
		text = quoted
	case replyTo.Text != "":
		text = replyTo.Text
	case replyTo.Caption != "":
		text = replyTo.Caption
	default:
		// No text at all. Naming the kind of thing still beats silence: it is
		// the difference between the model knowing there is a referent it
		// cannot read and it thinking there was no reply.
		text = describeNonTextMessage(replyTo)
	}
	return user, text
}

// replyAuthor names the author of a replied-to message the way the transcript
// names everyone else: the bare @handle, no "@".
func (c *Client) replyAuthor(from *models.User) string {
	if from == nil {
		// Anonymous admins and channel posts have no From.
		return "alguem"
	}
	if c.isSelf(from) {
		// The bot's own Telegram handle is not necessarily what it is called
		// in the prompt, and the model has to recognise its own past messages
		// as its own -- the event card most of all.
		return c.botName
	}
	if from.Username != "" {
		return from.Username
	}
	if from.FirstName != "" {
		return from.FirstName
	}
	return "alguem"
}

func describeNonTextMessage(msg *models.Message) string {
	switch {
	case len(msg.Photo) > 0:
		return "(uma foto)"
	case msg.Sticker != nil:
		return "(um sticker)"
	case msg.Voice != nil:
		return "(um audio)"
	case msg.Video != nil || msg.VideoNote != nil:
		return "(um video)"
	case msg.Document != nil:
		return "(um arquivo)"
	case msg.Poll != nil:
		return "(uma enquete)"
	default:
		return ""
	}
}

func (c *Client) addToChatHistory(update *models.Update) {
	c.lock.Lock()
	defer c.lock.Unlock()
	msg := c.getMessageFromUpdate(update)
	chatID := update.Message.Chat.ID
	if c.chatHistory[chatID] == nil {
		c.chatHistory[chatID] = make([]*SavedMessage, 0)
	}
	c.chatHistory[chatID] = append(c.chatHistory[chatID], msg)
	if len(c.chatHistory[chatID]) > c.maxHistorySize {
		c.chatHistory[chatID] = c.chatHistory[chatID][1:]
	}
	c.storage.SaveChatHistoryAsync(c.getCopyOfChatHistory())
}

// getChatHistoryBefore renders the backlog for a chat, excluding the last
// `skip` entries -- normally 1, so the message being answered does not also
// appear inside the context block.
func (c *Client) getChatHistoryBefore(chatID int64, skip int) string {
	c.lock.RLock()
	defer c.lock.RUnlock()
	messages := c.chatHistory[chatID]
	if len(messages) <= skip {
		return ""
	}
	messages = messages[:len(messages)-skip]

	historyLines := make([]string, len(messages))
	for i, msg := range messages {
		historyLines[i] = msg.String()
	}
	return strings.Join(historyLines, lineSep)
}

// trimChatHistory keeps a bounded tail of recent messages as restart insurance.
func (c *Client) trimChatHistory(chatID int64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	messages := c.chatHistory[chatID]
	if len(messages) > contextCarryOver {
		c.chatHistory[chatID] = messages[len(messages)-contextCarryOver:]
	}
	c.storage.SaveChatHistoryAsync(c.getCopyOfChatHistory())
}

func (c *Client) getCopyOfChatHistory() map[int64][]*SavedMessage {
	history := make(map[int64][]*SavedMessage, len(c.chatHistory))
	for chatID, messages := range c.chatHistory {
		history[chatID] = make([]*SavedMessage, len(messages))
		copy(history[chatID], messages)
	}
	return history
}

func (c *Client) trackUser(from *models.User) {
	if from.Username == "" {
		return
	}

	username := "@" + from.Username

	c.usersLock.RLock()
	_, known := c.users[username]
	c.usersLock.RUnlock()

	if !known {
		c.usersLock.Lock()
		c.users[username] = from.ID
		// Copy users to avoid concurrent map read/write during JSON encoding.
		usersCopy := make(map[string]int64, len(c.users))
		for k, v := range c.users {
			usersCopy[k] = v
		}
		c.usersLock.Unlock()

		c.storage.SaveToDBAsync("users.json", usersCopy)
	}
}

// Bot returns the underlying telegram bot.
func (c *Client) Bot() *bot.Bot {
	return c.bot
}
