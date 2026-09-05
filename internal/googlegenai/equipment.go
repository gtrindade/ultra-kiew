package googlegenai

import (
	"fmt"
	"strings"

	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"google.golang.org/genai"
)

const (
	// EquipmentLookupToolName is the name of the tool that looks up equipment descriptions.
	EquipmentLookupToolName = "equipment_lookup"
)

var (
	// EquipmentLookupTool is the name of the tool that provides equipment descriptions.
	EquipmentLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        EquipmentLookupToolName,
				Description: "Provide descriptions for equipment. If asked for a description of equipment, provide the full output of the function call.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"equipmentName": {
							Type:        "string",
							Description: "The equipment name",
							Example:     "What is the description for Longsword?",
						},
					},
					Required: []string{"equipmentName"},
				},
			},
		},
	}
)

func (c *Client) EquipmentLookup(args map[string]any) (string, error) {
	equipmentName, ok := args["equipmentName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: equipmentName is required")
	}

	fmt.Printf("Looking up equipment: %q\n", equipmentName)

	equipment, err := c.dbClient.GetEquipmentByName(equipmentName)
	if err != nil {
		return "", fmt.Errorf("failed to get equipment from database: %w", err)
	}

	if len(equipment) == 0 {
		return fmt.Sprintf("No equipment found with the name %q", equipmentName), nil
	}

	results := ""
	for _, item := range equipment {
		results += formatEquipmentDescription(item)
	}

	if results == "" {
		return fmt.Sprintf("No description found for the equipment %q", equipmentName), nil
	}

	return results, nil
}

func formatEquipmentDescription(item *mysql.Equipment) string {
	var desc strings.Builder

	fmt.Fprintf(&desc, "%s\n\n", item.Name)

	if item.Category != nil {
		fmt.Fprintf(&desc, "Category: %s\n", *item.Category)
	}
	if item.Subcategory != nil {
		fmt.Fprintf(&desc, "Subcategory: %s\n", *item.Subcategory)
	}
	if item.Family != nil {
		fmt.Fprintf(&desc, "Family: %s\n", *item.Family)
	}
	desc.WriteString("\n")

	if item.Cost != nil {
		fmt.Fprintf(&desc, "Cost: %s\n", *item.Cost)
	}
	if item.Weight != nil {
		fmt.Fprintf(&desc, "Weight: %s\n", *item.Weight)
	}
	if item.Type != nil {
		fmt.Fprintf(&desc, "Type: %s\n", *item.Type)
	}
	desc.WriteString("\n")

	// Combat-related stats
	if item.DmgS != nil || item.DmgM != nil || item.Critical != nil {
		desc.WriteString("Combat Stats:\n")
		if item.DmgS != nil {
			fmt.Fprintf(&desc, "Damage (Small): %s\n", *item.DmgS)
		}
		if item.DmgM != nil {
			fmt.Fprintf(&desc, "Damage (Medium): %s\n", *item.DmgM)
		}
		if item.Critical != nil {
			fmt.Fprintf(&desc, "Critical: %s\n", *item.Critical)
		}
		if item.RangeIncrement != nil {
			fmt.Fprintf(&desc, "Range Increment: %s\n", *item.RangeIncrement)
		}
		desc.WriteString("\n")
	}

	// Armor-related stats
	if item.ArmorShieldBonus != nil || item.MaximumDexBonus != nil || item.ArmorCheckPenalty != nil {
		desc.WriteString("Armor Stats:\n")
		if item.ArmorShieldBonus != nil {
			fmt.Fprintf(&desc, "Armor/Shield Bonus: %s\n", *item.ArmorShieldBonus)
		}
		if item.MaximumDexBonus != nil {
			fmt.Fprintf(&desc, "Maximum Dex Bonus: %s\n", *item.MaximumDexBonus)
		}
		if item.ArmorCheckPenalty != nil {
			fmt.Fprintf(&desc, "Armor Check Penalty: %s\n", *item.ArmorCheckPenalty)
		}
		if item.ArcaneSpellFailureChance != nil {
			fmt.Fprintf(&desc, "Arcane Spell Failure: %s\n", *item.ArcaneSpellFailureChance)
		}
		if item.Speed30 != nil {
			fmt.Fprintf(&desc, "Speed (30 ft): %s\n", *item.Speed30)
		}
		if item.Speed20 != nil {
			fmt.Fprintf(&desc, "Speed (20 ft): %s\n", *item.Speed20)
		}
		desc.WriteString("\n")
	}

	if item.FullText != nil && *item.FullText != "" {
		desc.WriteString("Description:\n")
		desc.WriteString(*item.FullText)
		desc.WriteString("\n\n")
	}

	if item.Reference != nil && *item.Reference != "" {
		fmt.Fprintf(&desc, "Source: %s", *item.Reference)
	}

	return desc.String()
}
