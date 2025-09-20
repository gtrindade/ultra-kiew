package telegram

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
)

type Client struct {
	bot     *bot.Bot
	ai      *googlegenai.Client
	botName string
}

func NewBot(config *config.Config, ai *googlegenai.Client) (*Client, error) {
	c := &Client{
		ai: ai,
	}
	opts := []bot.Option{
		bot.WithDefaultHandler(c.handler),
		bot.WithCheckInitTimeout(time.Second * 30),
	}

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN environment variable is not set")
	}

	b, err := bot.New(botToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	c.bot = b
	c.botName = config.BotName
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

	text := update.Message.Text
	switch {
	case strings.HasPrefix(text, c.botName) || update.Message.Chat.Type == models.ChatTypePrivate:
		text = strings.TrimPrefix(text, c.botName)
		response, err = c.ai.SendMessage(ctx, update.Message.Chat.ID, text)
	default:
		return
	}

	if err != nil {
		fmt.Printf("Failed to send message: %v", err)
		return
	}

	b.SendMessage(ctx, &bot.SendMessageParams{
		ReplyParameters: &models.ReplyParameters{
			MessageID: update.Message.ID,
		},
		ChatID: update.Message.Chat.ID,
		Text:   response,
	})
}
