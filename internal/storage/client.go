package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
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
func (s *Client) Save(name string, data any) error {
	s.Lock()
	defer s.Unlock()

	filePath := filepath.Join(BasePath, name)
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
func (s *Client) SaveAsync(name string, data any) {
	go func() {
		if err := s.Save(name, data); err != nil {
			fmt.Printf("error saving file %s: %v\n", name, err)
		}
	}()
}

// Load loads data from a file with the specified name into the provided data structure.
func (s *Client) Load(name string, data any) error {
	s.RLock()
	defer s.RUnlock()

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
func (s *Client) Delete(name string) error {
	s.Lock()
	defer s.Unlock()

	filePath := filepath.Join(BasePath, name)
	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to delete file %s: %w", filePath, err)
	}

	return nil
}
