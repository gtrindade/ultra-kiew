package googlegenai

import (
	"context"
	"fmt"
	"path/filepath"

	"google.golang.org/genai"
)

const (
	baseFilePath = "data"
)

// UploadFile uploads a file to Google GenAI and returns an error if it fails.
func (c *Client) UploadFile(ctx context.Context, filePath, fileName, mimeType string) error {
	samplePdf, err := c.client.Files.UploadFromPath(
		ctx,
		filepath.Join(baseFilePath, filePath),
		&genai.UploadFileConfig{
			MIMEType: "application/pdf",
		},
	)
	if err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}

	c.fileMap[fileName] = samplePdf

	return nil
}

func (c *Client) DeleteFile(ctx context.Context, fileName string) error {
	_, err := c.client.Files.Delete(ctx, fileName, nil)
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}
	return nil
}

func (c *Client) GetFile(ctx context.Context, fileName string) (*genai.File, error) {
	file, err := c.client.Files.Get(ctx, fileName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get file: %w", err)
	}
	return file, nil
}
