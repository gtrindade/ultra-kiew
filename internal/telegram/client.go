package telegram

import (
	"context"
	"fmt"
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
}

// SavedMessage represents a message saved from a user.
type SavedMessage struct {
	UserID    int64
	UserName  string
	Text      string
	Timestamp time.Time
}

func (m *SavedMessage) String() string {
	return fmt.Sprintf("[%s - %s]: `%s`", m.Timestamp.Format(time.RFC3339), m.UserName, m.Text)
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

	err = c.storage.LoadChatHistory(&c.chatHistory)
	if err != nil {
		return nil, fmt.Errorf("failed to load chat history: %w", err)
	}

	c.storage.LoadFromDB("users.json", &c.users)

	return c, nil
}

// Start starts the Telegram bot and listens for updates.
func (c *Client) Start(ctx context.Context) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
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
		c.storage.LoadFromDB("events.json", &events)

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
	isReplyToBot := update.Message.ReplyToMessage != nil && update.Message.ReplyToMessage.From != nil && strings.EqualFold(update.Message.ReplyToMessage.From.Username, c.botName)
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
	prompt := googlegenai.BuildPrompt(
		c.getChatHistoryBefore(chatID, 1),
		getMessageFromUpdate(update).String(),
		systemNote,
	)
	c.trimChatHistory(chatID)

	chatTitle := update.Message.Chat.Title
	response, err = c.ai.SendMessage(ctx, chatID, chatTitle, prompt)

	if err != nil {
		fmt.Printf("Failed to send message: %v", err)
		response = "Sorry, something went wrong."
	}

	var replyParams *models.ReplyParameters
	if !isChatPrivate {
		replyParams = &models.ReplyParameters{
			MessageID: update.Message.ID,
		}
	}

	if response != "" {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ReplyParameters: replyParams,
			ChatID:          chatID,
			Text:            response,
		})
	}
}

func getMessageFromUpdate(update *models.Update) *SavedMessage {
	return &SavedMessage{
		UserID:    update.Message.From.ID,
		UserName:  update.Message.From.Username,
		Text:      update.Message.Text,
		Timestamp: time.Unix(int64(update.Message.Date), 0),
	}
}

func (c *Client) addToChatHistory(update *models.Update) {
	c.lock.Lock()
	defer c.lock.Unlock()
	msg := getMessageFromUpdate(update)
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
