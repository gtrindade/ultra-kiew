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

	return nil
}

func (c *Client) LoadFromDB(name string, data any) error {
	return c.Load(filepath.Join(DBPath, name), data)
}

func (c *Client) SaveToDBAsync(name string, data any) {
	c.SaveAsync(filepath.Join(DBPath, name), data)
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
