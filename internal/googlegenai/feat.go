package googlegenai

import (
	"fmt"
	"strings"

	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"google.golang.org/genai"
)

const (
	// FeatLookupToolName is the name of the tool that sends a message with a file.
	FeatLookupToolName = "feat_lookup"
)

var (
	// FeatLookupTool is the name of the tool that provides feat descriptions.
	FeatLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        FeatLookupToolName,
				Description: "Provide descriptions for feats. If asked for a description of the feat, provide the full output of the function call.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"featName": {
							Type:        "string",
							Description: "The feat name",
							Example:     "What is the description for the feat Power Attack?",
						},
					},
					Required: []string{"featName"},
				},
			},
		},
	}
)

func (c *Client) FeatLookup(args map[string]any) (string, error) {
	featName, ok := args["featName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: featName is required")
	}

	fmt.Printf("Looking up feat: %q\n", featName)

	feats, err := c.dbClient.GetFeatByName(featName)
	if err != nil {
		return "", fmt.Errorf("failed to get feat from database: %v", err)
	}

	if len(feats) == 0 {
		return fmt.Sprintf("No feat found with the name %q", featName), nil
	}

	results := ""
	for _, feat := range feats {
		results += formatFeatDescription(feat)
	}

	if results == "" {
		return fmt.Sprintf("No description found for the feat %q", featName), nil
	}

	return results, nil
}

func formatFeatDescription(feat *mysql.Feat) string {
	var desc strings.Builder

	desc.WriteString(fmt.Sprintf("%s\n\n", feat.Name))

	if feat.Categories != "" {
		desc.WriteString(fmt.Sprintf("Categories: %s\n\n", feat.Categories))
	}

	if feat.Description != "" {
		desc.WriteString("Description:\n")
		desc.WriteString(feat.Description)
		desc.WriteString("\n\n")
	}

	if feat.Benefit != "" {
		desc.WriteString("Benefit:\n")
		desc.WriteString(feat.Benefit)
		desc.WriteString("\n\n")
	}

	if feat.Normal != "" {
		desc.WriteString("Normal:\n")
		desc.WriteString(feat.Normal)
		desc.WriteString("\n\n")
	}

	if feat.Special != "" {
		desc.WriteString("Special:\n")
		desc.WriteString(feat.Special)
		desc.WriteString("\n\n")
	}

	if feat.Source != "" {
		desc.WriteString(fmt.Sprintf("Source: %s", feat.Source))
	}

	return desc.String()
}
