package googlegenai

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	filesFileName = "files.json"
)

type FileMap map[string]*genai.File

// UploadFile uploads a file to Google GenAI and returns an error if it fails.
func (c *Client) UploadFile(ctx context.Context, filePath, fileName string) (*genai.File, error) {
	fmt.Printf("Uploading file: %s\n", fileName)

	file, err := c.client.Files.UploadFromPath(
		ctx,
		filepath.Join(storage.BasePath, filePath),
		&genai.UploadFileConfig{
			MIMEType: c.GetMimeTypeFromExtension(filePath),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to upload file: %w", err)
	}

	fmt.Printf("File uploaded successfully: %s\n", fileName)

	return file, nil
}

func (c *Client) DeleteFile(ctx context.Context, fileName string) error {
	_, err := c.client.Files.Delete(ctx, fileName, nil)
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}
	return nil
}

func (c *Client) GetFile(ctx context.Context, fileID string) (*genai.File, error) {
	file, err := c.client.Files.Get(ctx, fileID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get file: %w", err)
	}
	return file, nil
}

func (c *Client) ListFiles(ctx context.Context) ([]*genai.File, error) {
	page, err := c.client.Files.List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	if len(page.Items) == 0 {
		fmt.Println("No files found.")
		return nil, nil
	}

	fmt.Printf("Found %d files:\n", len(page.Items))
	for _, file := range page.Items {
		fmt.Printf("- %s (%s)\n", file.Name, file.MIMEType)
	}

	return page.Items, nil
}

func (c *Client) GetMimeTypeFromExtension(fileName string) string {
	ext := filepath.Ext(fileName)
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream" // Default MIME type
	}
}
