package googlegenai

import (
	"fmt"
	"strings"

	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"google.golang.org/genai"
)

const (
	// SkillLookupToolName is the name of the tool that looks up skill descriptions.
	SkillLookupToolName = "skill_lookup"
)

var (
	// SkillLookupTool is the tool that provides skill descriptions.
	SkillLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        SkillLookupToolName,
				Description: "Provide descriptions for skills. If asked for a description of a skill, provide the full output of the function call.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"skillName": {
							Type:        "string",
							Description: "The skill name",
							Example:     "What is the description for Climb?",
						},
					},
					Required: []string{"skillName"},
				},
			},
		},
	}
)

func (c *Client) SkillLookup(args map[string]any) (string, error) {
	skillName, ok := args["skillName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: skillName is required")
	}

	fmt.Printf("Looking up skill: %q\n", skillName)

	skills, err := c.dbClient.GetSkillsByName(skillName)
	if err != nil {
		return "", fmt.Errorf("failed to get skills from database: %v", err)
	}

	if len(skills) == 0 {
		return fmt.Sprintf("No skills found with the name %q", skillName), nil
	}

	results := ""
	for _, skill := range skills {
		results += formatSkillDescription(skill)
	}

	if results == "" {
		return fmt.Sprintf("No description found for the skill %q", skillName), nil
	}

	return results, nil
}

func formatSkillDescription(skill *mysql.Skill) string {
	var desc strings.Builder

	desc.WriteString(fmt.Sprintf("%s\n\n", skill.Name))

	// Basic information
	if skill.KeyAbility != nil && *skill.KeyAbility != "" {
		desc.WriteString(fmt.Sprintf("Key Ability: %s\n", *skill.KeyAbility))
	}
	if skill.Trained != nil && *skill.Trained != "" {
		desc.WriteString(fmt.Sprintf("Trained Only: %s\n", *skill.Trained))
	}
	if skill.ArmorCheck != nil && *skill.ArmorCheck != "" {
		desc.WriteString(fmt.Sprintf("Armor Check Penalty: %s\n", *skill.ArmorCheck))
	}
	if skill.Psionic != nil && *skill.Psionic != "" {
		desc.WriteString(fmt.Sprintf("Psionic: %s\n", *skill.Psionic))
	}
	desc.WriteString("\n")

	// Main description
	if skill.Description != nil && *skill.Description != "" {
		desc.WriteString("Description:\n")
		desc.WriteString(*skill.Description)
		desc.WriteString("\n\n")
	}

	// Skill usage information
	if skill.SkillCheck != nil && *skill.SkillCheck != "" {
		desc.WriteString("Skill Check:\n")
		desc.WriteString(*skill.SkillCheck)
		desc.WriteString("\n\n")
	}
	if skill.Action != nil && *skill.Action != "" {
		desc.WriteString("Action:\n")
		desc.WriteString(*skill.Action)
		desc.WriteString("\n\n")
	}
	if skill.TryAgain != nil && *skill.TryAgain != "" {
		desc.WriteString("Try Again:\n")
		desc.WriteString(*skill.TryAgain)
		desc.WriteString("\n\n")
	}

	// Additional rules
	if skill.Special != nil && *skill.Special != "" {
		desc.WriteString("Special:\n")
		desc.WriteString(*skill.Special)
		desc.WriteString("\n\n")
	}
	if skill.Restriction != nil && *skill.Restriction != "" {
		desc.WriteString("Restrictions:\n")
		desc.WriteString(*skill.Restriction)
		desc.WriteString("\n\n")
	}
	if skill.Synergy != nil && *skill.Synergy != "" {
		desc.WriteString("Synergy:\n")
		desc.WriteString(*skill.Synergy)
		desc.WriteString("\n\n")
	}
	if skill.EpicUse != nil && *skill.EpicUse != "" {
		desc.WriteString("Epic Use:\n")
		desc.WriteString(*skill.EpicUse)
		desc.WriteString("\n\n")
	}
	if skill.Untrained != nil && *skill.Untrained != "" {
		desc.WriteString("Untrained Use:\n")
		desc.WriteString(*skill.Untrained)
		desc.WriteString("\n\n")
	}

	// Full text and reference
	if skill.FullText != nil && *skill.FullText != "" {
		desc.WriteString("Full Text:\n")
		desc.WriteString(*skill.FullText)
		desc.WriteString("\n\n")
	}
	if skill.Reference != nil && *skill.Reference != "" {
		desc.WriteString(fmt.Sprintf("Source: %s", *skill.Reference))
	}

	return desc.String()
}
