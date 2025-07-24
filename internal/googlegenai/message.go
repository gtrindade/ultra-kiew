package googlegenai

import (
	"context"
	"fmt"

	"github.com/davecgh/go-spew/spew"
	"google.golang.org/genai"
)

// SendMessageWithParts sends a message with multiple parts to the chat and returns the response text.
func (c *Client) SendMessageWithParts(ctx context.Context, chatID int, parts []*genai.Part) (string, error) {
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
func (c *Client) SendMessage(ctx context.Context, chatID int, text string) (string, error) {
	var err error
	chat, exists := c.chats[chatID]
	if !exists {
		chat, err = c.NewChat(ctx, chatID)
		if err != nil {
			return "", fmt.Errorf("failed to create new chat: %w", err)
		}
	}
	result, err := chat.Send(ctx, genai.NewPartFromText(text))

	if err != nil {
		return "", err
	}
	functionCalls := result.FunctionCalls()

	var response []*genai.Part
	for _, call := range functionCalls {
		toolConfig, exists := c.toolConfigs[call.Name]
		if !exists {
			fmt.Println("Tool configuration for", call.Name, "not found")
			continue
		}

		functionResult, err := toolConfig.Function(call.Args)
		if err != nil {
			fmt.Printf("Error executing function %s: %v\n", call.Name, err)
			continue
		}
		spew.Dump(functionResult)
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

	if len(response) == 0 {
		// No function calls, return the text response directly
		return result.Text(), nil
	}
	var finalResponse *genai.GenerateContentResponse
	finalResponse, err = chat.Send(ctx, response...)
	if err != nil {
		return "", err
	}

	return finalResponse.Text(), nil
}
