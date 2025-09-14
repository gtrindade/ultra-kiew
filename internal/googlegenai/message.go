package googlegenai

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

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
	msg := fmt.Sprintf("%s. The chatID is %d", text, chatID)
	fmt.Printf("[%q]\n", msg)

	result, err := chat.Send(ctx, genai.NewPartFromText(msg))
	if err != nil {
		return "", err
	}
	functionCalls := result.FunctionCalls()

	for len(functionCalls) > 0 {
		var response []*genai.Part
		for _, call := range functionCalls {
			toolConfig, exists := c.toolConfigs[call.Name]
			if !exists {
				fmt.Println("Tool configuration for", call.Name, "not found")
				part := &genai.Part{
					Text: fmt.Sprintf("Error: Tool configuration for %s not found", call.Name),
				}
				response = append(response, part)
				continue
			}

			functionResult, err := toolConfig.Function(call.Args)
			if err != nil {
				fmt.Printf("Error executing function %s: %v\n", call.Name, err)
				part := &genai.Part{
					Text: fmt.Sprintf("Error executing function %s: %v", call.Name, err),
				}
				response = append(response, part)
				continue
			}
			part := &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name: call.Name,
					Response: map[string]any{
						"result": functionResult,
					},
				},
			}
			response = append(response, part)
		}

		result, err = chat.Send(ctx, response...)
		if err != nil {
			return "", err
		}
		functionCalls = result.FunctionCalls()
	}

	return result.Text(), nil
}
