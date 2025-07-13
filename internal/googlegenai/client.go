package googlegenai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	// Model is the default model used for generating content.
	Model = "gemini-2.0-flash"
	// Backend is the default backend for Google GenAI.
	Backend = "gemini-api"
)

type GenericFunction func(args map[string]any) (string, error)

type ToolConfig struct {
	Function GenericFunction
	Tool     *genai.Tool
}

type Client struct {
	client        *genai.Client
	config        *genai.GenerateContentConfig
	chats         map[int]*genai.Chat
	toolConfigs   map[string]*ToolConfig
	lock          sync.RWMutex
	fileCache     map[string][]byte
	storageClient *storage.Client
	fileMap       map[string]*genai.File
}

// NewClient creates a new Google GenAI client with the provided API key and backend.
func NewClient(ctx context.Context, toolConfigs map[string]*ToolConfig, storageClient *storage.Client) (*Client, error) {
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
		chats:         make(map[int]*genai.Chat),
		client:        client,
		toolConfigs:   toolConfigs,
		fileCache:     make(map[string][]byte),
		storageClient: storageClient,
		fileMap:       make(map[string]*genai.File),
	}

	err = c.AddTools(toolConfigs)
	if err != nil {
		return nil, err
	}

	err = c.UploadFiles(ctx)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) AddTools(toolConfigs map[string]*ToolConfig) error {
	c.toolConfigs[SpellLookup] = &ToolConfig{
		Function: c.SpellLookup,
		Tool:     SpellLookupTool,
	}

	c.toolConfigs[ChatData] = &ToolConfig{
		Function: c.ChatData,
		Tool:     ChatDataTool,
	}

	tools := make([]*genai.Tool, 0, len(toolConfigs))
	for name, toolConfig := range toolConfigs {
		if toolConfig == nil || toolConfig.Tool == nil {
			return errors.New("tool configuration for " + name + " is missing or invalid")
		}
		if toolConfig.Function == nil {
			return errors.New("function for tool " + name + " is not defined")
		}
		tools = append(tools, toolConfig.Tool)
	}

	c.config = &genai.GenerateContentConfig{
		Tools: tools,
	}

	return nil
}

func (c *Client) UploadFiles(ctx context.Context) error {
	err := c.UploadFile(ctx, filepath.Join("pdfs", SpellCompendium), "application/pdf")
	if err != nil {
		return fmt.Errorf("failed to upload spell compendium: %w", err)
	}
	err = c.UploadFile(ctx, ChatDataFile, "application/json")
	if err != nil {
		return fmt.Errorf("failed to upload chat data: %w", err)
	}
	return nil
}
