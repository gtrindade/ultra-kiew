package tools

import (
	"context"
	"fmt"
	"path"

	"github.com/gtrindade/ultra-kiew/internal/storage"
	"google.golang.org/genai"
)

const (
	// SpellLookup is the name of the tool that sends a message with a file.
	SpellLookup = "spell_lookup"

	spellCompendium = "Spell Compendium.pdf"
)

var (
	// SpellLookup is the name of the tool that provides spell descriptions.
	SpellLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        SpellLookup,
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

func (c *Client) SpellLookup(args map[string]any) (string, error) {
	spellName, ok := args["spellName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: spellName is required")
	}

	fmt.Printf("Looking up spell: %q\n", spellName)

	filePath := path.Join(storage.BasePath, storage.PDFsPath, spellCompendium)
	return c.SendMessageWithFile(context.Background(), spellName)
}
