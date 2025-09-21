package googlegenai

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

const MaxFunctionResponseLength = 10000

// SendMessageWithParts sends a message with multiple parts to the chat and returns the response text.
func (c *Client) SendMessageWithParts(ctx context.Context, chatID int64, parts []*genai.Part) (string, error) {
	var err error
	chat, exists := c.chats[chatID]
	if !exists {
		chat, err = c.NewChat(ctx, chatID)
		if err != nil {
			return "", fmt.Errorf("failed to create new chat: %w", err)
		}
	}
	result, err := chat.Send(ctx, parts...)
	if err != nil {
		return "", err
	}

	return result.Text(), nil
}

// SendMessage sends a text message to the chat and handles any function calls that may be triggered.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (string, error) {
	var err error
	chat, exists := c.chats[chatID]
	if !exists {
		chat, err = c.NewChat(ctx, chatID)
		if err != nil {
			return "", fmt.Errorf("failed to create new chat: %w", err)
		}
	}

	err = c.checkChatHistory(chatID)
	if err != nil {
		return "", err
	}

	msg := fmt.Sprintf("%s. The chatID is %d", text, chatID)
	parts := []*genai.Part{genai.NewPartFromText(msg)}
	result, err := chat.Send(ctx, parts...)
	if err != nil {
		return "", fmt.Errorf("failed to send message: %w", err)
	}

	functionCalls := result.FunctionCalls()
	for len(functionCalls) > 0 {
		var response []*genai.Part
		for _, call := range functionCalls {
			toolConfig, exists := c.toolConfigs[call.Name]
			if !exists {
				part := genai.NewPartFromText(fmt.Sprintf("Error: Tool configuration for %s not found", call.Name))
				response = append(response, part)
				continue
			}

			functionResult, err := toolConfig.Function(call.Args)
			if err != nil {
				part := genai.NewPartFromText(fmt.Sprintf("Error executing function %s: %v", call.Name, err))
				response = append(response, part)
				continue
			}

			if len(functionResult) > MaxFunctionResponseLength {
				fmt.Printf("Function result too long (%d characters), truncating\n", len(functionResult))
				functionResult = functionResult[:MaxFunctionResponseLength] + "...(truncated)"
				response = append(response, genai.NewPartFromText("Note: The function result was too long and has been truncated."))
			}

			part := genai.NewPartFromFunctionResponse(call.Name, map[string]any{
				"result": functionResult,
			})
			response = append(response, part)
		}

		if len(response) > 0 {
			result, err = chat.Send(ctx, response...)
			if err != nil {
				return "", fmt.Errorf("failed to send function response: %w", err)
			}
			functionCalls = result.FunctionCalls()
		} else {
			break
		}
	}

	responseText := result.Text()
	if responseText == "" {
		err = c.checkChatHistory(chatID)
		if err != nil {
			return err.Error(), nil
		}
		return "I apologize, but I couldn't generate a response. Please try again.", nil
	}

	return responseText, nil
}

func (c *Client) checkChatHistory(chatID int64) error {
	chat, exists := c.chats[chatID]
	if !exists {
		return fmt.Errorf("chat with ID %d does not exist", chatID)
	}

	history := chat.History(false)
	for _, content := range history {
		if content != nil && len(content.Parts) > 0 {
			continue
		}

		newChat, err := c.NewChat(context.Background(), chatID)
		if err != nil {
			return fmt.Errorf("failed to recover chat session: %w", err)
		}
		c.chats[chatID] = newChat
		return fmt.Errorf("Due to a known issue, I failed to generate a response and broke the chat history. I had to start a new session. Please try again.\n\nThe issue: https://discuss.ai.google.dev/t/empty-response-text-from-gemini-2-5-pro-despite-no-safety-and-max-tokens-issues/98010/23")
	}

	return nil
}
