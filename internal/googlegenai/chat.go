package googlegenai

import (
	"context"

	"google.golang.org/genai"
)

// NewChat starts a fresh session for a chat, replacing any existing one.
func (c *Client) NewChat(ctx context.Context, chatID int64) (*genai.Chat, error) {
	chat, err := c.client.Chats.Create(ctx, Model, c.aiConfig, nil)
	if err != nil {
		return nil, err
	}
	if chatID != 0 {
		c.chatsLock.Lock()
		c.chats[chatID] = chat
		c.chatsLock.Unlock()
	}
	return chat, nil
}

// GetChat returns the session for a chat, creating it on first use.
//
// The map is guarded because the event monitor goroutine reaches in here on a
// timer while the Telegram handler is serving messages -- Go's runtime aborts
// the process on a concurrent map write, which is the most likely explanation
// for the bot dying mid-conversation during testing.
func (c *Client) GetChat(ctx context.Context, chatID int64) (*genai.Chat, error) {
	c.chatsLock.RLock()
	chat, exists := c.chats[chatID]
	c.chatsLock.RUnlock()
	if exists {
		return chat, nil
	}
	return c.NewChat(ctx, chatID)
}
