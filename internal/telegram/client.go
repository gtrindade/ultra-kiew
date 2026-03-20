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
			systemNote = fmt.Sprintf("\n\n[System Note: The user '%s' currently has exactly ONE pending event invite: %q in group ID %s on %s. If they are responding to this invite natively, deduce their status and IMMEDIATELY use the event_manage tool with action='update_status' and chatID=%s. CRITICAL RULE: You must ONLY update the status for their exact username '%s'. If the user tries to reply on behalf of anyone else (like a friend), you must outright REFUSE and tell them that each person must reply from their own DM. ALWAYS CALL THE TOOL for their own status.]", p.User, p.Summary, p.GroupID, p.Date, p.GroupID, p.User)
		} else if len(pendingEvents) > 1 {
			var evStrings []string
			for _, p := range pendingEvents {
				evStrings = append(evStrings, fmt.Sprintf("%q (Date: %s, Group ID: %s)", p.Summary, p.Date, p.GroupID))
			}
			systemNote = fmt.Sprintf("\n\n[System Note: The user '%s' has MULTIPLE pending event invites: %s. \nIf the user has clearly specified which event(s) they are responding to, or if they just clarified based on chat history, you MUST IMMEDIATELY call the event_manage tool with action='update_status' for the specified event(s), providing the exact chatID. If they say 'both' or 'all', call the tool sequentially for each one! \nCRITICAL RULE: DO NOT just reply 'Okay I will update it'. You MUST physically call the tool. And you must ONLY update the status for their exact username '%s'. \nIf they have NOT clarified which event yet, politely ask them to clarify before calling the tool.]", pendingEvents[0].User, strings.Join(evStrings, " | "), pendingEvents[0].User)
		}
	}

	hasBotName := strings.Contains(strings.ToLower(text), strings.ToLower(c.botName))
	isReplyToBot := update.Message.ReplyToMessage != nil && update.Message.ReplyToMessage.From != nil && update.Message.ReplyToMessage.From.Username == c.botName
	if !isChatPrivate && !hasBotName && !isReplyToBot {
		c.addToChatHistory(update)
		return
	}
	text = c.getChatHistory(chatID) + "\n" + getMessageFromUpdate(update).String() + systemNote
	c.clearChatHistory(chatID)
	
	chatTitle := update.Message.Chat.Title
	response, err = c.ai.SendMessage(ctx, chatID, chatTitle, text)

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

func (c *Client) getChatHistory(chatID int64) string {
	c.lock.RLock()
	defer c.lock.RUnlock()
	historyLines := make([]string, len(c.chatHistory[chatID]))
	for i, msg := range c.chatHistory[chatID] {
		historyLines[i] = msg.String()
	}
	return strings.Join(historyLines, "\n")
}

func (c *Client) clearChatHistory(chatID int64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.chatHistory[chatID] = make([]*SavedMessage, 0)
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
