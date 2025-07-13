package googlegenai

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/genai"
)

// SendMessageWithFile sends a message with a file to the chat and returns the response text.
func (c *Client) SendMessageWithFile(ctx context.Context, spellName string, filePath string) (string, error) {
	var err error
	chat, err := c.NewChat(ctx, 0)
	if err != nil {
		return "", err
	}

	c.lock.RLock()
	fileContent, found := c.fileCache[filePath]
	c.lock.RUnlock()
	if !found {
		fileContent, err = os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to read file %s: %w", filePath, err)
		}
		fmt.Println("File content loaded from disk:", filePath)
		c.lock.Lock()
		c.fileCache[filePath] = fileContent
		c.lock.Unlock()
	} else {
		fmt.Println("File content loaded from cache:", filePath)
	}

	parts := []genai.Part{{
		InlineData: &genai.Blob{
			MIMEType: "application/pdf",
			Data:     fileContent,
		}}, {
		Text: fmt.Sprintf("Please provide the full description of the spell %q\n", spellName),
	}}

	result, err := chat.SendMessage(ctx, parts...)
	if err != nil {
		return "", err
	}

	return result.Text(), nil
}

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
		response := &genai.Part{
			FunctionResponse: &genai.FunctionResponse{
				Name: call.Name,
				Response: map[string]any{
					"result": functionResult,
				},
			},
		}
		finalResult, err := chat.Send(ctx, response)
		if err != nil {
			fmt.Printf("Error sending function response for %s: %v\n", call.Name, err)
			continue
		}
		result = finalResult
	}

	return result.Text(), nil
}
