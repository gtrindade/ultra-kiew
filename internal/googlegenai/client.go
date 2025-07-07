package googlegenai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"sync"

	"google.golang.org/genai"
)

const (
	// SpellLookup is the name of the tool that sends a message with a file.
	SpellLookup = "spell_lookup"
	// Model is the default model used for generating content.
	Model = "gemini-2.0-flash"
	// Backend is the default backend for Google GenAI.
	Backend  = "gemini-api"
	filePath = "data"
)

type GenericFunction func(args map[string]any) (string, error)

type ToolConfig struct {
	Function GenericFunction
	Tool     *genai.Tool
}

type Client struct {
	client      *genai.Client
	config      *genai.GenerateContentConfig
	chats       map[int]*genai.Chat
	toolConfigs map[string]*ToolConfig
	lock        sync.RWMutex
	fileCache   map[string][]byte
}

// NewClient creates a new Google GenAI client with the provided API key and backend.
func NewClient(ctx context.Context, toolConfigs map[string]*ToolConfig) (*Client, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Fatal("GEMINI_API_KEY environment variable is not set")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}

	c := &Client{
		chats:       make(map[int]*genai.Chat),
		client:      client,
		toolConfigs: toolConfigs,
		fileCache:   make(map[string][]byte),
	}

	toolConfigs[SpellLookup] = &ToolConfig{
		Function: c.SpellLookup,
		Tool:     SpellLookupTool(),
	}

	tools := make([]*genai.Tool, 0, len(toolConfigs))
	for name, toolConfig := range toolConfigs {
		if toolConfig == nil || toolConfig.Tool == nil {
			return nil, errors.New("tool configuration for " + name + " is missing or invalid")
		}
		if toolConfig.Function == nil {
			return nil, errors.New("function for tool " + name + " is not defined")
		}
		tools = append(tools, toolConfig.Tool)
	}

	c.config = &genai.GenerateContentConfig{
		Tools: tools,
	}

	return c, nil
}

func (c *Client) NewChat(ctx context.Context, chatID int) (*genai.Chat, error) {
	chat, err := c.client.Chats.Create(ctx, Model, c.config, nil)
	if err != nil {
		return nil, err
	}
	if chatID != 0 {
		c.chats[chatID] = chat
	}
	return chat, nil
}

func (c *Client) GetChat(ctx context.Context, chatID int) (*genai.Chat, error) {
	chat, exists := c.chats[chatID]
	if !exists {
		return c.NewChat(ctx, chatID)
	}
	return chat, nil
}

func (c *Client) SpellLookup(args map[string]any) (string, error) {
	spellName, ok := args["spellName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: spellName is required")
	}

	fmt.Printf("Looking up spell: %q\n", spellName)

	return c.SendMessageWithFile(context.Background(), spellName, "Spell Compendium.pdf")
}

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
		fileContent, err = os.ReadFile(path.Join("data", filePath))
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

func (c *Client) SendMessage(ctx context.Context, chatID int, text string) (string, error) {
	var err error
	chat, exists := c.chats[chatID]
	if !exists {
		chat, err = c.NewChat(ctx, chatID)
		if err != nil {
			return "", fmt.Errorf("failed to create new chat: %w", err)
		}
	}
	part := genai.Part{
		Text: text,
	}
	result, err := chat.SendMessage(ctx, part)
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
		response := genai.Part{
			FunctionResponse: &genai.FunctionResponse{
				Name: call.Name,
				Response: map[string]any{
					"result": functionResult,
				},
			},
		}
		finalResult, err := chat.SendMessage(ctx, response)
		if err != nil {
			fmt.Printf("Error sending function response for %s: %v\n", call.Name, err)
			continue
		}
		result = finalResult
	}

	return result.Text(), nil
}

func SpellLookupTool() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        SpellLookup,
				Description: "Provide descriptions for spells",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"spellName": {
							Type:        "string",
							Description: "The spell name",
							Example:     "What is the description for the spell Fireball?",
						},
					},
					Required: []string{"spellName"},
				},
			},
		},
	}
}
