package storage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

var (
	// BasePath is the default base path for storage.
	BasePath = "data"

	// PDFsPath is the path where PDF files are stored.
	PDFsPath = "pdfs"

	// DBPath is the path where database files are stored.
	DBPath = "db"

	// ChatHistoryFileName is the filename for chat history.
	ChatHistoryFileName = "chat_history.json"
)

// Client provides a simple file-based storage system.
type Client struct {
	sync.RWMutex
}

// NewClient creates a new Client instance with the specified base path.
func NewClient() *Client {
	return &Client{}
}

// Save saves the given data to a file with the specified name.
func (c *Client) Save(name string, data any) error {
	c.Lock()
	defer c.Unlock()

	filePath := filepath.Join(BasePath, name)

	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", filePath, err)
	}

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", filePath, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode data to JSON: %w", err)
	}

	fmt.Printf("Data saved to file %s successfully\n", filePath)

	return nil
}
func (c *Client) SaveAsync(name string, data any) {
	go func() {
		if err := c.Save(name, data); err != nil {
			fmt.Printf("error saving file %s: %v\n", name, err)
		}
	}()
}

// Load loads data from a file with the specified name into the provided data structure.
func (c *Client) Load(name string, data any) error {
	c.RLock()
	defer c.RUnlock()

	filePath := filepath.Join(BasePath, name)
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("file %s does not exist when trying to load it\n", filePath)
			return nil
		}
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(data); err != nil {
		return fmt.Errorf("failed to decode JSON data: %w", err)
	}

	fmt.Printf("Data loaded from file %s successfully\n", filePath)
	return nil
}

// LoadFromDB loads data from a file in the predefined database path.
func (c *Client) LoadFromDB(name string, data any) error {
	return c.Load(filepath.Join(DBPath, name), data)
}

// LoadForUpdate reads a state file the caller intends to write back, and
// returns an error the caller must abort on.
//
// This distinction is the whole point, and ignoring it is a data-loss bug
// rather than a nit. Load leaves data at its zero value in two very different
// situations: the file does not exist yet (normal, every first run) and the
// file exists but will not decode (a write torn by a crash, a half-synced
// disk). A caller that drops the error cannot tell those apart -- it carries
// on with an empty map, and the MustSave at the end of its work writes that
// emptiness straight back over the file. A recoverable corrupt events.json
// becomes a permanently empty one, and every scheduled session is gone.
//
// A missing file still loads cleanly as empty, so first runs are unaffected;
// only a genuine read or decode failure stops the operation.
func (c *Client) LoadForUpdate(name string, data any) error {
	if err := c.LoadFromDB(name, data); err != nil {
		return fmt.Errorf("could not read %s: %w -- refusing to continue, because saving now would overwrite it with nothing", name, err)
	}
	return nil
}

// LoadOrLog reads a state file the caller only consults and never writes back
// -- a lookup table like users.json, or a cross-check against another
// package's file. Losing it degrades the answer (a DM that cannot be
// addressed, a hint the model does not get) but cannot destroy anything, so
// the failure is recorded and the caller carries on.
func (c *Client) LoadOrLog(name string, data any) {
	if err := c.LoadFromDB(name, data); err != nil {
		log.Printf("storage: could not read %s, continuing without it: %v", name, err)
	}
}

// SaveToDB saves data to a file in the predefined database path and does not
// return until it is written.
//
// Use this for anything the bot is about to claim it did. The async variant
// returns before the write lands, so a tool could answer "event created" and
// then lose it to a crash, and two async saves of the same file have no
// ordering between them. Chat history can afford that; events and groups
// cannot.
func (c *Client) SaveToDB(name string, data any) error {
	return c.Save(filepath.Join(DBPath, name), data)
}

// SaveToDBAsync saves data to a file in the predefined database path asynchronously.
func (c *Client) SaveToDBAsync(name string, data any) {
	c.SaveAsync(filepath.Join(DBPath, name), data)
}

// SaveChatHistoryAsync saves chat history to the predefined chat history file asynchronously.
func (c *Client) SaveChatHistoryAsync(data any) {
	c.SaveToDBAsync(ChatHistoryFileName, data)
}

// LoadChatHistory loads chat history from the predefined chat history file.
func (c *Client) LoadChatHistory(data any) error {
	return c.LoadFromDB(ChatHistoryFileName, data)
}

// Delete removes the file with the specified name.
func (c *Client) Delete(name string) error {
	c.Lock()
	defer c.Unlock()

	filePath := filepath.Join(BasePath, name)
	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to delete file %s: %w", filePath, err)
	}

	return nil
}

// MustSave writes data to the database path, logging rather than returning an
// error. Callers that have already decided to act want the write attempted and
// the failure recorded; there is nothing useful to tell the user mid-tool-call
// beyond what the next read will show anyway.
func (c *Client) MustSave(name string, data any) {
	if err := c.SaveToDB(name, data); err != nil {
		fmt.Printf("CRITICAL: failed to persist %s: %v\n", name, err)
	}
}
