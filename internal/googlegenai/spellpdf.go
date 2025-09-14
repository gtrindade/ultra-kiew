package googlegenai

import (
	"context"
	"fmt"
	"path"

	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	// SpellPDFLookupToolName is the name of the tool that sends a message with a file.
	SpellPDFLookupToolName = "spell_lookup_pdf"

	SpellCompendium = "spell-compendium.pdf"
)

var (
	// SpellPDFLookupTool is the name of the tool that provides spell descriptions.
	SpellPDFLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        SpellLookupToolName,
				Description: "Provide descriptions for spells",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"spellName": {
							Type:        "string",
							Description: "The spell name",
							Example:     "What is the description for the spell Fireball?",
						},
					},
					Required: []string{"spellName"},
				},
			},
		},
	}
)

func (c *Client) SpellLookupOnPDF(args map[string]any) (string, error) {
	ctx := context.Background()
	spellName, ok := args["spellName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: spellName is required")
	}

	fmt.Printf("Looking up spell: %q\n", spellName)

	spellCompendium, err := c.GetFile(ctx, c.fileMap[SpellCompendium].Name)
	if err != nil {
		filePath := path.Join(storage.BasePath, storage.PDFsPath, SpellCompendium)
		spellCompendium, err = c.UploadFile(ctx, filePath, SpellCompendium)
		if err != nil {
			return "", fmt.Errorf("failed to upload spell compendium: %w", err)
		}
	}

	parts := []*genai.Part{
		genai.NewPartFromText(fmt.Sprintf("Please provide the full description of the spell %q based on the following PDF:\n", spellName)),
		genai.NewPartFromURI(spellCompendium.URI, spellCompendium.MIMEType),
	}

	result, err := c.SendMessageWithParts(ctx, 0, parts)
	if err != nil {
		return "", err
	}
	return result, nil
}
