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

	if update == nil || update.Message == nil {
		return
	}

	chatID := update.Message.Chat.ID
	text := update.Message.Text
	hasBotName := strings.Contains(strings.ToLower(text), strings.ToLower(c.botName))
	isChatPrivate := update.Message.Chat.Type == models.ChatTypePrivate
	isReplyToBot := update.Message.ReplyToMessage != nil && update.Message.ReplyToMessage.From != nil && update.Message.ReplyToMessage.From.Username == c.botName
	if !isChatPrivate && !hasBotName && !isReplyToBot {
		c.addToChatHistory(update)
		return
	}
	text = c.getChatHistory(chatID) + "\n" + getMessageFromUpdate(update).String()
	c.clearChatHistory(chatID)
	response, err = c.ai.SendMessage(ctx, chatID, text)

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

	b.SendMessage(ctx, &bot.SendMessageParams{
		ReplyParameters: replyParams,
		ChatID:          chatID,
		Text:            response,
	})
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
