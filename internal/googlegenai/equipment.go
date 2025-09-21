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
		return "", fmt.Errorf("failed to get equipment from database: %v", err)
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

	desc.WriteString(fmt.Sprintf("%s\n\n", item.Name))

	if item.Category != nil {
		desc.WriteString(fmt.Sprintf("Category: %s\n", *item.Category))
	}
	if item.Subcategory != nil {
		desc.WriteString(fmt.Sprintf("Subcategory: %s\n", *item.Subcategory))
	}
	if item.Family != nil {
		desc.WriteString(fmt.Sprintf("Family: %s\n", *item.Family))
	}
	desc.WriteString("\n")

	if item.Cost != nil {
		desc.WriteString(fmt.Sprintf("Cost: %s\n", *item.Cost))
	}
	if item.Weight != nil {
		desc.WriteString(fmt.Sprintf("Weight: %s\n", *item.Weight))
	}
	if item.Type != nil {
		desc.WriteString(fmt.Sprintf("Type: %s\n", *item.Type))
	}
	desc.WriteString("\n")

	// Combat-related stats
	if item.DmgS != nil || item.DmgM != nil || item.Critical != nil {
		desc.WriteString("Combat Stats:\n")
		if item.DmgS != nil {
			desc.WriteString(fmt.Sprintf("Damage (Small): %s\n", *item.DmgS))
		}
		if item.DmgM != nil {
			desc.WriteString(fmt.Sprintf("Damage (Medium): %s\n", *item.DmgM))
		}
		if item.Critical != nil {
			desc.WriteString(fmt.Sprintf("Critical: %s\n", *item.Critical))
		}
		if item.RangeIncrement != nil {
			desc.WriteString(fmt.Sprintf("Range Increment: %s\n", *item.RangeIncrement))
		}
		desc.WriteString("\n")
	}

	// Armor-related stats
	if item.ArmorShieldBonus != nil || item.MaximumDexBonus != nil || item.ArmorCheckPenalty != nil {
		desc.WriteString("Armor Stats:\n")
		if item.ArmorShieldBonus != nil {
			desc.WriteString(fmt.Sprintf("Armor/Shield Bonus: %s\n", *item.ArmorShieldBonus))
		}
		if item.MaximumDexBonus != nil {
			desc.WriteString(fmt.Sprintf("Maximum Dex Bonus: %s\n", *item.MaximumDexBonus))
		}
		if item.ArmorCheckPenalty != nil {
			desc.WriteString(fmt.Sprintf("Armor Check Penalty: %s\n", *item.ArmorCheckPenalty))
		}
		if item.ArcaneSpellFailureChance != nil {
			desc.WriteString(fmt.Sprintf("Arcane Spell Failure: %s\n", *item.ArcaneSpellFailureChance))
		}
		if item.Speed30 != nil {
			desc.WriteString(fmt.Sprintf("Speed (30 ft): %s\n", *item.Speed30))
		}
		if item.Speed20 != nil {
			desc.WriteString(fmt.Sprintf("Speed (20 ft): %s\n", *item.Speed20))
		}
		desc.WriteString("\n")
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
