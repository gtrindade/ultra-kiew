package googlegenai

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"

	"google.golang.org/genai"
)

const (
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
		Tool:     SpellLookupTool,
	}

	toolConfigs[ChatData] = &ToolConfig{
		Function: c.ChatData,
		Tool:     ChatDataTool,
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
