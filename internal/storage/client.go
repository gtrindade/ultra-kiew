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

	// ChatdataPath is the path where chat data is stored.
	ChatDataPath = "chat_data"
)

// Storage provides a simple file-based storage system.
type Storage struct {
	sync.RWMutex
}

// NewStorage creates a new Storage instance with the specified base path.
func NewStorage() *Storage {
	return &Storage{}
}

// Save saves the given data to a file with the specified name.
func (s *Storage) Save(name string, data any) error {
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

// Load loads data from a file with the specified name into the provided data structure.
func (s *Storage) Load(name string, data any) error {
	s.RLock()
	defer s.RUnlock()

	filePath := filepath.Join(BasePath, name)
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(data); err != nil {
		return fmt.Errorf("failed to decode JSON data: %w", err)
	}

	return nil
}

// Delete removes the file with the specified name.
func (s *Storage) Delete(name string) error {
	s.Lock()
	defer s.Unlock()

	filePath := filepath.Join(BasePath, name)
	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to delete file %s: %w", filePath, err)
	}

	return nil
}
