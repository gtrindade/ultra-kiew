package googlegenai

import (
	"context"

	"google.golang.org/genai"
)

func (c *Client) NewChat(ctx context.Context, chatID int64) (*genai.Chat, error) {
	chat, err := c.client.Chats.Create(ctx, Model, c.aiConfig, nil)
	if err != nil {
		return nil, err
	}
	if chatID != 0 {
		c.chats[chatID] = chat
	}
	return chat, nil
}

func (c *Client) GetChat(ctx context.Context, chatID int64) (*genai.Chat, error) {
	chat, exists := c.chats[chatID]
	if !exists {
		return c.NewChat(ctx, chatID)
	}
	return chat, nil
}
