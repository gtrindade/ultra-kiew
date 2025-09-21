package googlegenai

import (
	"fmt"
	"strings"

	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"google.golang.org/genai"
)

const (
	// MonsterLookupToolName is the name of the tool that looks up monster descriptions.
	MonsterLookupToolName = "monster_lookup"
)

var (
	// MonsterLookupTool is the tool that provides monster descriptions.
	MonsterLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        MonsterLookupToolName,
				Description: "Provide descriptions for monsters and creatures. If asked for a description of a monster, provide the full output of the function call.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"monsterName": {
							Type:        "string",
							Description: "The monster name",
							Example:     "What is the description for Dragon?",
						},
					},
					Required: []string{"monsterName"},
				},
			},
		},
	}
)

func (c *Client) MonsterLookup(args map[string]any) (string, error) {
	monsterName, ok := args["monsterName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: monsterName is required")
	}

	fmt.Printf("Looking up monster: %q\n", monsterName)

	monsters, err := c.dbClient.GetMonstersByName(monsterName)
	if err != nil {
		return "", fmt.Errorf("failed to get monsters from database: %v", err)
	}

	if len(monsters) == 0 {
		return fmt.Sprintf("No monsters found with the name %q", monsterName), nil
	}

	results := ""
	for _, monster := range monsters {
		results += formatMonsterDescription(monster)
	}

	if results == "" {
		return fmt.Sprintf("No description found for the monster %q", monsterName), nil
	}

	return results, nil
}

