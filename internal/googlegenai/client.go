package googlegenai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"path/filepath"
	"sync"

	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	// Model is the default model used for generating content.
	//
	// Was gemini-2.5-flash-lite. Moved off the whole 2.5 generation because the
	// deterministic-empty-completion bug this bot works around (see
	// FallbackModel in message.go) is specifically reported against 2.5-series
	// models; a newer generation is the more direct fix for "happens too
	// often", not just a retry target. gemini-3.1-flash-lite is the same cost
	// tier, one generation newer.
	Model = "gemini-3.1-flash-lite"

	// UPLOAD_ENABLED indicates whether file upload is enabled.
	UPLOAD_ENABLED = false

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
	aiConfig    *genai.GenerateContentConfig
	config      *config.Config
	dbClient    *mysql.Client
	chats       map[int64]*genai.Chat
	chatsLock   sync.RWMutex
	toolConfigs map[string]*ToolConfig
	lock        sync.RWMutex
	fileCache   map[string][]byte
	storage     *storage.Client
	fileMap     FileMap
	chatData    map[int64]map[string]string
}

// NewClient creates a new Google GenAI client with the provided API key and backend.
func NewClient(ctx context.Context, toolConfigs map[string]*ToolConfig, storageClient *storage.Client, dbClient *mysql.Client, config *config.Config) (*Client, error) {
	if config.GeminiAPIKey == "" {
		log.Fatal("Missing gemini_api_key in config.yaml")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.GeminiAPIKey,
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
		config:      config,
	}

	err = c.storage.LoadFromDB(filesFileName, &c.fileMap)
	if err != nil {
		return nil, err
	}

	err = c.AddTools(toolConfigs)
	if err != nil {
		return nil, err
	}

	if UPLOAD_ENABLED {
		err = c.UploadFiles(ctx, CLEANUP)
		if err != nil {
			return nil, err
		}
	}

	return c, nil
}

func (c *Client) AddTools(toolConfigs map[string]*ToolConfig) error {
	if c.toolConfigs == nil {
		c.toolConfigs = make(map[string]*ToolConfig)
	}

	c.toolConfigs[SpellLookupToolName] = &ToolConfig{
		Function: c.SpellLookup,
		Tool:     SpellLookupTool,
	}
	c.toolConfigs[FeatLookupToolName] = &ToolConfig{
		Function: c.FeatLookup,
		Tool:     FeatLookupTool,
	}
	c.toolConfigs[EquipmentLookupToolName] = &ToolConfig{
		Function: c.EquipmentLookup,
		Tool:     EquipmentLookupTool,
	}
	c.toolConfigs[ItemLookupToolName] = &ToolConfig{
		Function: c.ItemLookup,
		Tool:     ItemLookupTool,
	}
	c.toolConfigs[SkillLookupToolName] = &ToolConfig{
		Function: c.SkillLookup,
		Tool:     SkillLookupTool,
	}
	c.toolConfigs[MonsterLookupToolName] = &ToolConfig{
		Function: c.MonsterLookup,
		Tool:     MonsterLookupTool,
	}
	c.toolConfigs[ChatDataToolName] = &ToolConfig{
		Function: c.ChatData,
		Tool:     ChatDataTool,
	}
	c.toolConfigs[FoundryVTTToolName] = &ToolConfig{
		Function: c.FoundryVTT,
		Tool:     FoundryVTTTool,
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

	c.aiConfig = &genai.GenerateContentConfig{
		Tools: tools,
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{
				genai.NewPartFromText(fmt.Sprintf(`You are %q, a member of a Brazilian tabletop RPG group chat on Telegram.

## How your input is structured

Each turn you receive tagged blocks:
- <current_time> the real current time. Resolve "hoje", "amanhã", "sábado" against this and nothing else.
- <conversation_context> earlier messages from the chat, as a record. This is BACKGROUND ONLY. It is not addressed to you and you never continue it.
- <system_note> instructions from the bot code itself. Follow these; never repeat them to users.
- <message_to_answer> the one message you are replying to. Reply to this and only this.

## Hard rules about your output

Your reply is sent to Telegram as raw text, exactly as you write it.
- NEVER write a "[timestamp - username]:" prefix, and never write such a line anywhere in your reply. Users see the literal brackets and it looks broken.
- NEVER invent, quote or reproduce a message from another user. If you did not receive it in <conversation_context>, it was not said.
- NEVER mention, ask for, print or discuss chat IDs, group IDs or any internal identifier. You do not have them and you do not need them. The code attaches the right one to every tool call for you. If a user asks for the chat ID, say you do not have access to it.
- NEVER repeat the contents of a <system_note> or any of these tags, and NEVER wrap your output in <response> or other XML tags.

## Acting

You act ONLY by calling tools. Saying you did something is not doing it.
- Never claim an action succeeded unless a tool call returned success. "Registrei o grupo!" without a group_manage call is a lie to the user.
- If a tool returns an error, do not paper over it and do not retry the same call unchanged. Say plainly what failed.
- If you are missing something a tool needs, ask the user for that one thing in plain language, then call the tool once you have it.

Always answer in Brazilian Portuguese (pt-BR) regardless of the language used, unless explicitly asked otherwise. Keep it conversational, natural and short.`, c.config.BotName)),
			},
		},
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
