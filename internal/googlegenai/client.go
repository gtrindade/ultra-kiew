package googlegenai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	// Model is the default model used for generating content.
	Model = "gemini-2.0-flash"

	// CLEANUP indicates whether to clean up existing files before uploading new ones.
	CLEANUP = false
)

type GenericFunction func(args map[string]any) (string, error)

type ToolConfig struct {
	Function GenericFunction
	Tool     *genai.Tool
}

type Client struct {
	client      *genai.Client
	config      *genai.GenerateContentConfig
	dbClient    *mysql.Client
	chats       map[int64]*genai.Chat
	toolConfigs map[string]*ToolConfig
	lock        sync.RWMutex
	fileCache   map[string][]byte
	storage     *storage.Client
	fileMap     FileMap
	chatData    map[int64]map[string]string
}

// NewClient creates a new Google GenAI client with the provided API key and backend.
func NewClient(ctx context.Context, toolConfigs map[string]*ToolConfig, storageClient *storage.Client, dbClient *mysql.Client) (*Client, error) {
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
		chats:       make(map[int64]*genai.Chat),
		client:      client,
		toolConfigs: toolConfigs,
		dbClient:    dbClient,
		fileCache:   make(map[string][]byte),
		storage:     storageClient,
		fileMap:     make(map[string]*genai.File),
		chatData:    make(map[int64]map[string]string),
	}

	err = c.LoadDB(ctx)
	if err != nil {
		return nil, err
	}

	err = c.AddTools(toolConfigs)
	if err != nil {
		return nil, err
	}

	err = c.UploadFiles(ctx, CLEANUP)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) LoadDB(ctx context.Context) error {
	if c.storage == nil {
		return errors.New("storage client is not initialized")
	}

	err := c.storage.LoadFromDB(filesFileName, &c.fileMap)
	if err != nil {
		return fmt.Errorf("failed to load database: %w", err)
	}

	fmt.Println("Database loaded successfully")
	return nil
}

func (c *Client) AddTools(toolConfigs map[string]*ToolConfig) error {
	c.toolConfigs[SpellLookupToolName] = &ToolConfig{
		Function: c.SpellLookup,
		Tool:     SpellLookupTool,
	}

	c.toolConfigs[ChatDataToolName] = &ToolConfig{
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

func (c *Client) UploadFiles(ctx context.Context, cleanup bool) error {
	wg := sync.WaitGroup{}
	errCh := make(chan error, 2)

	files, err := c.ListFiles(ctx)
	if err != nil {
		return fmt.Errorf("failed to list files: %w", err)
	}
	if cleanup && files != nil {
		for _, file := range files {
			fmt.Printf("Deleting file: %s\n", file.Name)
			err = c.DeleteFile(ctx, file.Name)
			if err != nil {
				return fmt.Errorf("failed to delete file %s: %w", file.Name, err)
			}
		}
	}

	wg.Add(1)
	go c.UploadFileIfNeeded(ctx, storage.PDFsPath, SpellCompendium, &wg, errCh)
	wg.Wait()

	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	c.storage.SaveToDBAsync(filesFileName, c.fileMap)

	return nil
}

func (c *Client) UploadFileIfNeeded(ctx context.Context, dir, fileName string, wg *sync.WaitGroup, errCh chan error) {
	defer wg.Done()

	var needsUpload bool

	c.lock.RLock()
	file, ok := c.fileMap[fileName]
	c.lock.RUnlock()
	if !ok || file == nil {
		fmt.Printf("File %s not found in cache, needs upload\n", fileName)
		needsUpload = true
	}

	if file != nil {
		_, err := c.GetFile(ctx, file.Name)
		if err != nil {
			fmt.Printf("File %s not found in GenAI, needs upload\n", fileName)
			needsUpload = true
		}
	}

	var err error
	if needsUpload {
		nameWithoutExt := fileName[:len(fileName)-len(filepath.Ext(fileName))]
		file, err = c.UploadFile(ctx, path.Join(dir, fileName), nameWithoutExt)
		if err != nil {
			errCh <- fmt.Errorf("failed to upload file %s: %w", fileName, err)
			return
		}
		fmt.Printf("File %s uploaded successfully (%s)\n", fileName, file.Name)

		c.lock.Lock()
		c.fileMap[fileName] = file
		c.lock.Unlock()
	}
}