func formatMonsterDescription(monster *mysql.Monster) string {
	var desc strings.Builder

	// Header with name and type information
	desc.WriteString(fmt.Sprintf("%s", monster.Name))
	if monster.Altname != nil && *monster.Altname != "" {
		desc.WriteString(fmt.Sprintf(" (%s)", *monster.Altname))
	}
	desc.WriteString("\n\n")

	// Basic creature information
	if monster.Size != nil || monster.Type != nil || monster.Descriptor != nil {
		var typeInfo []string
		if monster.Size != nil && *monster.Size != "" {
			typeInfo = append(typeInfo, *monster.Size)
		}
		if monster.Type != nil && *monster.Type != "" {
			typeInfo = append(typeInfo, *monster.Type)
		}
		if monster.Descriptor != nil && *monster.Descriptor != "" {
			typeInfo = append(typeInfo, *monster.Descriptor)
		}
		if len(typeInfo) > 0 {
			desc.WriteString(strings.Join(typeInfo, " "))
			desc.WriteString("\n")
		}
	}

	// Combat stats
	if monster.HitDice != nil && *monster.HitDice != "" {
		desc.WriteString(fmt.Sprintf("Hit Dice: %s\n", *monster.HitDice))
	}
	if monster.Initiative != nil && *monster.Initiative != "" {
		desc.WriteString(fmt.Sprintf("Initiative: %s\n", *monster.Initiative))
	}
	if monster.Speed != nil && *monster.Speed != "" {
		desc.WriteString(fmt.Sprintf("Speed: %s\n", *monster.Speed))
	}
	if monster.ArmorClass != nil && *monster.ArmorClass != "" {
		desc.WriteString(fmt.Sprintf("Armor Class: %s\n", *monster.ArmorClass))
	}
	desc.WriteString("\n")

	// Attack information
	if monster.BaseAttack != nil && *monster.BaseAttack != "" {
		desc.WriteString(fmt.Sprintf("Base Attack/Grapple: %s", *monster.BaseAttack))
		if monster.Grapple != nil && *monster.Grapple != "" {
			desc.WriteString(fmt.Sprintf("/%s", *monster.Grapple))
		}
		desc.WriteString("\n")
	}
	if monster.Attack != nil && *monster.Attack != "" {
		desc.WriteString(fmt.Sprintf("Attack: %s\n", *monster.Attack))
	}
	if monster.FullAttack != nil && *monster.FullAttack != "" {
		desc.WriteString(fmt.Sprintf("Full Attack: %s\n", *monster.FullAttack))
	}
	if monster.Space != nil && *monster.Space != "" {
		desc.WriteString(fmt.Sprintf("Space: %s\n", *monster.Space))
	}
	if monster.Reach != nil && *monster.Reach != "" {
		desc.WriteString(fmt.Sprintf("Reach: %s\n", *monster.Reach))
	}
	desc.WriteString("\n")

	// Special abilities
	if monster.SpecialAttacks != nil && *monster.SpecialAttacks != "" {
		desc.WriteString(fmt.Sprintf("Special Attacks: %s\n", *monster.SpecialAttacks))
	}
	if monster.SpecialQualities != nil && *monster.SpecialQualities != "" {
		desc.WriteString(fmt.Sprintf("Special Qualities: %s\n", *monster.SpecialQualities))
	}
	if monster.SpecialAbilities != nil && *monster.SpecialAbilities != "" {
		desc.WriteString("Special Abilities:\n")
		desc.WriteString(*monster.SpecialAbilities)
		desc.WriteString("\n")
	}
	desc.WriteString("\n")

	// Saves and abilities
	if monster.Saves != nil && *monster.Saves != "" {
		desc.WriteString(fmt.Sprintf("Saves: %s\n", *monster.Saves))
	}
	if monster.Abilities != nil && *monster.Abilities != "" {
		desc.WriteString(fmt.Sprintf("Abilities: %s\n", *monster.Abilities))
	}
	desc.WriteString("\n")

	// Skills and feats
	if monster.Skills != nil && *monster.Skills != "" {
		desc.WriteString(fmt.Sprintf("Skills: %s\n", *monster.Skills))
	}
	if monster.Feats != nil && *monster.Feats != "" {
		desc.WriteString(fmt.Sprintf("Feats: %s\n", *monster.Feats))
	}
	if monster.BonusFeats != nil && *monster.BonusFeats != "" {
		desc.WriteString(fmt.Sprintf("Bonus Feats: %s\n", *monster.BonusFeats))
	}
	if monster.EpicFeats != nil && *monster.EpicFeats != "" {
		desc.WriteString(fmt.Sprintf("Epic Feats: %s\n", *monster.EpicFeats))
	}
	desc.WriteString("\n")

	// Environment and organization
	if monster.Environment != nil && *monster.Environment != "" {
		desc.WriteString(fmt.Sprintf("Environment: %s\n", *monster.Environment))
	}
	if monster.Organization != nil && *monster.Organization != "" {
		desc.WriteString(fmt.Sprintf("Organization: %s\n", *monster.Organization))
	}
	desc.WriteString("\n")

	// Challenge and advancement
	if monster.ChallengeRating != nil && *monster.ChallengeRating != "" {
		desc.WriteString(fmt.Sprintf("Challenge Rating: %s\n", *monster.ChallengeRating))
	}
	if monster.Treasure != nil && *monster.Treasure != "" {
		desc.WriteString(fmt.Sprintf("Treasure: %s\n", *monster.Treasure))
	}
	if monster.Alignment != nil && *monster.Alignment != "" {
		desc.WriteString(fmt.Sprintf("Alignment: %s\n", *monster.Alignment))
	}
	if monster.Advancement != nil && *monster.Advancement != "" {
		desc.WriteString(fmt.Sprintf("Advancement: %s\n", *monster.Advancement))
	}
	if monster.LevelAdjustment != nil && *monster.LevelAdjustment != "" {
		desc.WriteString(fmt.Sprintf("Level Adjustment: %s\n", *monster.LevelAdjustment))
	}
	desc.WriteString("\n")

	// Full description and reference
	if monster.FullText != nil && *monster.FullText != "" {
		desc.WriteString("Description:\n")
		desc.WriteString(*monster.FullText)
		desc.WriteString("\n\n")
	}
	if monster.Reference != nil && *monster.Reference != "" {
		desc.WriteString(fmt.Sprintf("Source: %s", *monster.Reference))
	}

	return desc.String()
}
