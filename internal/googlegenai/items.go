package googlegenai

import (
	"fmt"
	"strings"

	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"google.golang.org/genai"
)

const (
	// ItemLookupToolName is the name of the tool that looks up item descriptions.
	ItemLookupToolName = "item_lookup"
)

var (
	// ItemLookupTool is the tool that provides item descriptions.
	ItemLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        ItemLookupToolName,
				Description: "Provide descriptions for magical items and artifacts. If asked for a description of an item, provide the full output of the function call.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"itemName": {
							Type:        "string",
							Description: "The item name",
							Example:     "What is the description for Ring of Protection?",
						},
					},
					Required: []string{"itemName"},
				},
			},
		},
	}
)

func (c *Client) ItemLookup(args map[string]any) (string, error) {
	itemName, ok := args["itemName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: itemName is required")
	}

	fmt.Printf("Looking up item: %q\n", itemName)

	items, err := c.dbClient.GetItemsByName(itemName)
	if err != nil {
		return "", fmt.Errorf("failed to get items from database: %v", err)
	}

	if len(items) == 0 {
		return fmt.Sprintf("No items found with the name %q", itemName), nil
	}

	results := ""
	for _, item := range items {
		results += formatItemDescription(item)
	}

	if results == "" {
		return fmt.Sprintf("No description found for the item %q", itemName), nil
	}

	return results, nil
}

func formatItemDescription(item *mysql.Item) string {
	var desc strings.Builder

	desc.WriteString(fmt.Sprintf("%s\n\n", item.Name))

	if item.Category != nil && *item.Category != "" {
		desc.WriteString(fmt.Sprintf("Category: %s\n", *item.Category))
	}
	if item.Subcategory != nil && *item.Subcategory != "" {
		desc.WriteString(fmt.Sprintf("Subcategory: %s\n", *item.Subcategory))
	}
	desc.WriteString("\n")

	if item.SpecialAbility != nil && *item.SpecialAbility != "" {
		desc.WriteString(fmt.Sprintf("Special Ability: %s\n", *item.SpecialAbility))
	}
	if item.Aura != nil && *item.Aura != "" {
		desc.WriteString(fmt.Sprintf("Aura: %s\n", *item.Aura))
	}
	if item.CasterLevel != nil && *item.CasterLevel != "" {
		desc.WriteString(fmt.Sprintf("Caster Level: %s\n", *item.CasterLevel))
	}
	if item.ManifesterLevel != nil && *item.ManifesterLevel != "" {
		desc.WriteString(fmt.Sprintf("Manifester Level: %s\n", *item.ManifesterLevel))
	}
	desc.WriteString("\n")

	if item.Price != nil && *item.Price != "" {
		desc.WriteString(fmt.Sprintf("Price: %s\n", *item.Price))
	}
	if item.Cost != nil && *item.Cost != "" {
		desc.WriteString(fmt.Sprintf("Cost: %s\n", *item.Cost))
	}
	if item.Weight != nil && *item.Weight != "" {
		desc.WriteString(fmt.Sprintf("Weight: %s\n", *item.Weight))
	}
	desc.WriteString("\n")

	if item.Prereq != nil && *item.Prereq != "" {
		desc.WriteString("Prerequisites:\n")
		desc.WriteString(*item.Prereq)
		desc.WriteString("\n\n")
	}

	if item.FullText != nil && *item.FullText != "" {
		desc.WriteString("Description:\n")
		desc.WriteString(*item.FullText)
		desc.WriteString("\n\n")
	}

	if item.Reference != nil && *item.Reference != "" {
		desc.WriteString(fmt.Sprintf("Source: %s", *item.Reference))
	}

	return desc.String()
}
